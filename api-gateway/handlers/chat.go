package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// chatStore defines the data access methods needed by ChatHandler.
type chatStore interface {
	CreateConversation(ctx context.Context, arg db.CreateConversationParams) (db.Conversation, error)
	AddConversationMember(ctx context.Context, arg db.AddConversationMemberParams) error
	FindDirectConversation(ctx context.Context, arg db.FindDirectConversationParams) (db.Conversation, error)
	GetConversation(ctx context.Context, id uuid.UUID) (db.Conversation, error)
	IsConversationMember(ctx context.Context, arg db.IsConversationMemberParams) (bool, error)
	ListConversationMembers(ctx context.Context, conversationID uuid.UUID) ([]db.ListConversationMembersRow, error)
	ListUserConversations(ctx context.Context, userID uuid.UUID) ([]db.ListUserConversationsRow, error)
	InsertMessage(ctx context.Context, arg db.InsertMessageParams) (db.Message, error)
	ListMessages(ctx context.Context, arg db.ListMessagesParams) ([]db.ListMessagesRow, error)
	UpdateLastReadAt(ctx context.Context, arg db.UpdateLastReadAtParams) error
	TouchConversationUpdatedAt(ctx context.Context, id uuid.UUID) error
	GetUserByZitadelID(ctx context.Context, zitadelID string) (db.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
}

// r2Presigner defines the R2 presigning interface for chat media uploads.
type r2Presigner interface {
	PresignPut(ctx context.Context, key string, contentType string, expiry time.Duration) (string, error)
}

// ChatHandler manages chat conversations, messages, and WebSocket interactions.
type ChatHandler struct {
	Queries chatStore
	R2      r2Presigner
	Hub     *ChatHub
	Cfg     *shared.APIConfig
}

// NewChatHandler constructs a ChatHandler instance.
func NewChatHandler(queries *db.Queries, r2 *shared.R2Client, hub *ChatHub, cfg *shared.APIConfig) *ChatHandler {
	if queries == nil {
		panic("NewChatHandler: queries is nil")
	}
	if hub == nil {
		panic("NewChatHandler: hub is nil")
	}
	return &ChatHandler{
		Queries: queries,
		R2:      r2,
		Hub:     hub,
		Cfg:     cfg,
	}
}

const (
	maxMessageContentLen  = 4000
	maxMediaURLLen        = 2048
	maxMediaMetadataBytes = 4096
	maxGroupNameLen       = 100
	maxGroupMembers       = 100
)

// isMember reports whether userID belongs to the conversation. Any lookup
// error is treated as "not a member" so callers fail closed.
func (h *ChatHandler) isMember(ctx context.Context, convID, userID uuid.UUID) bool {
	ok, err := h.Queries.IsConversationMember(ctx, db.IsConversationMemberParams{
		ConversationID: convID,
		UserID:         userID,
	})
	return err == nil && ok
}

// requireActiveUsers returns ErrNotFound unless every id is an existing,
// active (non-deleted) user.
func (h *ChatHandler) requireActiveUsers(ctx context.Context, ids ...uuid.UUID) error {
	for _, id := range ids {
		u, err := h.Queries.GetUserByID(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "user not found")
		}
		if err != nil {
			return shared.Wrap(shared.ErrInternal, "failed to look up user")
		}
		if !u.IsActive || u.DeletedAt.Valid {
			return shared.Wrap(shared.ErrNotFound, "user not found")
		}
	}
	return nil
}

// ConversationResponse represents the wire format of a chat conversation.
type ConversationResponse struct {
	ID          uuid.UUID            `json:"id"`
	Type        string               `json:"type"`
	Name        *string              `json:"name,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Members     []MemberResponse     `json:"members,omitempty"`
	LastMessage *LastMessageResponse `json:"last_message,omitempty"`
	Unread      bool                 `json:"unread"`
}

type MemberResponse struct {
	UserID      uuid.UUID `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name,omitempty"`
	AvatarURL   *string   `json:"avatar_url,omitempty"`
	Role        string    `json:"role"`
}

