package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type dmTestStore struct {
	device               db.DmDevice
	deviceErr            error
	updateDevice         db.DmDevice
	updateDeviceErr      error
	bundleDevice         db.DmDevice
	bundleErr            error
	claimedKey           db.ClaimDMOneTimeKeyRow
	claimErr             error
	recipientDevice      db.GetEligibleDMRecipientDeviceForSendRow
	recipientErr         error
	existing             db.GetDMSendIdempotencyByClientIDRow
	existingErr          error
	idempotencyInsertErr error
	concurrentExisting   *db.GetDMSendIdempotencyByClientIDRow
	idempotencyLookups   int
	inserted             db.InsertDMInboxMessageRow
	insertErr            error
	insertedIdempotency  int
	nextSequence         int64
	pendingMessages      int64
	insertedMessages     int
	inboxRows            []db.ListDMInboxAfterRow
	ackRows              int64
	registered           db.DmDevice
	count                int64
	insertedKeys         int
	lastAck              db.AckDMInboxMessageParams
}

func (s *dmTestStore) DeleteDMDevicesByUser(context.Context, uuid.UUID) error { return nil }
func (s *dmTestStore) CreateDMDevice(_ context.Context, arg db.CreateDMDeviceParams) (db.DmDevice, error) {
	s.registered = db.DmDevice{
		ID: arg.ID, UserID: arg.UserID, Protocol: arg.Protocol,
		ProtocolVersion: arg.ProtocolVersion, KeyBundle: arg.KeyBundle,
		FallbackKeyID: arg.FallbackKeyID, FallbackKeyMaterial: arg.FallbackKeyMaterial,
		RegisteredAt: time.Unix(1, 0).UTC(),
	}
	return s.registered, nil
}
func (s *dmTestStore) InsertDMOneTimeKey(context.Context, db.InsertDMOneTimeKeyParams) (int64, error) {
	s.insertedKeys++
	return 1, nil
}
func (s *dmTestStore) CountDMOneTimeKeys(context.Context, uuid.UUID) (int64, error) {
	return s.count, nil
}
func (s *dmTestStore) CountPendingDMInboxForRecipient(context.Context, uuid.UUID) (int64, error) {
	return s.pendingMessages, nil
}
func (s *dmTestStore) GetDMDeviceByUser(context.Context, uuid.UUID) (db.DmDevice, error) {
	return s.device, s.deviceErr
}
func (s *dmTestStore) GetDMDeviceByUserForKeyShare(context.Context, uuid.UUID) (db.DmDevice, error) {
	return s.device, s.deviceErr
}
func (s *dmTestStore) GetDMDeviceForUpdate(context.Context, uuid.UUID) (db.DmDevice, error) {
	if s.updateDeviceErr != nil {
		return s.updateDevice, s.updateDeviceErr
	}
	return s.updateDevice, nil
}
func (s *dmTestStore) GetEligibleDMDeviceForBundle(context.Context, db.GetEligibleDMDeviceForBundleParams) (db.DmDevice, error) {
	return s.bundleDevice, s.bundleErr
}
func (s *dmTestStore) ClaimDMOneTimeKey(context.Context, uuid.UUID) (db.ClaimDMOneTimeKeyRow, error) {
	return s.claimedKey, s.claimErr
}
func (s *dmTestStore) GetEligibleDMRecipientDeviceForSend(context.Context, db.GetEligibleDMRecipientDeviceForSendParams) (db.GetEligibleDMRecipientDeviceForSendRow, error) {
	return s.recipientDevice, s.recipientErr
}
func (s *dmTestStore) IncrementDMDeviceSequence(context.Context, uuid.UUID) (int64, error) {
	return s.nextSequence, nil
}
func (s *dmTestStore) GetDMSendIdempotencyByClientID(context.Context, db.GetDMSendIdempotencyByClientIDParams) (db.GetDMSendIdempotencyByClientIDRow, error) {
	s.idempotencyLookups++
	if s.concurrentExisting != nil && s.idempotencyLookups > 1 {
		return *s.concurrentExisting, nil
	}
	return s.existing, s.existingErr
}
func (s *dmTestStore) InsertDMSendIdempotency(_ context.Context, arg db.InsertDMSendIdempotencyParams) (db.InsertDMSendIdempotencyRow, error) {
	if s.idempotencyInsertErr != nil {
		return db.InsertDMSendIdempotencyRow{}, s.idempotencyInsertErr
	}
	s.insertedIdempotency++
	createdAt := time.Unix(2, 0).UTC()
	return db.InsertDMSendIdempotencyRow{
		MessageID: arg.MessageID, SequenceNo: arg.SequenceNo,
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Duration(arg.ExpiresInSeconds) * time.Second),
	}, nil
}
func (s *dmTestStore) InsertDMInboxMessage(_ context.Context, arg db.InsertDMInboxMessageParams) (db.InsertDMInboxMessageRow, error) {
	if s.insertErr != nil {
		return s.inserted, s.insertErr
	}
	s.insertedMessages++
	return db.InsertDMInboxMessageRow{
		ID: arg.ID, SequenceNo: arg.SequenceNo,
		ClientMessageID: arg.ClientMessageID,
		CreatedAt:       arg.CreatedAt,
		ExpiresAt:       arg.ExpiresAt,
	}, nil
}
func (s *dmTestStore) ListDMInboxAfter(context.Context, db.ListDMInboxAfterParams) ([]db.ListDMInboxAfterRow, error) {
	return s.inboxRows, nil
}
func (s *dmTestStore) AckDMInboxMessage(_ context.Context, arg db.AckDMInboxMessageParams) (int64, error) {
	s.lastAck = arg
	return s.ackRows, nil
}

