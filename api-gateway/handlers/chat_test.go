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
	listUserConversations      func(ctx context.Context, arg db.ListUserConversationsParams) ([]db.ListUserConversationsRow, error)
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

func (s *stubChatStore) ListUserConversations(ctx context.Context, arg db.ListUserConversationsParams) ([]db.ListUserConversationsRow, error) {
	if s.listUserConversations != nil {
		return s.listUserConversations(ctx, arg)
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

// ---- cursor pagination ------------------------------------------------

type listEnvelope struct {
	Data       []map[string]any `json:"data"`
	Pagination struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"pagination"`
}

func callList(t *testing.T, h *ChatHandler, user *db.User, target string, params map[string]string, fn func(echo.Context) error) (listEnvelope, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if id, ok := params["id"]; ok {
		c.SetParamNames("id")
		c.SetParamValues(id)
	}
	c.Set("auth.currentUser", user)

	if err := fn(c); err != nil {
		return listEnvelope{}, err
	}
	var out listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list envelope: %v (body %s)", err, rec.Body.String())
	}
	return out, nil
}

func messageRows(n int, base time.Time) []db.ListMessagesRow {
	rows := make([]db.ListMessagesRow, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, db.ListMessagesRow{
			ID:             uuid.New(),
			ConversationID: uuid.New(),
			SenderID:       uuid.New(),
			MessageType:    "text",
			Content:        sql.NullString{String: "m", Valid: true},
			CreatedAt:      base.Add(-time.Duration(i) * time.Second),
		})
	}
	return rows
}

func TestChat_ListMessages_CursorPagination(t *testing.T) {
	user := &db.User{ID: uuid.New(), Username: "alice"}
	convID := uuid.New()
	base := time.Now().UTC().Truncate(time.Microsecond)
	rows := messageRows(3, base)

	var got db.ListMessagesParams
	store := &stubChatStore{
		isConversationMember: func(ctx context.Context, arg db.IsConversationMemberParams) (bool, error) { return true, nil },
		listMessages: func(ctx context.Context, arg db.ListMessagesParams) ([]db.ListMessagesRow, error) {
			got = arg
			return rows, nil
		},
	}
	h := &ChatHandler{Queries: store, Hub: NewChatHub()}
	target := "/api/chat/conversations/" + convID.String() + "/messages"

	// A full page (len == limit) must return a next_cursor for the last row.
	out, err := callList(t, h, user, target+"?limit=3", map[string]string{"id": convID.String()}, h.ListMessages)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if got.PageLimit != 3 || got.CursorCreated.Valid {
		t.Errorf("first page params wrong: %+v", got)
	}
	if len(out.Data) != 3 {
		t.Fatalf("expected 3 items, got %d", len(out.Data))
	}
	if out.Pagination.NextCursor == nil {
		t.Fatal("expected next_cursor on a full page")
	}
	ts, id, err := shared.DecodeCursor(*out.Pagination.NextCursor)
	if err != nil {
		t.Fatalf("next_cursor is not a valid shared cursor: %v", err)
	}
	last := rows[len(rows)-1]
	if !ts.Equal(last.CreatedAt) || id != last.ID {
		t.Errorf("cursor points at (%v,%v), want (%v,%v)", ts, id, last.CreatedAt, last.ID)
	}

	// Feeding that cursor back must reach the store as (created_at, id).
	_, err = callList(t, h, user, target+"?limit=3&cursor="+*out.Pagination.NextCursor, map[string]string{"id": convID.String()}, h.ListMessages)
	if err != nil {
		t.Fatalf("ListMessages page 2: %v", err)
	}
	if !got.CursorCreated.Valid || !got.CursorCreated.Time.Equal(last.CreatedAt) || !got.CursorID.Valid || got.CursorID.UUID != last.ID {
		t.Errorf("cursor not forwarded to the query: %+v", got)
	}

	// A short page means "no more pages": next_cursor is null, not "".
	rows = messageRows(2, base)
	out, err = callList(t, h, user, target+"?limit=3", map[string]string{"id": convID.String()}, h.ListMessages)
	if err != nil {
		t.Fatalf("ListMessages short page: %v", err)
	}
	if out.Pagination.NextCursor != nil {
		t.Errorf("expected null next_cursor on the last page, got %q", *out.Pagination.NextCursor)
	}
}

func TestChat_ListMessages_BadCursorAndLimit(t *testing.T) {
	user := &db.User{ID: uuid.New(), Username: "alice"}
	convID := uuid.New()
	store := &stubChatStore{
		isConversationMember: func(ctx context.Context, arg db.IsConversationMemberParams) (bool, error) { return true, nil },
	}
	h := &ChatHandler{Queries: store, Hub: NewChatHub()}
	target := "/api/chat/conversations/" + convID.String() + "/messages"

	for _, q := range []string{"?cursor=not-a-cursor", "?limit=0", "?limit=abc"} {
		_, err := callList(t, h, user, target+q, map[string]string{"id": convID.String()}, h.ListMessages)
		if !errors.Is(err, shared.ErrValidation) {
			t.Errorf("%s: expected ErrValidation, got %v", q, err)
		}
	}
}

func TestChat_ListConversations_CursorPagination(t *testing.T) {
	user := &db.User{ID: uuid.New(), Username: "alice"}
	base := time.Now().UTC().Truncate(time.Microsecond)
	rows := []db.ListUserConversationsRow{
		{ID: uuid.New(), Type: "direct", CreatedAt: base, UpdatedAt: base},
		{ID: uuid.New(), Type: "direct", CreatedAt: base, UpdatedAt: base.Add(-time.Second)},
	}

	var got db.ListUserConversationsParams
	store := &stubChatStore{
		listUserConversations: func(ctx context.Context, arg db.ListUserConversationsParams) ([]db.ListUserConversationsRow, error) {
			got = arg
			return rows, nil
		},
	}
	h := &ChatHandler{Queries: store, Hub: NewChatHub()}

	out, err := callList(t, h, user, "/api/chat/conversations?limit=2", nil, h.ListConversations)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if got.UserID != user.ID || got.PageLimit != 2 || got.CursorUpdated.Valid {
		t.Errorf("first page params wrong: %+v", got)
	}
	if len(out.Data) != 2 || out.Pagination.NextCursor == nil {
		t.Fatalf("expected 2 items and a next_cursor, got %d / %v", len(out.Data), out.Pagination.NextCursor)
	}
	ts, id, err := shared.DecodeCursor(*out.Pagination.NextCursor)
	if err != nil || !ts.Equal(rows[1].UpdatedAt) || id != rows[1].ID {
		t.Errorf("cursor should be (updated_at, id) of the last row; got (%v,%v,%v)", ts, id, err)
	}

	_, err = callList(t, h, user, "/api/chat/conversations?cursor="+*out.Pagination.NextCursor, nil, h.ListConversations)
	if err != nil {
		t.Fatalf("ListConversations page 2: %v", err)
	}
	if !got.CursorUpdated.Valid || !got.CursorUpdated.Time.Equal(rows[1].UpdatedAt) || got.CursorID.UUID != rows[1].ID {
		t.Errorf("cursor not forwarded to the query: %+v", got)
	}

	if _, err = callList(t, h, user, "/api/chat/conversations?cursor=not-a-cursor", nil, h.ListConversations); !errors.Is(err, shared.ErrValidation) {
		t.Errorf("bad cursor: expected ErrValidation, got %v", err)
	}
}

// ---- WebSocket ticket auth (the browser path) -------------------------

const chatTestSecret = "chat-test-secret-chat-test-secret-32b"

func newTicketHandler(store *stubChatStore) *ChatHandler {
	return &ChatHandler{
		Queries: store,
		Hub:     NewChatHub(),
		Cfg:     &shared.APIConfig{MediaTokenSecret: chatTestSecret},
	}
}

func TestChat_IssueWSTicket(t *testing.T) {
	e := echo.New()
	user := &db.User{ID: uuid.New(), Username: "alice"}
	h := newTicketHandler(&stubChatStore{})

	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/chat/ws-ticket", nil), rec)
	c.Set("auth.currentUser", user)
	if err := h.IssueWSTicket(c); err != nil {
		t.Fatalf("IssueWSTicket: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Data struct {
			Ticket    string    `json:"ticket"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	uid, err := shared.VerifyWSTicket(body.Data.Ticket, chatTestSecret)
	if err != nil || uid != user.ID {
		t.Fatalf("issued ticket does not verify for the user: %v %v", uid, err)
	}
	if time.Until(body.Data.ExpiresAt) > time.Minute {
		t.Errorf("ticket should be short-lived, expires_at=%v", body.Data.ExpiresAt)
	}

	// No secret configured: fail loudly instead of issuing an unsigned ticket.
	h.Cfg = &shared.APIConfig{}
	c = e.NewContext(httptest.NewRequest(http.MethodPost, "/api/chat/ws-ticket", nil), httptest.NewRecorder())
	c.Set("auth.currentUser", user)
	if err := h.IssueWSTicket(c); !errors.Is(err, shared.ErrInternal) {
		t.Errorf("expected ErrInternal without a secret, got %v", err)
	}
}

func TestChat_HandleWebSocket_TicketAuth(t *testing.T) {
	e := echo.New()
	uid := uuid.New()
	active := func(ctx context.Context, id uuid.UUID) (db.User, error) { return db.User{ID: id, IsActive: true}, nil }
	valid, _, _ := shared.NewWSTicket(uid, chatTestSecret, time.Minute)
	expired, _ := shared.SignWSTicket(uid, chatTestSecret, time.Now().Add(-time.Minute))

	cases := []struct {
		name       string
		query      string
		getUser    func(ctx context.Context, id uuid.UUID) (db.User, error)
		wantUnauth bool
	}{
		{"no ticket", "", active, true},
		{"garbage ticket", "?ticket=abc", active, true},
		{"expired ticket", "?ticket=" + expired, active, true},
		{"unknown user", "?ticket=" + valid, func(ctx context.Context, id uuid.UUID) (db.User, error) { return db.User{}, sql.ErrNoRows }, true},
		{"deactivated user", "?ticket=" + valid, func(ctx context.Context, id uuid.UUID) (db.User, error) { return db.User{ID: id, IsActive: false}, nil }, true},
		// A valid ticket gets past authentication; this plain HTTP request is
		// then refused by the websocket upgrader itself (400, not 401).
		{"valid ticket", "?ticket=" + valid, active, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTicketHandler(&stubChatStore{getUserByID: tc.getUser})
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/chat/ws"+tc.query, nil), rec)

			err := h.HandleWebSocket(c)
			if tc.wantUnauth {
				if !errors.Is(err, shared.ErrUnauthorized) {
					t.Fatalf("expected ErrUnauthorized, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected the handshake to pass authentication, got %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected the upgrader to answer 400 to a non-websocket request, got %d", rec.Code)
			}
		})
	}
}