type LastMessageResponse struct {
	ID        uuid.UUID `json:"id"`
	SenderID  uuid.UUID `json:"sender_id"`
	Type      string    `json:"type"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateConversationRequest struct {
	TargetUserID *uuid.UUID  `json:"target_user_id,omitempty"`
	Name         *string     `json:"name,omitempty"`
	MemberIDs    []uuid.UUID `json:"member_ids,omitempty"`
}

// CreateConversation initiates a direct or group conversation.
func (h *ChatHandler) CreateConversation(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	var req CreateConversationRequest
	if err := c.Bind(&req); err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid json body")
	}

	ctx := c.Request().Context()

	// Direct Chat Flow
	if req.TargetUserID != nil {
		targetID := *req.TargetUserID
		if targetID == user.ID {
			return shared.Wrap(shared.ErrValidation, "cannot start chat with yourself")
		}
		if err := h.requireActiveUsers(ctx, targetID); err != nil {
			return err
		}

		// Check if direct conversation already exists
		conv, err := h.Queries.FindDirectConversation(ctx, db.FindDirectConversationParams{
			UserID:   user.ID,
			UserID_2: targetID,
		})
		if err == nil {
			// Found existing direct conversation
			members, _ := h.Queries.ListConversationMembers(ctx, conv.ID)
			return c.JSON(http.StatusOK, h.formatConversation(conv, members, nil, false))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrInternal, "failed to check existing conversation")
		}

		// Create new direct conversation
		newConv, err := h.Queries.CreateConversation(ctx, db.CreateConversationParams{
			Type: "direct",
			Name: sql.NullString{},
		})
		if err != nil {
			return shared.Wrap(shared.ErrInternal, "failed to create conversation")
		}

		for _, memberID := range []uuid.UUID{user.ID, targetID} {
			if err := h.Queries.AddConversationMember(ctx, db.AddConversationMemberParams{
				ConversationID: newConv.ID,
				UserID:         memberID,
				Role:           "member",
			}); err != nil {
				return shared.Wrap(shared.ErrInternal, "failed to add conversation member")
			}
		}

		members, _ := h.Queries.ListConversationMembers(ctx, newConv.ID)
		return c.JSON(http.StatusCreated, h.formatConversation(newConv, members, nil, false))
	}

	// Group Chat Flow
	if len(req.MemberIDs) > 0 {
		var nameVal sql.NullString
		if req.Name != nil && *req.Name != "" {
			if utf8.RuneCountInString(*req.Name) > maxGroupNameLen {
				return shared.Wrap(shared.ErrValidation, "group name is too long")
			}
			nameVal = sql.NullString{String: *req.Name, Valid: true}
		}

		// De-duplicate, drop the creator, and make sure every member exists
		// before anything is written.
		seen := map[uuid.UUID]bool{user.ID: true}
		memberIDs := make([]uuid.UUID, 0, len(req.MemberIDs))
		for _, mID := range req.MemberIDs {
			if !seen[mID] {
				seen[mID] = true
				memberIDs = append(memberIDs, mID)
			}
		}
		if len(memberIDs) == 0 {
			return shared.Wrap(shared.ErrValidation, "at least one other member is required")
		}
		if len(memberIDs) > maxGroupMembers {
			return shared.Wrap(shared.ErrValidation, "too many group members")
		}
		if err := h.requireActiveUsers(ctx, memberIDs...); err != nil {
			return err
		}

		newConv, err := h.Queries.CreateConversation(ctx, db.CreateConversationParams{
			Type: "group",
			Name: nameVal,
		})
		if err != nil {
			return shared.Wrap(shared.ErrInternal, "failed to create group conversation")
		}

		if err := h.Queries.AddConversationMember(ctx, db.AddConversationMemberParams{
			ConversationID: newConv.ID,
			UserID:         user.ID,
			Role:           "admin",
		}); err != nil {
			return shared.Wrap(shared.ErrInternal, "failed to add conversation member")
		}

		for _, mID := range memberIDs {
			if err := h.Queries.AddConversationMember(ctx, db.AddConversationMemberParams{
				ConversationID: newConv.ID,
				UserID:         mID,
				Role:           "member",
			}); err != nil {
				return shared.Wrap(shared.ErrInternal, "failed to add conversation member")
			}
		}

		members, _ := h.Queries.ListConversationMembers(ctx, newConv.ID)
		return c.JSON(http.StatusCreated, h.formatConversation(newConv, members, nil, false))
	}

	return shared.Wrap(shared.ErrValidation, "target_user_id or member_ids is required")
}

// ListConversations returns all conversations for the authenticated user.
func (h *ChatHandler) ListConversations(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	ctx := c.Request().Context()
	rows, err := h.Queries.ListUserConversations(ctx, user.ID)
	if err != nil {
		return shared.Wrap(shared.ErrInternal, "failed to list conversations")
	}

	var res []ConversationResponse
	for _, r := range rows {
		membersRow, _ := h.Queries.ListConversationMembers(ctx, r.ID)

		var members []MemberResponse
		for _, m := range membersRow {
			members = append(members, MemberResponse{
				UserID:      m.UserID,
				Username:    m.Username,
				DisplayName: stringPtr(m.DisplayName),
				AvatarURL:   stringPtr(m.AvatarUrl),
				Role:        m.Role,
			})
		}

		var lastMsg *LastMessageResponse
		unread := false
		if r.LastMessageID != uuid.Nil {
			lastMsg = &LastMessageResponse{
				ID:        r.LastMessageID,
				SenderID:  r.LastMessageSenderID,
				Type:      r.LastMessageType,
				Content:   r.LastMessageContent,
				CreatedAt: r.LastMessageCreatedAt,
			}
			if !r.LastMessageCreatedAt.IsZero() && r.LastMessageCreatedAt.After(r.LastReadAt) && r.LastMessageSenderID != user.ID {
				unread = true
			}
		}

		var namePtr *string
		if r.Name.Valid {
			namePtr = &r.Name.String
		}

		res = append(res, ConversationResponse{
			ID:          r.ID,
			Type:        r.Type,
			Name:        namePtr,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
			Members:     members,
			LastMessage: lastMsg,
			Unread:      unread,
		})
	}

	if res == nil {
		res = []ConversationResponse{}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"conversations": res,
	})
}

// SendMessageRequest payload for REST message send.
type SendMessageRequest struct {
	MessageType   string          `json:"message_type"`
	Content       string          `json:"content"`
	MediaURL      string          `json:"media_url,omitempty"`
	MediaMetadata json.RawMessage `json:"media_metadata,omitempty"`
}

// SendMessage sends a text, photo, video, sticker, or GIF message.
func (h *ChatHandler) SendMessage(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	convID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid conversation uuid")
	}

	var req SendMessageRequest
	if err := c.Bind(&req); err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid json body")
	}

	ctx := c.Request().Context()
	msg, err := h.SendMessageInternal(ctx, user.ID, convID, req.MessageType, req.Content, req.MediaURL, req.MediaMetadata)
	if err != nil {
		return err
	}

	// Broadcast message via WebSocket Hub
	members, err := h.Queries.ListConversationMembers(ctx, convID)
	if err == nil {
		var recipientIDs []uuid.UUID
		for _, m := range members {
			recipientIDs = append(recipientIDs, m.UserID)
		}
		out, _ := json.Marshal(map[string]any{
			"type":    "message.new",
			"message": msg,
		})
		h.Hub.BroadcastToUsers(recipientIDs, out)
	}

	return c.JSON(http.StatusCreated, msg)
}

// SendMessageInternal helper for creating and saving a message in DB.
func (h *ChatHandler) SendMessageInternal(ctx context.Context, senderID uuid.UUID, convID uuid.UUID, msgType string, content string, mediaURL string, mediaMeta json.RawMessage) (map[string]any, error) {
	// Verify user membership
	isMember, err := h.Queries.IsConversationMember(ctx, db.IsConversationMemberParams{
		ConversationID: convID,
		UserID:         senderID,
	})
	if err != nil || !isMember {
		return nil, shared.Wrap(shared.ErrNotFound, "conversation not found")
	}

	if msgType == "" {
		msgType = "text"
	}
	validTypes := map[string]bool{"text": true, "photo": true, "video": true, "sticker": true, "gif": true}
	if !validTypes[msgType] {
		return nil, shared.Wrap(shared.ErrValidation, "unsupported message type")
	}

	if utf8.RuneCountInString(content) > maxMessageContentLen {
		return nil, shared.Wrap(shared.ErrValidation, "content is too long")
	}
	if len(mediaURL) > maxMediaURLLen {
		return nil, shared.Wrap(shared.ErrValidation, "media_url is too long")
	}
	switch msgType {
	case "text":
		if strings.TrimSpace(content) == "" {
			return nil, shared.Wrap(shared.ErrValidation, "content is required for text messages")
		}
	case "photo", "video":
		if mediaURL == "" {
			return nil, shared.Wrap(shared.ErrValidation, "media_url is required for photo and video messages")
		}
	default: // sticker, gif
		if content == "" && mediaURL == "" {
			return nil, shared.Wrap(shared.ErrValidation, "content or media_url is required")
		}
	}
	if len(mediaMeta) > 0 && (len(mediaMeta) > maxMediaMetadataBytes || !json.Valid(mediaMeta)) {
		return nil, shared.Wrap(shared.ErrValidation, "media_metadata must be valid json of at most 4KB")
	}

	var contentVal sql.NullString
	if content != "" {
		contentVal = sql.NullString{String: content, Valid: true}
	}
	var mediaURLVal sql.NullString
	if mediaURL != "" {
		mediaURLVal = sql.NullString{String: mediaURL, Valid: true}
	}

	metaBytes := mediaMeta
	if len(metaBytes) == 0 {
		metaBytes = json.RawMessage("{}")
	}

	msg, err := h.Queries.InsertMessage(ctx, db.InsertMessageParams{
		ConversationID: convID,
		SenderID:       senderID,
		MessageType:    msgType,
		Content:        contentVal,
		MediaUrl:       mediaURLVal,
		MediaMetadata:  metaBytes,
	})
	if err != nil {
		return nil, shared.Wrap(shared.ErrInternal, "failed to insert message")
	}

	_ = h.Queries.TouchConversationUpdatedAt(ctx, convID)

	var cStr, mStr *string
	if msg.Content.Valid {
		cStr = &msg.Content.String
	}
	if msg.MediaUrl.Valid {
		mStr = &msg.MediaUrl.String
	}

	return map[string]any{
		"id":              msg.ID,
		"conversation_id": msg.ConversationID,
		"sender_id":       msg.SenderID,
		"message_type":    msg.MessageType,
		"content":         cStr,
		"media_url":       mStr,
		"media_metadata":  json.RawMessage(msg.MediaMetadata),
		"created_at":      msg.CreatedAt,
	}, nil
}

// ListMessages retrieves cursor-paginated messages for a conversation.
func (h *ChatHandler) ListMessages(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	convID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid conversation uuid")
	}

	ctx := c.Request().Context()
	isMember, err := h.Queries.IsConversationMember(ctx, db.IsConversationMemberParams{
		ConversationID: convID,
		UserID:         user.ID,
	})
	if err != nil || !isMember {
		return shared.Wrap(shared.ErrNotFound, "conversation not found")
	}

	var cursorTime sql.NullTime
	if cursorParam := c.QueryParam("cursor"); cursorParam != "" {
		if t, err := time.Parse(time.RFC3339, cursorParam); err == nil {
			cursorTime = sql.NullTime{Time: t, Valid: true}
		}
	}

	limit := int32(30)

	rows, err := h.Queries.ListMessages(ctx, db.ListMessagesParams{
		ConversationID: convID,
		Limit:          limit,
		Cursor:         cursorTime,
	})
	if err != nil {
		return shared.Wrap(shared.ErrInternal, "failed to list messages")
	}

	var msgs []map[string]any
	for _, r := range rows {
		var cStr, mStr *string
		if r.Content.Valid {
			cStr = &r.Content.String
		}
		if r.MediaUrl.Valid {
			mStr = &r.MediaUrl.String
		}

		msgs = append(msgs, map[string]any{
			"id":                  r.ID,
			"conversation_id":     r.ConversationID,
			"sender_id":           r.SenderID,
			"sender_username":     r.SenderUsername,
			"sender_display_name": stringPtr(r.SenderDisplayName),
			"sender_avatar_url":   stringPtr(r.SenderAvatarUrl),
			"message_type":        r.MessageType,
			"content":             cStr,
			"media_url":           mStr,
			"media_metadata":      json.RawMessage(r.MediaMetadata),
			"created_at":          r.CreatedAt,
		})
	}

	if msgs == nil {
		msgs = []map[string]any{}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"messages": msgs,
	})
}

// MarkRead updates the user's last_read_at timestamp.
func (h *ChatHandler) MarkRead(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	convID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid conversation uuid")
	}

	ctx := c.Request().Context()
	err = h.Queries.UpdateLastReadAt(ctx, db.UpdateLastReadAtParams{
		ConversationID: convID,
		UserID:         user.ID,
	})
	if err != nil {
		return shared.Wrap(shared.ErrInternal, "failed to update read status")
	}

	return c.JSON(http.StatusOK, map[string]any{"status": "ok"})
}

type UploadIntentRequest struct {
	FileType    string `json:"file_type"`    // photo or video
	ContentType string `json:"content_type"` // e.g., image/png, video/mp4
}

// UploadIntent generates a presigned R2 upload URL for chat attachments.
func (h *ChatHandler) UploadIntent(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.Wrap(shared.ErrUnauthorized, "missing authentication")
	}

	var req UploadIntentRequest
	if err := c.Bind(&req); err != nil {
		return shared.Wrap(shared.ErrValidation, "invalid request body")
	}

	if req.FileType != "photo" && req.FileType != "video" {
		return shared.Wrap(shared.ErrValidation, "file_type must be photo or video")
	}
	if req.ContentType == "" {
		return shared.Wrap(shared.ErrValidation, "content_type is required")
	}

	ext := "bin"
	if parts := strings.Split(req.ContentType, "/"); len(parts) == 2 {
		ext = parts[1]
	}

	key := fmt.Sprintf("chat/%s/%s.%s", user.ID, uuid.New().String(), ext)
	expiry := 15 * time.Minute
	if h.Cfg != nil && h.Cfg.PresignUploadExpiry > 0 {
		expiry = h.Cfg.PresignUploadExpiry
	}

	if h.R2 == nil {
		return shared.Wrap(shared.ErrInternal, "R2 client not configured")
	}

	presignedURL, err := h.R2.PresignPut(c.Request().Context(), key, req.ContentType, expiry)
	if err != nil {
		return shared.Wrap(shared.ErrInternal, "failed to generate upload intent")
	}

	return c.JSON(http.StatusOK, map[string]any{
		"upload_url": presignedURL,
		"media_key":  key,
		"expires_in": int(expiry.Seconds()),
	})
}

func (h *ChatHandler) formatConversation(conv db.Conversation, membersRow []db.ListConversationMembersRow, lastMsg *LastMessageResponse, unread bool) ConversationResponse {
	var members []MemberResponse
	for _, m := range membersRow {
		members = append(members, MemberResponse{
			UserID:      m.UserID,
			Username:    m.Username,
			DisplayName: stringPtr(m.DisplayName),
			AvatarURL:   stringPtr(m.AvatarUrl),
			Role:        m.Role,
		})
	}

	var namePtr *string
	if conv.Name.Valid {
		namePtr = &conv.Name.String
	}

	return ConversationResponse{
		ID:          conv.ID,
		Type:        conv.Type,
		Name:        namePtr,
		CreatedAt:   conv.CreatedAt,
		UpdatedAt:   conv.UpdatedAt,
		Members:     members,
		LastMessage: lastMsg,
		Unread:      unread,
	}
}

func stringPtr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}