type dmTestRunner struct{ store dmTxStore }

func (r dmTestRunner) Run(ctx context.Context, fn func(dmTxStore) error) error {
	return fn(r.store)
}

func newDMTestServer(store *dmTestStore, user db.User) *echo.Echo {
	h := &DMHandler{Store: store, Run: dmTestRunner{store: store}, TTL: 24 * time.Hour}
	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: user},
	}))
	h.RegisterRoutes(e.Group("/api"), DMRateLimits{})
	return e
}

func dmRequest(e *echo.Echo, method, path string, body any, authenticated bool) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if authenticated {
		req.Header.Set(echo.HeaderAuthorization, "Bearer test")
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestDMAuthRequired(t *testing.T) {
	store := &dmTestStore{}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodGet, "/api/dm/inbox?device_id="+uuid.NewString()+"&after=0", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDMRouteRateLimitsRunAfterAuthentication(t *testing.T) {
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:       db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-v1", ProtocolVersion: 1},
		updateDevice: db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		bundleDevice: db.DmDevice{
			ID: recipientDeviceID, UserID: testOwnerID, Protocol: "custom-v1", ProtocolVersion: 1,
			FallbackKeyID:       sql.NullString{String: "fallback", Valid: true},
			FallbackKeyMaterial: []byte("opaque-key"),
		},
		claimErr: sql.ErrNoRows, existingErr: sql.ErrNoRows, nextSequence: 1,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-v1", ProtocolVersion: 1,
		},
	}
	limit, err := middleware.NewRateLimitAuth(middleware.RateLimitConfig{
		Rate: rate.Limit(100), Burst: 100, TTL: time.Minute, SweepInterval: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &DMHandler{Store: store, Run: dmTestRunner{store: store}, TTL: time.Hour}
	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	h.RegisterRoutes(e.Group("/api"), DMRateLimits{KeyBundle: limit, Send: limit})
	bundle := dmRequest(e, http.MethodGet,
		"/api/dm/users/"+testOwnerID.String()+"/key-bundle?device_id="+senderDeviceID.String(), nil, true)
	if bundle.Code != http.StatusOK {
		t.Fatalf("key-bundle status = %d, want 200; body=%s", bundle.Code, bundle.Body.String())
	}
	send := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-v1", Version: 1, ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
	}, true)
	if send.Code != http.StatusCreated {
		t.Fatalf("send status = %d, want 201; body=%s", send.Code, send.Body.String())
	}
}

