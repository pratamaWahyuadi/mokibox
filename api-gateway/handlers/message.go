package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const maxDMMessageRunes = 2000

type messageStore interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
	GetOrCreateDMConversation(ctx context.Context, arg db.GetOrCreateDMConversationParams) (db.DmConversation, error)
	SendDMMessage(ctx context.Context, arg db.SendDMMessageParams) (db.SendDMMessageRow, error)
	ListDMConversations(ctx context.Context, arg db.ListDMConversationsParams) ([]db.ListDMConversationsRow, error)
	GetDMConversationForUser(ctx context.Context, arg db.GetDMConversationForUserParams) (db.DmConversation, error)
	ListDMMessages(ctx context.Context, arg db.ListDMMessagesParams) ([]db.DmMessage, error)
	ListDMMessagesAfter(ctx context.Context, arg db.ListDMMessagesAfterParams) ([]db.DmMessage, error)
	MarkDMMessagesRead(ctx context.Context, arg db.MarkDMMessagesReadParams) (int64, error)
	AcceptDMRequest(ctx context.Context, arg db.AcceptDMRequestParams) (db.AcceptDMRequestRow, error)
	DeclineDMRequest(ctx context.Context, arg db.DeclineDMRequestParams) (db.DeclineDMRequestRow, error)
}

type MessageHandler struct {
	Queries messageStore
}

func NewMessageHandler(queries *db.Queries) *MessageHandler {
	return &MessageHandler{Queries: queries}
}

func newMessageHandlerForTest(store messageStore) *MessageHandler {
	return &MessageHandler{Queries: store}
}

type sendMessageRequest struct {
	RecipientID string `json:"recipient_id"`
	Content     string `json:"content" validate:"required,max=2000"`
}

