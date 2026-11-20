package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type stubMessageStore struct {
	getUserByID               func(context.Context, uuid.UUID) (db.User, error)
	getOrCreateDMConversation func(context.Context, db.GetOrCreateDMConversationParams) (db.DmConversation, error)
	sendDMMessage             func(context.Context, db.SendDMMessageParams) (db.SendDMMessageRow, error)
	listDMConversations       func(context.Context, db.ListDMConversationsParams) ([]db.ListDMConversationsRow, error)
	getDMConversationForUser  func(context.Context, db.GetDMConversationForUserParams) (db.DmConversation, error)
	listDMMessages            func(context.Context, db.ListDMMessagesParams) ([]db.DmMessage, error)
	listDMMessagesAfter       func(context.Context, db.ListDMMessagesAfterParams) ([]db.DmMessage, error)
	markDMMessagesRead        func(context.Context, db.MarkDMMessagesReadParams) (int64, error)
	acceptDMRequest           func(context.Context, db.AcceptDMRequestParams) (db.AcceptDMRequestRow, error)
	declineDMRequest          func(context.Context, db.DeclineDMRequestParams) (db.DeclineDMRequestRow, error)
}

func (s *stubMessageStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.getUserByID == nil {
		return db.User{}, errors.New("GetUserByID not scripted")
	}
	return s.getUserByID(ctx, id)
}
func (s *stubMessageStore) GetOrCreateDMConversation(ctx context.Context, arg db.GetOrCreateDMConversationParams) (db.DmConversation, error) {
	if s.getOrCreateDMConversation == nil {
		return db.DmConversation{}, errors.New("GetOrCreateDMConversation not scripted")
	}
	return s.getOrCreateDMConversation(ctx, arg)
}
func (s *stubMessageStore) SendDMMessage(ctx context.Context, arg db.SendDMMessageParams) (db.SendDMMessageRow, error) {
	if s.sendDMMessage == nil {
		return db.SendDMMessageRow{}, errors.New("SendDMMessage not scripted")
	}
	return s.sendDMMessage(ctx, arg)
}
func (s *stubMessageStore) ListDMConversations(ctx context.Context, arg db.ListDMConversationsParams) ([]db.ListDMConversationsRow, error) {
	if s.listDMConversations == nil {
		return nil, errors.New("ListDMConversations not scripted")
	}
	return s.listDMConversations(ctx, arg)
}
func (s *stubMessageStore) GetDMConversationForUser(ctx context.Context, arg db.GetDMConversationForUserParams) (db.DmConversation, error) {
	if s.getDMConversationForUser == nil {
		return db.DmConversation{}, errors.New("GetDMConversationForUser not scripted")
	}
	return s.getDMConversationForUser(ctx, arg)
}
func (s *stubMessageStore) ListDMMessages(ctx context.Context, arg db.ListDMMessagesParams) ([]db.DmMessage, error) {
	if s.listDMMessages == nil {
		return nil, errors.New("ListDMMessages not scripted")
	}
	return s.listDMMessages(ctx, arg)
}
func (s *stubMessageStore) ListDMMessagesAfter(ctx context.Context, arg db.ListDMMessagesAfterParams) ([]db.DmMessage, error) {
	if s.listDMMessagesAfter == nil {
		return nil, errors.New("ListDMMessagesAfter not scripted")
	}
	return s.listDMMessagesAfter(ctx, arg)
}
func (s *stubMessageStore) MarkDMMessagesRead(ctx context.Context, arg db.MarkDMMessagesReadParams) (int64, error) {
	if s.markDMMessagesRead == nil {
		return 0, errors.New("MarkDMMessagesRead not scripted")
	}
	return s.markDMMessagesRead(ctx, arg)
}
func (s *stubMessageStore) AcceptDMRequest(ctx context.Context, arg db.AcceptDMRequestParams) (db.AcceptDMRequestRow, error) {
	if s.acceptDMRequest == nil {
		return db.AcceptDMRequestRow{}, errors.New("AcceptDMRequest not scripted")
	}
	return s.acceptDMRequest(ctx, arg)
}
func (s *stubMessageStore) DeclineDMRequest(ctx context.Context, arg db.DeclineDMRequestParams) (db.DeclineDMRequestRow, error) {
	if s.declineDMRequest == nil {
		return db.DeclineDMRequestRow{}, errors.New("DeclineDMRequest not scripted")
	}
	return s.declineDMRequest(ctx, arg)
}