func TestDMRegistrationRejectsKeyAndCountLimits(t *testing.T) {
	tests := []struct {
		name string
		req  dmRegisterDeviceRequest
	}{
		{name: "empty key bundle", req: dmRegisterDeviceRequest{Protocol: "custom-protocol", Version: 9}},
		{name: "invalid protocol identifier", req: dmRegisterDeviceRequest{Protocol: "custom protocol", Version: 9, KeyBundle: []byte("opaque")}},
		{name: "too many keys", req: dmRegisterDeviceRequest{
			Protocol: "custom-protocol", Version: 9, KeyBundle: []byte("opaque"),
			OneTimeKeys: make([]dmOneTimeKeyInput, dmMaxOneTimeKeysPerBatch+1),
		}},
		{name: "duplicate key ids", req: dmRegisterDeviceRequest{
			Protocol: "custom-protocol", Version: 9, KeyBundle: []byte("opaque"),
			OneTimeKeys: []dmOneTimeKeyInput{
				{KeyID: "same", KeyMaterial: []byte("first opaque key")},
				{KeyID: "same", KeyMaterial: []byte("second opaque key")},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &dmTestStore{}
			e := newDMTestServer(store, viewerUser())
			rec := dmRequest(e, http.MethodPost, "/api/dm/device", tt.req, true)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if store.registered.ID != uuid.Nil {
				t.Fatal("invalid registration reached the store")
			}
		})
	}
}

func TestDMRegisterDeviceStoresKeysWithoutEchoingKeyMaterial(t *testing.T) {
	keyBundle := bytes.Repeat([]byte{0xa1}, 57)
	oneTimeKey := bytes.Repeat([]byte{0xc3}, 43)
	store := &dmTestStore{}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/device", dmRegisterDeviceRequest{
		Protocol: "not-an-allowlisted-protocol", Version: 32768,
		KeyBundle:   keyBundle,
		OneTimeKeys: []dmOneTimeKeyInput{{KeyID: "key-1", KeyMaterial: oneTimeKey}},
	}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	for _, key := range [][]byte{keyBundle, oneTimeKey} {
		if strings.Contains(rec.Body.String(), base64Encode(key)) {
			t.Fatal("registration response exposed key material")
		}
	}
	if store.insertedKeys != 1 {
		t.Fatalf("inserted key count = %d, want 1", store.insertedKeys)
	}
}

func TestDMKeyBundleFallsBackWhenOneTimeKeysAreExhausted(t *testing.T) {
	targetID := uuid.New()
	deviceID := uuid.New()
	senderDeviceID := uuid.New()
	store := &dmTestStore{
		device:       db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		updateDevice: db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		bundleDevice: db.DmDevice{
			ID: deviceID, UserID: targetID, Protocol: "custom-protocol",
			ProtocolVersion: 32768, KeyBundle: []byte("opaque key bundle"),
			FallbackKeyID:       sql.NullString{String: "fallback-1", Valid: true},
			FallbackKeyMaterial: []byte("opaque fallback material"),
		},
		claimErr: sql.ErrNoRows,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodGet, "/api/dm/users/"+targetID.String()+"/key-bundle?device_id="+senderDeviceID.String(), nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fallback_key"`) {
		t.Fatalf("status/body = %d/%s, want fallback bundle", rec.Code, rec.Body.String())
	}
	store.bundleDevice.FallbackKeyMaterial = nil
	store.bundleDevice.FallbackKeyID = sql.NullString{}
	rec = dmRequest(e, http.MethodGet, "/api/dm/users/"+targetID.String()+"/key-bundle?device_id="+senderDeviceID.String(), nil, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_KEYS_EXHAUSTED"`) {
		t.Fatalf("status/body = %d/%s, want DM_KEYS_EXHAUSTED", rec.Code, rec.Body.String())
	}
}

