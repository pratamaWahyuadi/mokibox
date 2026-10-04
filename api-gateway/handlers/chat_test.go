package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type stubChatStore struct {
	createConversation         func(ctx context.Context, arg db.CreateConversationParams) (db.Conversation, error)
	addConversationMember      func(ctx context.Context, arg db.AddConversationMemberParams) error
	findDirectConversation     func(ctx context.Context, arg db.FindDirectConversationParams) (db.Conversation, error)
	getConversation            func(ctx context.Context, id uuid.UUID) (db.Conversation, error)
	isConversationMember       func(ctx context.Context, arg db.IsConversationMemberParams) (bool, error)
	listConversationMembers    func(ctx context.Context, conversationID uuid.UUID) ([]db.ListConversationMembersRow, error)
	listUserConversations      func(ctx context.Context, userID uuid.UUID) ([]db.ListUserConversationsRow, error)
	insertMessage              func(ctx context.Context, arg db.InsertMessageParams) (db.Message, error)
	listMessages               func(ctx context.Context, arg db.ListMessagesParams) ([]db.ListMessagesRow, error)
	updateLastReadAt           func(ctx context.Context, arg db.UpdateLastReadAtParams) error
	touchConversationUpdatedAt func(ctx context.Context, id uuid.UUID) error
	getUserByZitadelID         func(ctx context.Context, zitadelID string) (db.User, error)
	getUserByID                func(ctx context.Context, id uuid.UUID) (db.User, error)
}

func (s *stubChatStore) CreateConversation(ctx context.Context, arg db.CreateConversationParams) (db.Conversation, error) {
	if s.createConversation != nil {
		return s.createConversation(ctx, arg)
	}
	return db.Conversation{}, nil
}

func (s *stubChatStore) AddConversationMember(ctx context.Context, arg db.AddConversationMemberParams) error {
	if s.addConversationMember != nil {
		return s.addConversationMember(ctx, arg)
	}
	return nil
}

func (s *stubChatStore) FindDirectConversation(ctx context.Context, arg db.FindDirectConversationParams) (db.Conversation, error) {
	if s.findDirectConversation != nil {
		return s.findDirectConversation(ctx, arg)
	}
	return db.Conversation{}, sql.ErrNoRows
}

func (s *stubChatStore) GetConversation(ctx context.Context, id uuid.UUID) (db.Conversation, error) {
	if s.getConversation != nil {
		return s.getConversation(ctx, id)
	}
	return db.Conversation{ID: id}, nil
}

func (s *stubChatStore) IsConversationMember(ctx context.Context, arg db.IsConversationMemberParams) (bool, error) {
	if s.isConversationMember != nil {
		return s.isConversationMember(ctx, arg)
	}
	return true, nil
}

func (s *stubChatStore) ListConversationMembers(ctx context.Context, conversationID uuid.UUID) ([]db.ListConversationMembersRow, error) {
	if s.listConversationMembers != nil {
		return s.listConversationMembers(ctx, conversationID)
	}
	return nil, nil
}

func (s *stubChatStore) ListUserConversations(ctx context.Context, userID uuid.UUID) ([]db.ListUserConversationsRow, error) {
	if s.listUserConversations != nil {
		return s.listUserConversations(ctx, userID)
	}
	return nil, nil
}

func (s *stubChatStore) InsertMessage(ctx context.Context, arg db.InsertMessageParams) (db.Message, error) {
	if s.insertMessage != nil {
		return s.insertMessage(ctx, arg)
	}
	return db.Message{
		ID:             uuid.New(),
		ConversationID: arg.ConversationID,
		SenderID:       arg.SenderID,
		MessageType:    arg.MessageType,
		Content:        arg.Content,
		MediaUrl:       arg.MediaUrl,
		MediaMetadata:  arg.MediaMetadata,
		CreatedAt:      time.Now(),
	}, nil
}

func (s *stubChatStore) ListMessages(ctx context.Context, arg db.ListMessagesParams) ([]db.ListMessagesRow, error) {
	if s.listMessages != nil {
		return s.listMessages(ctx, arg)
	}
	return nil, nil
}

func (s *stubChatStore) UpdateLastReadAt(ctx context.Context, arg db.UpdateLastReadAtParams) error {
	if s.updateLastReadAt != nil {
		return s.updateLastReadAt(ctx, arg)
	}
	return nil
}

func (s *stubChatStore) TouchConversationUpdatedAt(ctx context.Context, id uuid.UUID) error {
	if s.touchConversationUpdatedAt != nil {
		return s.touchConversationUpdatedAt(ctx, id)
	}
	return nil
}

func (s *stubChatStore) GetUserByZitadelID(ctx context.Context, zitadelID string) (db.User, error) {
	if s.getUserByZitadelID != nil {
		return s.getUserByZitadelID(ctx, zitadelID)
	}
	return db.User{}, nil
}

func (s *stubChatStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.getUserByID != nil {
		return s.getUserByID(ctx, id)
	}
	return db.User{ID: id, IsActive: true}, nil
}

type stubChatR2 struct {
	presignPut func(ctx context.Context, key string, contentType string, expiry time.Duration) (string, error)
}

func (r *stubChatR2) PresignPut(ctx context.Context, key string, contentType string, expiry time.Duration) (string, error) {
	if r.presignPut != nil {
		return r.presignPut(ctx, key, contentType, expiry)
	}
	return "https://mock-r2.cloudflarestorage.com/" + key, nil
}