func TestMessageHandlerSendCreatesPendingRequest(t *testing.T) {
	conversationID := uuid.New()
	now := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	store := &stubMessageStore{
		getUserByID: func(_ context.Context, id uuid.UUID) (db.User, error) {
			if id != testOwnerID {
				t.Fatalf("recipient id = %s, want %s", id, testOwnerID)
			}
			return db.User{ID: id, Username: "target", IsActive: true}, nil
		},
		getOrCreateDMConversation: func(_ context.Context, arg db.GetOrCreateDMConversationParams) (db.DmConversation, error) {
			if arg.SenderID != testViewerID || arg.RecipientID != testOwnerID {
				t.Fatalf("conversation args = %+v", arg)
			}
			return db.DmConversation{ID: conversationID, RequestStatus: "pending"}, nil
		},
		sendDMMessage: func(_ context.Context, arg db.SendDMMessageParams) (db.SendDMMessageRow, error) {
			if arg.ConversationID != conversationID || arg.SenderID != testViewerID || arg.Content != "hello" {
				t.Fatalf("send args = %+v", arg)
			}
			return db.SendDMMessageRow{
				ID: uuid.New(), ConversationID: conversationID, SenderID: testViewerID,
				Content: arg.Content, CreatedAt: now, RequestStatus: "pending",
			}, nil
		},
	}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/messages", handler.Send)

	rec := doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testOwnerID.String()+`","content":"  hello  "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ConversationID uuid.UUID `json:"conversation_id"`
		RequestStatus  string    `json:"request_status"`
		Message        struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	decodeData(t, rec, &got)
	if got.ConversationID != conversationID || got.RequestStatus != "pending" || got.Message.Content != "hello" {
		t.Errorf("response = %+v", got)
	}
}

func TestMessageHandlerSendRejectsSelfAndInvalidContent(t *testing.T) {
	store := &stubMessageStore{}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/messages", handler.Send)

	rec := doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testViewerID.String()+`","content":"hello"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)

	rec = doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testOwnerID.String()+`","content":"`+strings.Repeat("界", maxDMMessageRunes+1)+`"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)

	rec = doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testOwnerID.String()+`","content":"   "}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)

	rec = doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testOwnerID.String()+`","content":"a\u0000b"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
}

func TestMessageHandlerPendingRecipientCannotReplyBeforeAccepting(t *testing.T) {
	conversationID := uuid.New()
	store := &stubMessageStore{
		getUserByID: func(_ context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true}, nil
		},
		getOrCreateDMConversation: func(_ context.Context, _ db.GetOrCreateDMConversationParams) (db.DmConversation, error) {
			return db.DmConversation{
				ID: conversationID, RequestStatus: "pending",
				RequestRecipientID: uuid.NullUUID{UUID: testViewerID, Valid: true},
			}, nil
		},
		sendDMMessage: func(_ context.Context, _ db.SendDMMessageParams) (db.SendDMMessageRow, error) {
			return db.SendDMMessageRow{}, sql.ErrNoRows
		},
	}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/messages", handler.Send)

	rec := doJSON(e, http.MethodPost, "/api/messages",
		`{"recipient_id":"`+testOwnerID.String()+`","content":"reply"}`)
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestMessageHandlerAcceptRequestForAuthenticatedRecipient(t *testing.T) {
	conversationID := uuid.New()
	store := &stubMessageStore{
		acceptDMRequest: func(_ context.Context, arg db.AcceptDMRequestParams) (db.AcceptDMRequestRow, error) {
			if arg.ConversationID != conversationID || arg.UserID != testViewerID {
				t.Fatalf("accept args = %+v", arg)
			}
			return db.AcceptDMRequestRow{ID: conversationID, RequestStatus: "accepted"}, nil
		},
	}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/messages/conversations/:id/accept", handler.AcceptRequest)
	rec := doJSON(e, http.MethodPost, "/api/messages/conversations/"+conversationID.String()+"/accept", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got dmRequestResponse
	decodeData(t, rec, &got)
	if got.ConversationID != conversationID || got.RequestStatus != "accepted" {
		t.Errorf("response = %+v", got)
	}
}

func TestMessageHandlerListMessagesHidesNonParticipantConversation(t *testing.T) {
	conversationID := uuid.New()
	store := &stubMessageStore{
		getDMConversationForUser: func(_ context.Context, arg db.GetDMConversationForUserParams) (db.DmConversation, error) {
			if arg.ConversationID != conversationID || arg.UserID != testViewerID {
				t.Fatalf("conversation access args = %+v", arg)
			}
			return db.DmConversation{}, sql.ErrNoRows
		},
		listDMMessages: func(context.Context, db.ListDMMessagesParams) ([]db.DmMessage, error) {
			t.Fatal("message list must not be queried for a non-participant")
			return nil, nil
		},
	}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/messages/conversations/:id", handler.ListMessages)
	rec := doJSON(e, http.MethodGet, "/api/messages/conversations/"+conversationID.String(), "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestMessageHandlerPollsMessagesAfterCursor(t *testing.T) {
	conversationID := uuid.New()
	afterID := uuid.New()
	afterTime := time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)
	messageID := uuid.New()
	messageTime := afterTime.Add(time.Second)
	store := &stubMessageStore{
		getDMConversationForUser: func(_ context.Context, arg db.GetDMConversationForUserParams) (db.DmConversation, error) {
			return db.DmConversation{ID: arg.ConversationID, RequestStatus: "accepted"}, nil
		},
		listDMMessagesAfter: func(_ context.Context, arg db.ListDMMessagesAfterParams) ([]db.DmMessage, error) {
			if arg.ConversationID != conversationID || arg.UserID != testViewerID ||
				arg.AfterID != afterID || !arg.AfterCreated.Equal(afterTime) || arg.PageLimit != int32(videoListDefaultLimit+1) {
				t.Fatalf("poll args = %+v", arg)
			}
			return []db.DmMessage{{
				ID: messageID, ConversationID: conversationID, SenderID: testOwnerID,
				Content: "new message", CreatedAt: messageTime,
			}}, nil
		},
	}
	handler := newMessageHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/messages/conversations/:id", handler.ListMessages)
	after := shared.EncodeCursor(afterTime, afterID)
	rec := doJSON(e, http.MethodGet, "/api/messages/conversations/"+conversationID.String()+"?after="+after, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got []dmMessageObject
	decodeData(t, rec, &got)
	if len(got) != 1 || got[0].ID != messageID || got[0].Content != "new message" {
		t.Errorf("messages = %+v", got)
	}
}