func TestDMBundleUsesSameNotFoundForMissingAndIneligibleUsers(t *testing.T) {
	senderDeviceID := uuid.New()
	store := &dmTestStore{
		device:       db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		updateDevice: db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		bundleErr:    sql.ErrNoRows,
	}
	e := newDMTestServer(store, viewerUser())
	first := dmRequest(e, http.MethodGet, "/api/dm/users/"+uuid.NewString()+"/key-bundle?device_id="+senderDeviceID.String(), nil, true)
	second := dmRequest(e, http.MethodGet, "/api/dm/users/"+uuid.NewString()+"/key-bundle?device_id="+senderDeviceID.String(), nil, true)
	if first.Code != http.StatusNotFound || second.Code != first.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("recipient responses differ: %d %q / %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}

func TestDMIneligibleAndMissingRecipientsHaveSameResponse(t *testing.T) {
	senderDeviceID := uuid.New()
	store := &dmTestStore{
		device:       db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		recipientErr: sql.ErrNoRows,
		bundleErr:    sql.ErrNoRows,
		existingErr:  sql.ErrNoRows,
	}
	e := newDMTestServer(store, viewerUser())
	makeSend := func(recipient uuid.UUID) dmSendRequest {
		return dmSendRequest{
			SenderDeviceID: senderDeviceID, RecipientDeviceID: uuid.New(),
			RecipientUserID: recipient, Protocol: "custom-protocol", Version: 1,
			ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
		}
	}
	ineligible := dmRequest(e, http.MethodPost, "/api/dm/messages", makeSend(uuid.New()), true)
	missing := dmRequest(e, http.MethodPost, "/api/dm/messages", makeSend(uuid.New()), true)
	if ineligible.Code != http.StatusNotFound || missing.Code != ineligible.Code {
		t.Fatalf("statuses = %d/%d, want both 404", ineligible.Code, missing.Code)
	}
	var first, second map[string]any
	if err := json.Unmarshal(ineligible.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if first["error"] == nil || first["error"].(map[string]any)["code"] != second["error"].(map[string]any)["code"] {
		t.Fatalf("error codes differ: %v / %v", first, second)
	}
}

func TestDMUploadOneTimeKeysEnforcesStoredCountLimit(t *testing.T) {
	store := &dmTestStore{
		updateDevice: db.DmDevice{ID: uuid.New(), UserID: testViewerID},
		count:        dmMaxOneTimeKeysStored,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/device/keys", dmUploadKeysRequest{
		DeviceID:    store.updateDevice.ID,
		OneTimeKeys: []dmOneTimeKeyInput{{KeyID: "overflow", KeyMaterial: []byte("opaque key")}},
	}, true)
	if rec.Code != http.StatusBadRequest || store.insertedKeys != 0 {
		t.Fatalf("status/insert count = %d/%d, want 400/0; body=%s", rec.Code, store.insertedKeys, rec.Body.String())
	}
}

func TestDMSendIdempotencyAndConflict(t *testing.T) {
	clientID := uuid.New()
	messageID := uuid.New()
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	ciphertext := []byte("opaque-ciphertext")
	ciphertextDigest := sha256.Sum256(ciphertext)
	store := &dmTestStore{
		device: db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-v1", ProtocolVersion: 4},
		existing: db.GetDMSendIdempotencyByClientIDRow{
			MessageID: messageID, RecipientDeviceID: recipientDeviceID, RecipientUserID: testOwnerID, SequenceNo: 7,
			Protocol: "custom-v1", Version: 4, CiphertextDigest: ciphertextDigest[:],
			CreatedAt: time.Unix(2, 0).UTC(), ExpiresAt: time.Unix(2, 0).UTC().Add(time.Hour),
		},
	}
	e := newDMTestServer(store, viewerUser())
	req := dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-v1", Version: 4,
		ClientMessageID: clientID, Ciphertext: ciphertext,
	}
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", req, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), messageID.String()) ||
		store.insertedMessages != 0 || store.insertedIdempotency != 0 {
		t.Fatalf("status/body = %d/%s, want existing idempotent result", rec.Code, rec.Body.String())
	}
	req.Ciphertext = []byte("different-ciphertext")
	rec = dmRequest(e, http.MethodPost, "/api/dm/messages", req, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_CONFLICT"`) {
		t.Fatalf("status/body = %d/%s, want idempotency conflict", rec.Code, rec.Body.String())
	}
}

func TestDMSendResolvesConcurrentIdempotencyReservation(t *testing.T) {
	clientID := uuid.New()
	messageID := uuid.New()
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	ciphertext := []byte("opaque-ciphertext")
	ciphertextDigest := sha256.Sum256(ciphertext)
	existing := db.GetDMSendIdempotencyByClientIDRow{
		MessageID: messageID, RecipientDeviceID: recipientDeviceID, RecipientUserID: testOwnerID,
		SequenceNo: 7, Protocol: "custom-v1", Version: 4, CiphertextDigest: ciphertextDigest[:],
		CreatedAt: time.Unix(2, 0).UTC(), ExpiresAt: time.Unix(2, 0).UTC().Add(time.Hour),
	}
	store := &dmTestStore{
		device:               db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-v1", ProtocolVersion: 4},
		existingErr:          sql.ErrNoRows,
		idempotencyInsertErr: sql.ErrNoRows,
		concurrentExisting:   &existing,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-v1", ProtocolVersion: 4,
		},
		nextSequence: 8,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-v1", Version: 4, ClientMessageID: clientID, Ciphertext: ciphertext,
	}, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), messageID.String()) ||
		store.insertedMessages != 0 {
		t.Fatalf("status/body/insertions = %d/%s/%d, want concurrent idempotent result without another inbox row",
			rec.Code, rec.Body.String(), store.insertedMessages)
	}
}

