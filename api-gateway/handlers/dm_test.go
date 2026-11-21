package handlers

import (
	"bytes"
	"context"
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

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type dmTestStore struct {
	device          db.DmDevice
	deviceErr       error
	updateDevice    db.DmDevice
	updateDeviceErr error
	bundleDevice    db.DmDevice
	bundleErr       error
	claimedKey      db.ClaimDMOneTimeKeyRow
	claimErr        error
	recipientDevice db.GetEligibleDMRecipientDeviceForSendRow
	recipientErr    error
	existing        db.GetDMInboxMessageByClientIDRow
	existingErr     error
	inserted        db.InsertDMInboxMessageRow
	insertErr       error
	nextSequence    int64
	inboxRows       []db.ListDMInboxAfterRow
	ackRows         int64
	deleteExpired   int64
	expirySwept     bool
	registered      db.DmDevice
	count           int64
	insertedKeys    int
	lastAck         db.AckDMInboxMessageParams
}

func (s *dmTestStore) RevokeDMDeviceForUser(context.Context, uuid.UUID) error { return nil }
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
func (s *dmTestStore) GetDMDeviceByUser(context.Context, uuid.UUID) (db.DmDevice, error) {
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
func (s *dmTestStore) GetDMInboxMessageByClientID(context.Context, db.GetDMInboxMessageByClientIDParams) (db.GetDMInboxMessageByClientIDRow, error) {
	return s.existing, s.existingErr
}
func (s *dmTestStore) InsertDMInboxMessage(_ context.Context, arg db.InsertDMInboxMessageParams) (db.InsertDMInboxMessageRow, error) {
	if s.insertErr != nil {
		return s.inserted, s.insertErr
	}
	return db.InsertDMInboxMessageRow{
		ID: uuid.New(), SequenceNo: arg.SequenceNo,
		ClientMessageID: arg.ClientMessageID,
		CreatedAt:       time.Unix(2, 0).UTC(),
		ExpiresAt:       time.Unix(2, 0).UTC().Add(time.Duration(arg.ExpiresInSeconds) * time.Second),
	}, nil
}
func (s *dmTestStore) ListDMInboxAfter(context.Context, db.ListDMInboxAfterParams) ([]db.ListDMInboxAfterRow, error) {
	return s.inboxRows, nil
}
func (s *dmTestStore) AckDMInboxMessage(_ context.Context, arg db.AckDMInboxMessageParams) (int64, error) {
	s.lastAck = arg
	return s.ackRows, nil
}
func (s *dmTestStore) DeleteExpiredDMInbox(context.Context) (int64, error) {
	s.expirySwept = true
	return s.deleteExpired, nil
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
	h.RegisterRoutes(e.Group("/api"))
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
	rec := dmRequest(e, http.MethodGet, "/api/dm/inbox?after=0", nil, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
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
	store := &dmTestStore{
		bundleDevice: db.DmDevice{
			ID: deviceID, UserID: targetID, Protocol: "custom-protocol",
			ProtocolVersion: 32768, KeyBundle: []byte("opaque key bundle"),
			FallbackKeyID:       sql.NullString{String: "fallback-1", Valid: true},
			FallbackKeyMaterial: []byte("opaque fallback material"),
		},
		claimErr: sql.ErrNoRows,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodGet, "/api/dm/users/"+targetID.String()+"/key-bundle", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"fallback_key"`) {
		t.Fatalf("status/body = %d/%s, want fallback bundle", rec.Code, rec.Body.String())
	}
	store.bundleDevice.FallbackKeyMaterial = nil
	store.bundleDevice.FallbackKeyID = sql.NullString{}
	rec = dmRequest(e, http.MethodGet, "/api/dm/users/"+targetID.String()+"/key-bundle", nil, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_KEYS_EXHAUSTED"`) {
		t.Fatalf("status/body = %d/%s, want DM_KEYS_EXHAUSTED", rec.Code, rec.Body.String())
	}
}

func TestDMBundleUsesSameNotFoundForMissingAndIneligibleUsers(t *testing.T) {
	store := &dmTestStore{bundleErr: sql.ErrNoRows}
	e := newDMTestServer(store, viewerUser())
	first := dmRequest(e, http.MethodGet, "/api/dm/users/"+uuid.NewString()+"/key-bundle", nil, true)
	second := dmRequest(e, http.MethodGet, "/api/dm/users/"+uuid.NewString()+"/key-bundle", nil, true)
	if first.Code != http.StatusNotFound || second.Code != first.Code || first.Body.String() != second.Body.String() {
		t.Fatalf("recipient responses differ: %d %q / %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}

func TestDMIneligibleAndMissingRecipientsHaveSameResponse(t *testing.T) {
	store := &dmTestStore{
		device:       db.DmDevice{ID: uuid.New(), UserID: testViewerID},
		recipientErr: sql.ErrNoRows,
		bundleErr:    sql.ErrNoRows,
		existingErr:  sql.ErrNoRows,
	}
	e := newDMTestServer(store, viewerUser())
	makeSend := func(recipient uuid.UUID) dmSendRequest {
		return dmSendRequest{
			RecipientUserID: recipient, Protocol: "custom-protocol", Version: 1, ClientMessageID: uuid.New(),
			Ciphertext: []byte("opaque"),
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
		OneTimeKeys: []dmOneTimeKeyInput{{KeyID: "overflow", KeyMaterial: []byte("opaque key")}},
	}, true)
	if rec.Code != http.StatusBadRequest || store.insertedKeys != 0 {
		t.Fatalf("status/insert count = %d/%d, want 400/0; body=%s", rec.Code, store.insertedKeys, rec.Body.String())
	}
}

func TestDMSendIdempotencyAndConflict(t *testing.T) {
	clientID := uuid.New()
	messageID := uuid.New()
	ciphertext := []byte("opaque-ciphertext")
	store := &dmTestStore{
		device: db.DmDevice{ID: uuid.New(), UserID: testViewerID, Protocol: "custom-v1", ProtocolVersion: 4},
		existing: db.GetDMInboxMessageByClientIDRow{
			ID: messageID, RecipientUserID: testOwnerID, SequenceNo: 7,
			Protocol: "custom-v1", Version: 4, Ciphertext: ciphertext, ClientMessageID: clientID,
		},
	}
	e := newDMTestServer(store, viewerUser())
	req := dmSendRequest{
		RecipientUserID: testOwnerID, Protocol: "custom-v1", Version: 4,
		ClientMessageID: clientID, Ciphertext: ciphertext,
	}
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", req, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), messageID.String()) {
		t.Fatalf("status/body = %d/%s, want existing idempotent result", rec.Code, rec.Body.String())
	}
	req.Ciphertext = []byte("different-ciphertext")
	rec = dmRequest(e, http.MethodPost, "/api/dm/messages", req, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"DM_CONFLICT"`) {
		t.Fatalf("status/body = %d/%s, want idempotency conflict", rec.Code, rec.Body.String())
	}
}

func TestDMSendAllocatesAndReturnsRecipientSequence(t *testing.T) {
	clientID := uuid.New()
	store := &dmTestStore{
		device:      db.DmDevice{ID: uuid.New(), UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 32768},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: uuid.New(), Protocol: "custom-protocol", ProtocolVersion: 32768,
		},
		nextSequence: 11,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		RecipientUserID: testOwnerID, Protocol: "custom-protocol", Version: 32768, ClientMessageID: clientID,
		Ciphertext: []byte("opaque"),
	}, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"sequence":11`) {
		t.Fatalf("status/body = %d/%s, want allocated sequence 11", rec.Code, rec.Body.String())
	}
}

func TestDMSendRejectsProtocolVersionMismatch(t *testing.T) {
	store := &dmTestStore{
		device:      db.DmDevice{ID: uuid.New(), UserID: testViewerID, Protocol: "sender-protocol", ProtocolVersion: 3},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: uuid.New(), Protocol: "recipient-protocol", ProtocolVersion: 3,
		},
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		RecipientUserID: testOwnerID, Protocol: "sender-protocol", Version: 3,
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
	rec := dmRequest(e, http.MethodGet, "/api/dm/inbox?after=0", nil, true)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"next_after":5`) ||
		strings.Index(rec.Body.String(), firstID.String()) > strings.Index(rec.Body.String(), secondID.String()) {
		t.Fatalf("poll response does not preserve sequence order: %d %s", rec.Code, rec.Body.String())
	}
	rec = dmRequest(e, http.MethodPost, "/api/dm/inbox/"+firstID.String()+"/ack", nil, true)
	if rec.Code != http.StatusNoContent || store.lastAck.ID != firstID || store.lastAck.RecipientDeviceID != deviceID {
		t.Fatalf("ack status/target = %d/%+v", rec.Code, store.lastAck)
	}
}

func TestDMPollSweepsExpiredMessages(t *testing.T) {
	store := &dmTestStore{device: db.DmDevice{ID: uuid.New(), UserID: testViewerID}}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodGet, "/api/dm/inbox?after=0", nil, true)
	if rec.Code != http.StatusOK || !store.expirySwept {
		t.Fatalf("status/sweep = %d/%t, want 200/true; body=%s", rec.Code, store.expirySwept, rec.Body.String())
	}
}

func TestDMRelayDoesNotLogCiphertextOrKeyBytes(t *testing.T) {
	ciphertext := []byte("distinct-secret-ciphertext")
	keyBundle := []byte("distinct-secret-key-bundle")
	fallbackKey := []byte("distinct-secret-fallback-key")
	oneTimeKey := []byte("distinct-secret-one-time-key")
	store := &dmTestStore{
		device:      db.DmDevice{ID: uuid.New(), UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 12},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: uuid.New(), Protocol: "custom-protocol", ProtocolVersion: 12,
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
		RecipientUserID: testOwnerID, Protocol: "custom-protocol", Version: 12, ClientMessageID: uuid.New(),
		Ciphertext: ciphertext,
	}, true)
	if send.Code != http.StatusCreated {
		t.Fatalf("send status = %d: %s", send.Code, send.Body.String())
	}
	poll := dmRequest(e, http.MethodGet, "/api/dm/inbox?after=0", nil, true)
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
	store := &dmTestStore{}
	e := newDMTestServer(store, viewerUser())
	for _, path := range []string{
		"/api/dm/inbox",
		"/api/dm/inbox?after=-1",
		"/api/dm/inbox?after=0&limit=101",
	} {
		rec := dmRequest(e, http.MethodGet, path, nil, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rec.Code)
		}
	}
}

func TestDMSendRejectsOversizedCiphertext(t *testing.T) {
	store := &dmTestStore{}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		RecipientUserID: testOwnerID, Protocol: "custom-protocol", Version: 1, ClientMessageID: uuid.New(),
		Ciphertext: make([]byte, dmMaxCiphertextBytes+1),
	}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDMSendAcceptsMaximumCiphertext(t *testing.T) {
	store := &dmTestStore{
		device:      db.DmDevice{ID: uuid.New(), UserID: testViewerID, Protocol: "custom-protocol", ProtocolVersion: 1},
		existingErr: sql.ErrNoRows,
		recipientDevice: db.GetEligibleDMRecipientDeviceForSendRow{
			ID: uuid.New(), Protocol: "custom-protocol", ProtocolVersion: 1,
		},
		nextSequence: 1,
	}
	e := newDMTestServer(store, viewerUser())
	rec := dmRequest(e, http.MethodPost, "/api/dm/messages", dmSendRequest{
		RecipientUserID: testOwnerID, Protocol: "custom-protocol", Version: 1,
		ClientMessageID: uuid.New(), Ciphertext: make([]byte, dmMaxCiphertextBytes),
	}, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
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