type dmMessageObject struct {
	ID             uuid.UUID `json:"id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	SenderID       uuid.UUID `json:"sender_id"`
	Content        string    `json:"content"`
	CreatedAt      string    `json:"created_at"`
	ReadAt         *string   `json:"read_at"`
}

type sendMessageResponse struct {
	ConversationID uuid.UUID       `json:"conversation_id"`
	RequestStatus  string          `json:"request_status"`
	Message        dmMessageObject `json:"message"`
}

type dmConversationObject struct {
	ID               uuid.UUID `json:"id"`
	OtherUserID      uuid.UUID `json:"other_user_id"`
	OtherUsername    string    `json:"other_username"`
	OtherDisplayName *string   `json:"other_display_name"`
	OtherAvatarURL   *string   `json:"other_avatar_url"`
	RequestStatus    string    `json:"request_status"`
	LastMessage      string    `json:"last_message"`
	LastSenderID     uuid.UUID `json:"last_sender_id"`
	LastMessageAt    string    `json:"last_message_at"`
	UnreadCount      int64     `json:"unread_count"`
}

type dmRequestResponse struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	RequestStatus  string    `json:"request_status"`
}

type dmReadResponse struct {
	UpdatedCount int64 `json:"updated_count"`
}

func dmMessageFromRow(row db.DmMessage) dmMessageObject {
	return dmMessageObjectFromFields(row.ID, row.ConversationID, row.SenderID, row.Content, row.CreatedAt, row.ReadAt)
}

func dmMessageFromSendRow(row db.SendDMMessageRow) dmMessageObject {
	return dmMessageObjectFromFields(row.ID, row.ConversationID, row.SenderID, row.Content, row.CreatedAt, row.ReadAt)
}

func dmMessageObjectFromFields(id, conversationID, senderID uuid.UUID, content string, createdAt time.Time, readAtValue sql.NullTime) dmMessageObject {
	out := dmMessageObject{
		ID:             id,
		ConversationID: conversationID,
		SenderID:       senderID,
		Content:        content,
		CreatedAt:      formatVideoTime(createdAt),
	}
	if readAtValue.Valid {
		readAt := formatVideoTime(readAtValue.Time)
		out.ReadAt = &readAt
	}
	return out
}

func (h *MessageHandler) Send(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "message handler not configured"))
	}
	sender, ok := middleware.UserFromContext(c)
	if !ok || sender == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req sendMessageRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	recipientID, err := uuid.Parse(req.RecipientID)
	if err != nil || recipientID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid recipient_id"))
	}
	if recipientID == sender.ID {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "cannot send a message to yourself"))
	}
	if err := validateDMMessageContent(req.Content); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	content := strings.TrimSpace(req.Content)

	ctx := c.Request().Context()
	recipient, err := h.Queries.GetUserByID(ctx, recipientID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("GetUserByID for direct message failed", "err", err, "recipient_id", recipientID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load recipient"))
	}
	if !recipient.IsActive {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}

	conversation, err := h.Queries.GetOrCreateDMConversation(ctx, db.GetOrCreateDMConversationParams{
		SenderID:    sender.ID,
		RecipientID: recipientID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("GetOrCreateDMConversation failed", "err", err, "sender_id", sender.ID, "recipient_id", recipientID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "open conversation"))
	}

	message, err := h.Queries.SendDMMessage(ctx, db.SendDMMessageParams{
		ConversationID: conversation.ID,
		SenderID:       sender.ID,
		Content:        content,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "conversation is unavailable"))
		}
		slog.Error("SendDMMessage failed", "err", err, "conversation_id", conversation.ID, "sender_id", sender.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "send message"))
	}
	return shared.RespondCreated(c, sendMessageResponse{
		ConversationID: conversation.ID,
		RequestStatus:  message.RequestStatus,
		Message:        dmMessageFromSendRow(message),
	})
}

func (h *MessageHandler) ListConversations(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "message handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	folder := c.QueryParam("folder")
	if folder == "" {
		folder = "inbox"
	}
	if folder != "inbox" && folder != "requests" && folder != "sent" {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "folder must be inbox, requests, or sent"))
	}
	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}
	params := db.ListDMConversationsParams{
		UserID:    user.ID,
		Folder:    folder,
		PageLimit: int32(limit + 1),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	rows, err := h.Queries.ListDMConversations(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListDMConversations failed", "err", err, "user_id", user.ID, "folder", folder)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "list conversations"))
	}
	hasNext := len(rows) > limit
	if hasNext {
		rows = rows[:limit]
	}
	items := make([]dmConversationObject, 0, len(rows))
	for _, row := range rows {
		item := dmConversationObject{
			ID:            row.ConversationID,
			OtherUserID:   row.OtherUserID,
			OtherUsername: row.OtherUsername,
			RequestStatus: row.RequestStatus,
			LastMessage:   row.LastMessage,
			LastSenderID:  row.LastSenderID,
			LastMessageAt: formatVideoTime(row.LastMessageAt),
			UnreadCount:   row.UnreadCount,
		}
		if row.OtherDisplayName.Valid {
			value := row.OtherDisplayName.String
			item.OtherDisplayName = &value
		}
		if row.OtherAvatarUrl.Valid {
			value := row.OtherAvatarUrl.String
			item.OtherAvatarURL = &value
		}
		items = append(items, item)
	}
	var nextCursor *string
	if hasNext {
		last := rows[len(rows)-1]
		cursor := shared.EncodeCursor(last.ConversationUpdatedAt, last.ConversationID)
		nextCursor = &cursor
	}
	return shared.RespondList(c, items, nextCursor)
}

func (h *MessageHandler) ListMessages(c echo.Context) error {
	user, conversationID, err := h.authorizedConversation(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}
	afterTS, afterID, err := shared.DecodeCursor(c.QueryParam("after"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid after cursor"))
	}
	if !cursorTS.IsZero() && !afterTS.IsZero() {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "cursor and after cannot be used together"))
	}
	if !afterTS.IsZero() {
		afterParams := db.ListDMMessagesAfterParams{
			ConversationID: conversationID,
			UserID:         user.ID,
			AfterCreated:   afterTS,
			AfterID:        afterID,
			PageLimit:      int32(limit + 1),
		}
		rows, err := h.Queries.ListDMMessagesAfter(c.Request().Context(), afterParams)
		if err != nil {
			slog.Error("ListDMMessagesAfter failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "poll new messages"))
		}
		if len(rows) > limit {
			rows = rows[:limit]
		}
		items := make([]dmMessageObject, 0, len(rows))
		for _, row := range rows {
			items = append(items, dmMessageFromRow(row))
		}
		nextCursor := shared.EncodeCursor(afterTS, afterID)
		if len(rows) > 0 {
			cursor := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			nextCursor = cursor
		}
		return shared.RespondList(c, items, &nextCursor)
	}
	params := db.ListDMMessagesParams{
		ConversationID: conversationID,
		UserID:         user.ID,
		PageLimit:      int32(limit + 1),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	rows, err := h.Queries.ListDMMessages(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListDMMessages failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "list messages"))
	}
	hasNext := len(rows) > limit
	if hasNext {
		rows = rows[:limit]
	}
	items := make([]dmMessageObject, 0, len(rows))
	for _, row := range rows {
		items = append(items, dmMessageFromRow(row))
	}
	var nextCursor *string
	if hasNext {
		last := rows[len(rows)-1]
		cursor := shared.EncodeCursor(last.CreatedAt, last.ID)
		nextCursor = &cursor
	}
	return shared.RespondList(c, items, nextCursor)
}

func (h *MessageHandler) MarkRead(c echo.Context) error {
	user, conversationID, err := h.authorizedConversation(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	updated, err := h.Queries.MarkDMMessagesRead(c.Request().Context(), db.MarkDMMessagesReadParams{
		ConversationID: conversationID,
		UserID:         user.ID,
	})
	if err != nil {
		slog.Error("MarkDMMessagesRead failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "mark messages read"))
	}
	return shared.RespondOK(c, dmReadResponse{UpdatedCount: updated})
}

func (h *MessageHandler) AcceptRequest(c echo.Context) error {
	user, conversationID, err := h.authenticatedConversationID(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	row, err := h.Queries.AcceptDMRequest(c.Request().Context(), db.AcceptDMRequestParams{
		ConversationID: conversationID,
		UserID:         user.ID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "message request not found"))
		}
		slog.Error("AcceptDMRequest failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "accept message request"))
	}
	return shared.RespondOK(c, dmRequestResponse{ConversationID: row.ID, RequestStatus: row.RequestStatus})
}

func (h *MessageHandler) DeclineRequest(c echo.Context) error {
	user, conversationID, err := h.authenticatedConversationID(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	row, err := h.Queries.DeclineDMRequest(c.Request().Context(), db.DeclineDMRequestParams{
		ConversationID: conversationID,
		UserID:         user.ID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "message request not found"))
		}
		slog.Error("DeclineDMRequest failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "decline message request"))
	}
	return shared.RespondOK(c, dmRequestResponse{ConversationID: row.ID, RequestStatus: row.RequestStatus})
}

func (h *MessageHandler) authenticatedConversationID(c echo.Context) (*db.User, uuid.UUID, error) {
	if h.Queries == nil {
		return nil, uuid.Nil, shared.Wrap(shared.ErrInternal, "message handler not configured")
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return nil, uuid.Nil, shared.Wrap(shared.ErrUnauthorized, "no authenticated user")
	}
	conversationID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return nil, uuid.Nil, shared.Wrap(shared.ErrValidation, "invalid conversation id")
	}
	return user, conversationID, nil
}

func (h *MessageHandler) authorizedConversation(c echo.Context) (*db.User, uuid.UUID, error) {
	user, conversationID, err := h.authenticatedConversationID(c)
	if err != nil {
		return nil, uuid.Nil, err
	}
	_, err = h.Queries.GetDMConversationForUser(c.Request().Context(), db.GetDMConversationForUserParams{
		ConversationID: conversationID,
		UserID:         user.ID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, uuid.Nil, shared.Wrap(shared.ErrNotFound, "conversation not found")
		}
		slog.Error("GetDMConversationForUser failed", "err", err, "conversation_id", conversationID, "user_id", user.ID)
		return nil, uuid.Nil, shared.Wrap(shared.ErrInternal, "load conversation")
	}
	return user, conversationID, nil
}

var _ messageStore = (*db.Queries)(nil)

func validateDMMessageContent(content string) error {
	content = strings.TrimSpace(content)
	if content == "" || strings.ContainsRune(content, '\x00') || utf8.RuneCountInString(content) > maxDMMessageRunes {
		return fmt.Errorf("content must contain 1 to %d characters", maxDMMessageRunes)
	}
	return nil
}