func TestDMSendAllocatesAndReturnsRecipientSequence(t *testing.T) {
	clientID := uuid.New()
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 32768},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-protocol", ProtocolVersion: 32768,
		},
		nextSequence: 11,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-protocol", Version: 32768, ClientMessageID: clientID,
		Ciphertext: []byte("opaque"),
	}, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"sequence":11`) {
		t.Fatalf("status/body = %d/%s, want allocated sequence 11", rec.Code, rec.Body.String())
	}
}

func TestDMSendRejectsProtocolVersionMismatch(t *testing.T) {
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "sender-protocol", ProtocolVersion: 3},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "recipient-protocol", ProtocolVersion: 3,
		},
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "sender-protocol", Version: 3,
		ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
	}, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_PROTOCOL_MISMATCH"`) {
		t.Fatalf("status/body = %d/%s, want protocol mismatch", rec.Code, rec.Body.String())
	}
}

func TestDMPollPreservesSequenceOrderAndAckTargetsCurrentDevice(t *testing.T) {
	deviceID := uuid.New()
	firstID, secondID := uuid.New(), uuid.New()
	store := &dmTestStore{
		device: db.DmDevice{ID: deviceID, UserID: testViewerID},
		inboxRows: []db.ListDMInboxAfterRow{
			{ID: firstID, SequenceNo: 2},
			{ID: secondID, SequenceNo: 5},
		},
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodGet, "/api/dm/inbox?device_id="+deviceID.String()+"&after=0", nil, true)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"next_after":5`) ||
		strings.Index(rec.Body.String(), firstID.String()) > strings.Index(rec.Body.String(), secondID.String()) {
		t.Fatalf("poll response does not preserve sequence order: %d %s", rec.Code, rec.Body.String())
	}
	rec = dmRequest(e, http.MethodPost, "/api/dm/inbox/"+firstID.String()+"/ack", dmAckRequest{DeviceID: deviceID}, true)
	if rec.Code != http.StatusNoContent || store.lastAck.ID != firstID || store.lastAck.RecipientDeviceID != deviceID {
		t.Fatalf("ack status/target = %d/%+v", rec.Code, store.lastAck)
	}
	rec = dmRequest(e, http.MethodPost, "/api/dm/inbox/"+firstID.String()+"/ack", dmAckRequest{DeviceID: deviceID}, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("repeated ack status = %d, want 204", rec.Code)
	}
}

func TestDMRelayDoesNotLogCiphertextOrKeyBytes(t *testing.T) {
	ciphertext := []byte("distinct-secret-ciphertext")
	keyBundle := []byte("distinct-secret-key-bundle")
	fallbackKey := []byte("distinct-secret-fallback-key")
	oneTimeKey := []byte("distinct-secret-one-time-key")
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 12},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-protocol", ProtocolVersion: 12,
		},
		nextSequence: 1,
		inboxRows: []db.ListDMInboxAfterRow{{
			ID: uuid.New(), SequenceNo: 1, Protocol: "custom-protocol", Version: 12, Ciphertext: ciphertext,
		}},
	}
	e := newDMTestServer(store, viewerUser())
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	register := dmRequest(e, http.MethodPost, "/api/dm/device", dmRegisterDeviceRequest{
		Protocol: "custom-protocol", Version: 12,
		KeyBundle:           keyBundle,
		FallbackKeyID:       "fallback",
		FallbackKeyMaterial: fallbackKey,
		OneTimeKeys:         []dmOneTimeKeyInput{{KeyID: "one-time", KeyMaterial: oneTimeKey}},
	}, true)
	if register.Code != http.StatusCreated {
		t.Fatalf("registration status = %d: %s", register.Code, register.Body.String())
	}
	send := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-protocol", Version: 12, ClientMessageID: uuid.New(),
		Ciphertext: ciphertext,
	}, true)
	if send.Code != http.StatusCreated {
		t.Fatalf("send status = %d: %s", send.Code, send.Body.String())
	}
	poll := dmRequest(e, http.MethodGet, "/api/dm/inbox?device_id="+senderDeviceID.String()+"&after=0", nil, true)
	if poll.Code != http.StatusOK {
		t.Fatalf("poll status = %d: %s", poll.Code, poll.Body.String())
	}
	for _, secret := range []string{
		string(ciphertext), base64Encode(ciphertext),
		string(keyBundle), base64Encode(keyBundle),
		string(fallbackKey), base64Encode(fallbackKey),
		string(oneTimeKey), base64Encode(oneTimeKey),
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("sensitive bytes appeared in logs: %q", secret)
		}
	}
}

func TestDMPollRejectsInvalidLimits(t *testing.T) {
	deviceID := uuid.New()
	store := &dmTestStore{device: db.DmDevice{ID: deviceID, UserID: testViewerID}}
	e := newDMTestServer(store, viewerUser())
	for _, path := range []string{
		"/api/dm/inbox?device_id=" + deviceID.String(),
		"/api/dm/inbox?device_id=" + deviceID.String() + "&after=-1",
		"/api/dm/inbox?device_id=" + deviceID.String() + "&after=0&limit=101",
	} {
		rec := dmRequest(e, http.MethodGet, path, nil, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rec.Code)
		}
	}
}

func TestDMSendRejectsOversizedCiphertext(t *testing.T) {
	senderDeviceID := uuid.New()
	store := &dmTestStore{}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientDeviceID: uuid.New(),
		RecipientUserID: testOwnerID, Protocol: "custom-protocol", Version: 1, ClientMessageID: uuid.New(),
		Ciphertext: make([]byte, dmMaxCiphertextBytes+1),
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDMSendAcceptsMaximumCiphertext(t *testing.T) {
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 1},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-protocol", ProtocolVersion: 1,
		},
		nextSequence: 1,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-protocol", Version: 1,
		ClientMessageID: uuid.New(), Ciphertext: make([]byte, dmMaxCiphertextBytes),
	}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDMSendRejectsFullRecipientInbox(t *testing.T) {
	senderDeviceID := uuid.New()
	recipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID, Protocol: "custom-v1", ProtocolVersion: 1},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: recipientDeviceID, Protocol: "custom-v1", ProtocolVersion: 1,
		},
		nextSequence:    11,
		pendingMessages: dmMaxPendingPerDevice,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientUserID: testOwnerID, RecipientDeviceID: recipientDeviceID,
		Protocol: "custom-v1", Version: 1, ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
	}, true)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"DM_INBOX_FULL"`) ||
		store.insertedMessages != 0 {
		t.Fatalf("status/body/insertions = %d/%s/%d, want 429/DM_INBOX_FULL/0", rec.Code, rec.Body.String(), store.insertedMessages)
	}
}