func TestChat_CreateConversation(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}
	targetID := uuid.New()

	store := &stubChatStore{
		findDirectConversation: func(ctx context.Context, arg db.FindDirectConversationParams) (db.Conversation, error) {
			return db.Conversation{}, sql.ErrNoRows
		},
		createConversation: func(ctx context.Context, arg db.CreateConversationParams) (db.Conversation, error) {
			return db.Conversation{
				ID:        uuid.New(),
				Type:      arg.Type,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}, nil
		},
	}

	h := &ChatHandler{
		Queries: store,
		Hub:     NewChatHub(),
	}

	body := `{"target_user_id":"` + targetID.String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("auth.currentUser", user)

	if err := h.CreateConversation(c); err != nil {
		t.Fatalf("CreateConversation error: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201 Created, got %d", rec.Code)
	}
}

func TestChat_SendMessage(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}
	convID := uuid.New()

	store := &stubChatStore{
		isConversationMember: func(ctx context.Context, arg db.IsConversationMemberParams) (bool, error) {
			return true, nil
		},
		insertMessage: func(ctx context.Context, arg db.InsertMessageParams) (db.Message, error) {
			return db.Message{
				ID:             uuid.New(),
				ConversationID: arg.ConversationID,
				SenderID:       arg.SenderID,
				MessageType:    arg.MessageType,
				Content:        arg.Content,
				CreatedAt:      time.Now(),
			}, nil
		},
	}

	h := &ChatHandler{
		Queries: store,
		Hub:     NewChatHub(),
	}

	body := `{"message_type":"text","content":"Hello world!"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations/"+convID.String()+"/messages", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(convID.String())
	c.Set("auth.currentUser", user)

	if err := h.SendMessage(c); err != nil {
		t.Fatalf("SendMessage error: %v", err)
	}

	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201 Created, got %d", rec.Code)
	}
}

func TestChat_UploadIntent(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}

	h := &ChatHandler{
		Queries: &stubChatStore{},
		R2:      &stubChatR2{},
		Cfg:     &shared.APIConfig{PresignUploadExpiry: 15 * time.Minute},
	}

	body := `{"file_type":"photo","content_type":"image/png"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/upload-intent", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("auth.currentUser", user)

	if err := h.UploadIntent(c); err != nil {
		t.Fatalf("UploadIntent error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", rec.Code)
	}

	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["upload_url"] == nil || res["media_key"] == nil {
		t.Errorf("expected upload_url and media_key in response, got %v", res)
	}
}

func TestChat_CreateConversation_UnknownTargetIsRejected(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}
	created := false

	store := &stubChatStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{}, sql.ErrNoRows
		},
		createConversation: func(ctx context.Context, arg db.CreateConversationParams) (db.Conversation, error) {
			created = true
			return db.Conversation{ID: uuid.New()}, nil
		},
	}
	h := &ChatHandler{Queries: store, Hub: NewChatHub()}

	body := `{"target_user_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, httptest.NewRecorder())
	c.Set("auth.currentUser", user)

	err := h.CreateConversation(c)
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown target user, got %v", err)
	}
	if created {
		t.Error("no conversation must be created for an unknown target user")
	}
}

func TestChat_CreateConversation_SelfIsRejected(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}
	h := &ChatHandler{Queries: &stubChatStore{}, Hub: NewChatHub()}

	body := `{"target_user_id":"` + user.ID.String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat/conversations", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, httptest.NewRecorder())
	c.Set("auth.currentUser", user)

	if err := h.CreateConversation(c); !errors.Is(err, shared.ErrValidation) {
		t.Fatalf("expected ErrValidation when chatting with yourself, got %v", err)
	}
}

func TestChat_SendMessageInternal_Validation(t *testing.T) {
	h := &ChatHandler{Queries: &stubChatStore{}, Hub: NewChatHub()}
	sender, conv := uuid.New(), uuid.New()

	tooLong := strings.Repeat("a", maxMessageContentLen+1)

	cases := []struct {
		name    string
		msgType string
		content string
		media   string
		meta    json.RawMessage
		wantErr error
	}{
		{"empty text", "text", "   ", "", nil, shared.ErrValidation},
		{"text too long", "text", tooLong, "", nil, shared.ErrValidation},
		{"photo without media", "photo", "caption", "", nil, shared.ErrValidation},
		{"video without media", "video", "", "", nil, shared.ErrValidation},
		{"empty sticker", "sticker", "", "", nil, shared.ErrValidation},
		{"unsupported type", "audio", "x", "", nil, shared.ErrValidation},
		{"bad metadata", "text", "hi", "", json.RawMessage("{not json"), shared.ErrValidation},
		{"valid text", "text", "hi", "", nil, nil},
		{"valid photo", "photo", "", "https://cdn.example/x.jpg", json.RawMessage(`{"w":1}`), nil},
		{"valid sticker", "sticker", "pack_1_happy", "", nil, nil},
		{"default type is text", "", "hi", "", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.SendMessageInternal(context.Background(), sender, conv, tc.msgType, tc.content, tc.media, tc.meta)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestChat_SendMessageInternal_NonMemberIsRejected(t *testing.T) {
	store := &stubChatStore{
		isConversationMember: func(ctx context.Context, arg db.IsConversationMemberParams) (bool, error) {
			return false, nil
		},
	}
	h := &ChatHandler{Queries: store, Hub: NewChatHub()}

	_, err := h.SendMessageInternal(context.Background(), uuid.New(), uuid.New(), "text", "hi", "", nil)
	if !errors.Is(err, shared.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for a non-member, got %v", err)
	}
}