func TestDMDeviceBoundRoutesRejectStaleDeviceIDs(t *testing.T) {
	activeDeviceID := uuid.New()
	staleDeviceID := uuid.New()
	store := &dmTestStore{
		device:       db.DmDevice{ID: activeDeviceID, UserID: testViewerID},
		updateDevice: db.DmDevice{ID: activeDeviceID, UserID: testViewerID},
	}
	e := newDMTestServer(store, viewerUser())
	tests := []struct {
		name string
		call func() *httptest.ResponseRecorder
	}{
		{
			name: "upload one-time keys",
			call: func() *httptest.ResponseRecorder {
				return dmRequest(e, http.MethodPost, "/api/dm/device/keys", dmUploadKeysRequest{
					DeviceID: staleDeviceID,
				}, true)
			},
		},
		{
			name: "fetch key bundle",
			call: func() *httptest.ResponseRecorder {
				return dmRequest(e, http.MethodGet, "/api/dm/users/"+uuid.NewString()+"/key-bundle?device_id="+staleDeviceID.String(), nil, true)
			},
		},
		{
			name: "poll inbox",
			call: func() *httptest.ResponseRecorder {
				return dmRequest(e, http.MethodGet, "/api/dm/inbox?device_id="+staleDeviceID.String()+"&after=0", nil, true)
			},
		},
		{
			name: "ack inbox message",
			call: func() *httptest.ResponseRecorder {
				return dmRequest(e, http.MethodPost, "/api/dm/inbox/"+uuid.NewString()+"/ack", dmAckRequest{
					DeviceID: staleDeviceID,
				}, true)
			},
		},
		{
			name: "send from stale device",
			call: func() *httptest.ResponseRecorder {
				return dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
					SenderDeviceID: staleDeviceID, RecipientDeviceID: uuid.New(),
					RecipientUserID: testOwnerID, Protocol: "custom-v1", Version: 1,
					ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
				}, true)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.call()
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_DEVICE_STALE"`) {
				t.Fatalf("status/body = %d/%s, want 409/DM_DEVICE_STALE", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDMSendRejectsStaleRecipientDevice(t *testing.T) {
	senderDeviceID := uuid.New()
	activeRecipientDeviceID := uuid.New()
	staleRecipientDeviceID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: senderDeviceID, UserID: testViewerID},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: activeRecipientDeviceID, Protocol: "custom-v1", ProtocolVersion: 1,
		},
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		SenderDeviceID: senderDeviceID, RecipientDeviceID: staleRecipientDeviceID,
		RecipientUserID: testOwnerID, Protocol: "custom-v1", Version: 1,
		ClientMessageID: uuid.New(), Ciphertext: []byte("opaque"),
	}, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_DEVICE_STALE"`) ||
		store.insertedMessages != 0 {
		t.Fatalf("status/body/insertions = %d/%s/%d, want 409/DM_DEVICE_STALE/0", rec.Code, rec.Body.String(), store.insertedMessages)
	}
}

func TestDMKeyIDsMustBeUniquePerRequest(t *testing.T) {
	duplicate := []dmOneTimeKeyInput{
		{KeyID: "one", KeyMaterial: []byte("first opaque key")},
		{KeyID: "two", KeyMaterial: []byte("second opaque key")},
	}
	if err := validateDMKeys(duplicate); err != nil {
		t.Fatalf("distinct key ids rejected: %v", err)
	}
	duplicate[1].KeyID = duplicate[0].KeyID
	if err := validateDMKeys(duplicate); err == nil {
		t.Fatal("duplicate key ids accepted")
	}
}

func base64Encode(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

var _ dmStore = (*dmTestStore)(nil)
