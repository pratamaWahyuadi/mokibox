package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type dmFakeStore struct {
	devices      map[uuid.UUID]db.DmDevice
	keys         map[uuid.UUID][]db.ClaimDMOneTimeKeyRow
	keyIDs       map[uuid.UUID]map[string]bool
	fallbackKeys map[uuid.UUID]db.DmFallbackKey
	idempotency  map[dmFakeIdemKey]db.GetDMIdempotencyRow
	inbox        map[uuid.UUID]db.ListPendingDMInboxRow
	acknowledged map[uuid.UUID]bool
	blocks       map[uuid.UUID]map[uuid.UUID]bool
	pairPurges   int
	eligible     bool
	pending      int64
}

type dmFakeIdemKey struct {
	deviceID uuid.UUID
	clientID uuid.UUID
}

func newDMFakeStore() *dmFakeStore {
	return &dmFakeStore{
		devices:      make(map[uuid.UUID]db.DmDevice),
		keys:         make(map[uuid.UUID][]db.ClaimDMOneTimeKeyRow),
		keyIDs:       make(map[uuid.UUID]map[string]bool),
		fallbackKeys: make(map[uuid.UUID]db.DmFallbackKey),
		idempotency:  make(map[dmFakeIdemKey]db.GetDMIdempotencyRow),
		inbox:        make(map[uuid.UUID]db.ListPendingDMInboxRow),
		acknowledged: make(map[uuid.UUID]bool),
		blocks:       make(map[uuid.UUID]map[uuid.UUID]bool),
	}
}

func (s *dmFakeStore) GetDMDeviceByUser(_ context.Context, userID uuid.UUID) (db.DmDevice, error) {
	device, ok := s.devices[userID]
	if !ok {
		return db.DmDevice{}, sql.ErrNoRows
	}
	return device, nil
}

func (s *dmFakeStore) GetDMDeviceByUserAndID(_ context.Context, arg db.GetDMDeviceByUserAndIDParams) (db.DmDevice, error) {
	device, ok := s.devices[arg.UserID]
	if !ok || device.DeviceID != arg.DeviceID {
		return db.DmDevice{}, sql.ErrNoRows
	}
	return device, nil
}

func (s *dmFakeStore) GetDMDeviceOwner(_ context.Context, deviceID uuid.UUID) (uuid.UUID, error) {
	for userID, device := range s.devices {
		if device.DeviceID == deviceID {
			return userID, nil
		}
	}
	return uuid.Nil, sql.ErrNoRows
}

func (s *dmFakeStore) GetDMTargetDevice(_ context.Context, userID uuid.UUID) (db.DmDevice, error) {
	return s.GetDMDeviceByUser(context.Background(), userID)
}

func (s *dmFakeStore) IsDMMutualFollow(_ context.Context, _ db.IsDMMutualFollowParams) (bool, error) {
	return s.eligible, nil
}

func (s *dmFakeStore) InsertDMOneTimeKey(_ context.Context, arg db.InsertDMOneTimeKeyParams) (int64, error) {
	if s.keyIDs[arg.DeviceID] == nil {
		s.keyIDs[arg.DeviceID] = make(map[string]bool)
	}
	if s.keyIDs[arg.DeviceID][arg.KeyID] {
		return 0, nil
	}
	s.keyIDs[arg.DeviceID][arg.KeyID] = true
	s.keys[arg.DeviceID] = append(s.keys[arg.DeviceID], db.ClaimDMOneTimeKeyRow{
		KeyID: arg.KeyID, PublicKey: arg.PublicKey, Signature: arg.Signature,
	})
	return 1, nil
}

func (s *dmFakeStore) UpsertDMFallbackKey(_ context.Context, arg db.UpsertDMFallbackKeyParams) (db.DmFallbackKey, error) {
	now := time.Now()
	key := db.DmFallbackKey{
		DeviceID: arg.DeviceID, KeyID: arg.KeyID, PublicKey: arg.PublicKey,
		Signature: arg.Signature, CreatedAt: now, UpdatedAt: now,
	}
	if old, ok := s.fallbackKeys[arg.DeviceID]; ok {
		key.CreatedAt = old.CreatedAt
	}
	s.fallbackKeys[arg.DeviceID] = key
	return key, nil
}

func (s *dmFakeStore) GetDMFallbackKey(_ context.Context, deviceID uuid.UUID) (db.DmFallbackKey, error) {
	key, ok := s.fallbackKeys[deviceID]
	if !ok {
		return db.DmFallbackKey{}, sql.ErrNoRows
	}
	return key, nil
}

func (s *dmFakeStore) ClaimDMOneTimeKey(_ context.Context, deviceID uuid.UUID) (db.ClaimDMOneTimeKeyRow, error) {
	if len(s.keys[deviceID]) == 0 {
		return db.ClaimDMOneTimeKeyRow{}, sql.ErrNoRows
	}
	key := s.keys[deviceID][0]
	s.keys[deviceID] = s.keys[deviceID][1:]
	return key, nil
}

func (s *dmFakeStore) GetDMIdempotency(_ context.Context, arg db.GetDMIdempotencyParams) (db.GetDMIdempotencyRow, error) {
	record, ok := s.idempotency[dmFakeIdemKey{deviceID: arg.SenderDeviceID, clientID: arg.ClientMessageID}]
	if !ok {
		return db.GetDMIdempotencyRow{}, sql.ErrNoRows
	}
	return record, nil
}

func (s *dmFakeStore) ListPendingDMInbox(_ context.Context, arg db.ListPendingDMInboxParams) ([]db.ListPendingDMInboxRow, error) {
	var rows []db.ListPendingDMInboxRow
	for _, row := range s.inbox {
		if row.RecipientDeviceID == arg.RecipientDeviceID && row.Sequence > arg.Sequence && !s.acknowledged[row.MessageID] {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (s *dmFakeStore) AcknowledgeDMInboxMessage(_ context.Context, arg db.AcknowledgeDMInboxMessageParams) (int64, error) {
	row, ok := s.inbox[arg.MessageID]
	if !ok || row.RecipientDeviceID != arg.RecipientDeviceID || s.acknowledged[arg.MessageID] {
		return 0, nil
	}
	row.Ciphertext = nil
	s.inbox[arg.MessageID] = row
	s.acknowledged[arg.MessageID] = true
	return 1, nil
}

func (s *dmFakeStore) GetDMInboxMessageForDevice(_ context.Context, arg db.GetDMInboxMessageForDeviceParams) (uuid.UUID, error) {
	row, ok := s.inbox[arg.MessageID]
	if !ok || row.RecipientDeviceID != arg.RecipientDeviceID {
		return uuid.Nil, sql.ErrNoRows
	}
	return row.MessageID, nil
}

func (s *dmFakeStore) InsertDMBlock(_ context.Context, arg db.InsertDMBlockParams) (int64, error) {
	if s.blocks[arg.BlockerID] == nil {
		s.blocks[arg.BlockerID] = make(map[uuid.UUID]bool)
	}
	if s.blocks[arg.BlockerID][arg.BlockedID] {
		return 0, nil
	}
	s.blocks[arg.BlockerID][arg.BlockedID] = true
	return 1, nil
}

func (s *dmFakeStore) DeleteDMBlock(_ context.Context, arg db.DeleteDMBlockParams) (int64, error) {
	if !s.blocks[arg.BlockerID][arg.BlockedID] {
		return 0, nil
	}
	delete(s.blocks[arg.BlockerID], arg.BlockedID)
	return 1, nil
}

func (s *dmFakeStore) DeleteDMInboxForPair(_ context.Context, arg db.DeleteDMInboxForPairParams) (int64, error) {
	var rows int64
	for messageID, row := range s.inbox {
		if row.SenderUserID == arg.SenderID && row.RecipientUserID == arg.RecipientID {
			delete(s.inbox, messageID)
			delete(s.acknowledged, messageID)
			rows++
		}
	}
	return rows, nil
}

func (s *dmFakeStore) DeleteDMIdempotencyForPairMessages(_ context.Context, arg db.DeleteDMIdempotencyForPairMessagesParams) (int64, error) {
	var rows int64
	for key, record := range s.idempotency {
		for userID, device := range s.devices {
			if userID != arg.SenderID || device.DeviceID != key.deviceID {
				continue
			}
			for _, inbox := range s.inbox {
				if inbox.MessageID == record.MessageID && inbox.RecipientUserID == arg.RecipientID {
					delete(s.idempotency, key)
					s.pairPurges++
					rows++
					break
				}
			}
		}
	}
	return rows, nil
}

func (s *dmFakeStore) LockActiveDMUser(_ context.Context, userID uuid.UUID) (uuid.UUID, error) {
	return userID, nil
}

func (s *dmFakeStore) GetDMDeviceForUpdate(_ context.Context, arg db.GetDMDeviceForUpdateParams) (db.DmDevice, error) {
	return s.GetDMDeviceByUserAndID(context.Background(), db.GetDMDeviceByUserAndIDParams{
		UserID: arg.UserID, DeviceID: arg.DeviceID,
	})
}

func (s *dmFakeStore) DeleteDMDevicesByUser(_ context.Context, userID uuid.UUID) (int64, error) {
	device, ok := s.devices[userID]
	if !ok {
		return 0, nil
	}
	delete(s.devices, userID)
	delete(s.keys, device.DeviceID)
	delete(s.keyIDs, device.DeviceID)
	delete(s.fallbackKeys, device.DeviceID)
	for messageID, row := range s.inbox {
		if row.RecipientDeviceID == device.DeviceID {
			delete(s.inbox, messageID)
			delete(s.acknowledged, messageID)
		}
	}
	for key := range s.idempotency {
		if key.deviceID == device.DeviceID {
			delete(s.idempotency, key)
		}
	}
	return 1, nil
}

func (s *dmFakeStore) CreateDMDevice(_ context.Context, arg db.CreateDMDeviceParams) (db.DmDevice, error) {
	device := db.DmDevice{
		DeviceID: arg.DeviceID, UserID: arg.UserID,
		IdentityCurve25519: arg.IdentityCurve25519, IdentityEd25519: arg.IdentityEd25519,
		NextInboxSequence: 1,
	}
	s.devices[arg.UserID] = device
	return device, nil
}

func (s *dmFakeStore) CreateDMIdempotency(_ context.Context, arg db.CreateDMIdempotencyParams) (db.CreateDMIdempotencyRow, error) {
	key := dmFakeIdemKey{deviceID: arg.SenderDeviceID, clientID: arg.ClientMessageID}
	if old, ok := s.idempotency[key]; ok && old.ExpiresAt.After(time.Now()) {
		return db.CreateDMIdempotencyRow{}, sql.ErrNoRows
	}
	s.idempotency[key] = db.GetDMIdempotencyRow{
		RequestDigest: append([]byte(nil), arg.RequestDigest...),
		MessageID:     arg.MessageID, ExpiresAt: arg.ExpiresAt,
	}
	return db.CreateDMIdempotencyRow{
		SenderDeviceID: arg.SenderDeviceID, ClientMessageID: arg.ClientMessageID,
		RequestDigest: append([]byte(nil), arg.RequestDigest...), MessageID: arg.MessageID,
		ExpiresAt: arg.ExpiresAt,
	}, nil
}

func (s *dmFakeStore) CountPendingDMInbox(_ context.Context, _ uuid.UUID) (int64, error) {
	return s.pending + int64(len(s.inbox)), nil
}

func (s *dmFakeStore) AllocateDMSequence(_ context.Context, deviceID uuid.UUID) (int64, error) {
	for userID, device := range s.devices {
		if device.DeviceID == deviceID {
			sequence := device.NextInboxSequence
			device.NextInboxSequence++
			s.devices[userID] = device
			return sequence, nil
		}
	}
	return 0, sql.ErrNoRows
}

func (s *dmFakeStore) CreateDMInboxMessage(_ context.Context, arg db.CreateDMInboxMessageParams) (db.CreateDMInboxMessageRow, error) {
	s.inbox[arg.MessageID] = db.ListPendingDMInboxRow{
		MessageID: arg.MessageID, SenderUserID: arg.SenderUserID, SenderDeviceID: arg.SenderDeviceID,
		RecipientUserID: arg.RecipientUserID, RecipientDeviceID: arg.RecipientDeviceID,
		Sequence: arg.Sequence, Protocol: arg.Protocol, ProtocolVersion: arg.ProtocolVersion,
		MessageType: arg.MessageType, Ciphertext: append([]byte(nil), arg.Ciphertext...),
		CreatedAt: time.Now(), ExpiresAt: arg.ExpiresAt,
	}
	return db.CreateDMInboxMessageRow{
		MessageID: arg.MessageID, RecipientDeviceID: arg.RecipientDeviceID,
		Sequence: arg.Sequence, ExpiresAt: arg.ExpiresAt,
	}, nil
}

func (s *dmFakeStore) SetDMIdempotencySequence(_ context.Context, arg db.SetDMIdempotencySequenceParams) (int64, error) {
	key := dmFakeIdemKey{deviceID: arg.SenderDeviceID, clientID: arg.ClientMessageID}
	record, ok := s.idempotency[key]
	if !ok {
		return 0, nil
	}
	record.ResponseSequence = arg.ResponseSequence
	s.idempotency[key] = record
	return 1, nil
}

type dmFakeRunner struct {
	store *dmFakeStore
}

func (r dmFakeRunner) Run(ctx context.Context, fn func(q dmTxStore) error) error {
	return fn(r.store)
}

func newDMTestEcho(user db.User, method, path string, handler echo.HandlerFunc) *echo.Echo {
	e := echo.New()
	e.Validator = testValidator{v: validatorForDMTests()}
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: user},
	}))
	e.Add(method, path, handler)
	return e
}

func TestRegisterDMDeviceIsIdempotentAndReplacementPurgesOldDevice(t *testing.T) {
	user := viewerUser()
	store := newDMFakeStore()
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/device", handler.RegisterDevice)
	curveKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	edKey := base64.StdEncoding.EncodeToString(bytesOf(1, 32))
	deviceID := uuid.New()
	body := fmt.Sprintf(`{"device_id":%q,"identity_curve25519":%q,"identity_ed25519":%q}`, deviceID, curveKey, edKey)

	first := doJSON(e, http.MethodPost, "/api/dm/device", body)
	if first.Code != http.StatusOK {
		t.Fatalf("first registration status = %d, body=%s", first.Code, first.Body.String())
	}
	second := doJSON(e, http.MethodPost, "/api/dm/device", body)
	if second.Code != http.StatusOK {
		t.Fatalf("idempotent registration status = %d, body=%s", second.Code, second.Body.String())
	}
	otherDeviceID := uuid.New()
	replacement := fmt.Sprintf(`{"device_id":%q,"identity_curve25519":%q,"identity_ed25519":%q}`, otherDeviceID, curveKey, edKey)
	third := doJSON(e, http.MethodPost, "/api/dm/device", replacement)
	if third.Code != http.StatusOK {
		t.Fatalf("replacement status = %d, body=%s", third.Code, third.Body.String())
	}
	var response dmRequestEnvelope[dmRegisterDeviceResponse]
	if err := json.Unmarshal(third.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Data.ReplacedDevice || response.Data.DeviceID != otherDeviceID {
		t.Fatalf("replacement response = %+v", response.Data)
	}
	if got := store.devices[user.ID].DeviceID; got != otherDeviceID {
		t.Fatalf("active device = %s, want %s", got, otherDeviceID)
	}
}

func TestRegisterDMDeviceRejectsForeignDeviceID(t *testing.T) {
	user := viewerUser()
	ownerID := uuid.New()
	foreignDeviceID := uuid.New()
	store := newDMFakeStore()
	store.devices[ownerID] = db.DmDevice{DeviceID: foreignDeviceID, UserID: ownerID}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/device", handler.RegisterDevice)

	body := fmt.Sprintf(`{"device_id":%q,"identity_curve25519":%q,"identity_ed25519":%q}`,
		foreignDeviceID, base64.RawStdEncoding.EncodeToString(bytesOf(1, 32)), base64.RawStdEncoding.EncodeToString(bytesOf(2, 32)))
	assertErrCode(t, doJSON(e, http.MethodPost, "/api/dm/device", body), http.StatusConflict, shared.CodeDMDeviceStale)
	if store.devices[ownerID].DeviceID != foreignDeviceID {
		t.Fatal("foreign device was replaced")
	}
	if _, ok := store.devices[user.ID]; ok {
		t.Fatal("caller was given a device owned by another user")
	}
}

func TestSendMessageStoresCiphertextAndReplaysIdempotently(t *testing.T) {
	user := viewerUser()
	recipientID := uuid.New()
	senderDeviceID, recipientDeviceID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.eligible = true
	store.devices[user.ID] = db.DmDevice{DeviceID: senderDeviceID, UserID: user.ID}
	store.devices[recipientID] = db.DmDevice{DeviceID: recipientDeviceID, UserID: recipientID, NextInboxSequence: 1}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/messages", handler.SendMessage)
	clientMessageID := uuid.New()
	request := dmSendRequest{
		DeviceID: senderDeviceID.String(), RecipientUserID: recipientID.String(),
		RecipientDeviceID: recipientDeviceID.String(), ClientMessageID: clientMessageID.String(),
		Protocol: "olm", ProtocolVersion: 1, MessageType: 0,
		Ciphertext: base64.StdEncoding.EncodeToString([]byte("opaque-ciphertext")),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	first := doJSON(e, http.MethodPost, "/api/dm/messages", string(encoded))
	if first.Code != http.StatusCreated {
		t.Fatalf("first send status = %d, body=%s", first.Code, first.Body.String())
	}
	var firstResponse dmRequestEnvelope[dmSendResponse]
	if err := json.Unmarshal(first.Body.Bytes(), &firstResponse); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(store.inbox) != 1 {
		t.Fatalf("stored messages = %d, want 1", len(store.inbox))
	}
	for _, message := range store.inbox {
		if string(message.Ciphertext) != "opaque-ciphertext" {
			t.Fatalf("stored ciphertext = %q", message.Ciphertext)
		}
	}

	retry := doJSON(e, http.MethodPost, "/api/dm/messages", string(encoded))
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, body=%s", retry.Code, retry.Body.String())
	}
	var retryResponse dmRequestEnvelope[dmSendResponse]
	if err := json.Unmarshal(retry.Body.Bytes(), &retryResponse); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if retryResponse.Data.MessageID != firstResponse.Data.MessageID ||
		retryResponse.Data.Sequence != firstResponse.Data.Sequence || len(store.inbox) != 1 {
		t.Fatalf("retry did not return original message: first=%+v retry=%+v stored=%d",
			firstResponse.Data, retryResponse.Data, len(store.inbox))
	}
}

func TestSendMessageRejectsIdempotencyCollisionAndFullInbox(t *testing.T) {
	user := viewerUser()
	recipientID := uuid.New()
	senderDeviceID, recipientDeviceID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.eligible = true
	store.devices[user.ID] = db.DmDevice{DeviceID: senderDeviceID, UserID: user.ID}
	store.devices[recipientID] = db.DmDevice{DeviceID: recipientDeviceID, UserID: recipientID, NextInboxSequence: 1}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/messages", handler.SendMessage)
	clientMessageID := uuid.New()
	makeBody := func(content string) string {
		req := dmSendRequest{
			DeviceID: senderDeviceID.String(), RecipientUserID: recipientID.String(),
			RecipientDeviceID: recipientDeviceID.String(), ClientMessageID: clientMessageID.String(),
			Protocol: "olm", ProtocolVersion: 1, MessageType: 1,
			Ciphertext: base64.StdEncoding.EncodeToString([]byte(content)),
		}
		encoded, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if rec := doJSON(e, http.MethodPost, "/api/dm/messages", makeBody("first")); rec.Code != http.StatusCreated {
		t.Fatalf("initial send status = %d, body=%s", rec.Code, rec.Body.String())
	}
	collision := doJSON(e, http.MethodPost, "/api/dm/messages", makeBody("different"))
	assertErrCode(t, collision, http.StatusConflict, shared.CodeDMIdempotencyConflict)

	fullStore := newDMFakeStore()
	fullStore.eligible = true
	fullStore.pending = dmMaxInboxPending
	fullStore.devices[user.ID] = db.DmDevice{DeviceID: senderDeviceID, UserID: user.ID}
	fullStore.devices[recipientID] = db.DmDevice{DeviceID: recipientDeviceID, UserID: recipientID}
	fullHandler := newDMHandlerForTest(fullStore, dmFakeRunner{store: fullStore}, 168*time.Hour)
	fullEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/messages", fullHandler.SendMessage)
	full := doJSON(fullEcho, http.MethodPost, "/api/dm/messages", makeBodyWithClientID(
		t, senderDeviceID, recipientID, recipientDeviceID, uuid.New(), "full",
	))
	assertErrCode(t, full, http.StatusConflict, shared.CodeDMInboxFull)
}

func TestGetDMKeyBundleClaimsOneTimeKeyOnce(t *testing.T) {
	user := viewerUser()
	targetID := uuid.New()
	requesterDeviceID, targetDeviceID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.eligible = true
	store.devices[user.ID] = db.DmDevice{DeviceID: requesterDeviceID, UserID: user.ID}
	store.devices[targetID] = db.DmDevice{
		DeviceID: targetDeviceID, UserID: targetID,
		IdentityCurve25519: base64.StdEncoding.EncodeToString(bytesOf(1, 32)),
		IdentityEd25519:    base64.StdEncoding.EncodeToString(bytesOf(2, 32)),
	}
	store.keys[targetDeviceID] = []db.ClaimDMOneTimeKeyRow{{
		KeyID: "one", PublicKey: base64.StdEncoding.EncodeToString(bytesOf(3, 32)),
		Signature: base64.StdEncoding.EncodeToString(bytesOf(4, 64)),
	}}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	path := fmt.Sprintf("/api/dm/users/%s/key-bundle?device_id=%s&include_one_time_key=true", targetID, requesterDeviceID)
	e := newDMTestEcho(user, http.MethodGet, "/api/dm/users/:id/key-bundle", handler.GetKeyBundle)

	first := doJSON(e, http.MethodGet, path, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first bundle status = %d, body=%s", first.Code, first.Body.String())
	}
	var bundleResponse dmRequestEnvelope[dmKeyBundle]
	if err := json.Unmarshal(first.Body.Bytes(), &bundleResponse); err != nil {
		t.Fatalf("decode bundle response: %v", err)
	}
	wantCurve := base64.RawStdEncoding.EncodeToString(bytesOf(1, 32))
	wantEd := base64.RawStdEncoding.EncodeToString(bytesOf(2, 32))
	wantPublic := base64.RawStdEncoding.EncodeToString(bytesOf(3, 32))
	wantSignature := base64.RawStdEncoding.EncodeToString(bytesOf(4, 64))
	if bundleResponse.Data.IdentityCurve25519 != wantCurve ||
		bundleResponse.Data.IdentityEd25519 != wantEd ||
		bundleResponse.Data.OneTimeKey == nil ||
		bundleResponse.Data.OneTimeKey.PublicKey != wantPublic ||
		bundleResponse.Data.OneTimeKey.Signature != wantSignature {
		t.Fatalf("bundle must use unpadded canonical base64, got %+v", bundleResponse.Data)
	}
	second := doJSON(e, http.MethodGet, path, "")
	assertErrCode(t, second, http.StatusConflict, shared.CodeDMKeyUnavailable)
}

func TestGetDMKeyBundleDoesNotClaimOneTimeKeyByDefault(t *testing.T) {
	user := viewerUser()
	targetID := uuid.New()
	requesterDeviceID, targetDeviceID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.eligible = true
	store.devices[user.ID] = db.DmDevice{DeviceID: requesterDeviceID, UserID: user.ID}
	store.devices[targetID] = db.DmDevice{
		DeviceID: targetDeviceID, UserID: targetID,
		IdentityCurve25519: base64.StdEncoding.EncodeToString(bytesOf(1, 32)),
		IdentityEd25519:    base64.StdEncoding.EncodeToString(bytesOf(2, 32)),
	}
	store.keys[targetDeviceID] = []db.ClaimDMOneTimeKeyRow{{
		KeyID: "one", PublicKey: base64.StdEncoding.EncodeToString(bytesOf(3, 32)),
		Signature: base64.StdEncoding.EncodeToString(bytesOf(4, 64)),
	}}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	path := fmt.Sprintf("/api/dm/users/%s/key-bundle?device_id=%s", targetID, requesterDeviceID)
	e := newDMTestEcho(user, http.MethodGet, "/api/dm/users/:id/key-bundle", handler.GetKeyBundle)

	rec := doJSON(e, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var bundleResponse dmRequestEnvelope[dmKeyBundle]
	if err := json.Unmarshal(rec.Body.Bytes(), &bundleResponse); err != nil {
		t.Fatalf("decode bundle response: %v", err)
	}
	if bundleResponse.Data.OneTimeKey != nil || bundleResponse.Data.FallbackKey != nil {
		t.Fatalf("default bundle must not claim a key, got %+v", bundleResponse.Data)
	}
	if len(store.keys[targetDeviceID]) != 1 {
		t.Fatalf("one-time keys remaining = %d, want 1", len(store.keys[targetDeviceID]))
	}
}

func TestBlockUserPurgesPairStateAndUnblockClearsBlock(t *testing.T) {
	user := viewerUser()
	targetID := uuid.New()
	senderDeviceID, recipientDeviceID, messageID := uuid.New(), uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.devices[user.ID] = db.DmDevice{DeviceID: senderDeviceID, UserID: user.ID}
	store.devices[targetID] = db.DmDevice{DeviceID: recipientDeviceID, UserID: targetID}
	store.inbox[messageID] = db.ListPendingDMInboxRow{
		MessageID: messageID, SenderUserID: user.ID, SenderDeviceID: senderDeviceID,
		RecipientUserID: targetID, RecipientDeviceID: recipientDeviceID,
	}
	store.idempotency[dmFakeIdemKey{deviceID: senderDeviceID, clientID: uuid.New()}] = db.GetDMIdempotencyRow{
		MessageID: messageID,
	}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	path := fmt.Sprintf("/api/dm/blocks/%s", targetID)
	blockEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/blocks/:id", handler.BlockUser)

	blocked := doJSON(blockEcho, http.MethodPost, path, "")
	if blocked.Code != http.StatusOK {
		t.Fatalf("block status = %d, body=%s", blocked.Code, blocked.Body.String())
	}
	var blockResponse dmRequestEnvelope[dmBlockResponse]
	if err := json.Unmarshal(blocked.Body.Bytes(), &blockResponse); err != nil {
		t.Fatalf("decode block response: %v", err)
	}
	if !blockResponse.Data.Blocked || blockResponse.Data.UserID != targetID {
		t.Fatalf("block response = %+v", blockResponse.Data)
	}
	if !store.blocks[user.ID][targetID] {
		t.Fatal("block was not stored")
	}
	if len(store.inbox) != 0 || store.pairPurges == 0 {
		t.Fatalf("pair state not purged: inbox=%d purges=%d", len(store.inbox), store.pairPurges)
	}

	unblockEcho := newDMTestEcho(user, http.MethodDelete, "/api/dm/blocks/:id", handler.UnblockUser)
	unblocked := doJSON(unblockEcho, http.MethodDelete, path, "")
	if unblocked.Code != http.StatusOK {
		t.Fatalf("unblock status = %d, body=%s", unblocked.Code, unblocked.Body.String())
	}
	if store.blocks[user.ID][targetID] {
		t.Fatal("block was not removed")
	}
}

func TestUploadFallbackKeyAndServeItWhenOneTimeKeysAreDrained(t *testing.T) {
	user := viewerUser()
	targetID := user.ID
	targetDeviceID := uuid.New()
	store := newDMFakeStore()
	store.eligible = true
	store.devices[targetID] = db.DmDevice{
		DeviceID: targetDeviceID, UserID: targetID,
		IdentityCurve25519: base64.StdEncoding.EncodeToString(bytesOf(1, 32)),
		IdentityEd25519:    base64.StdEncoding.EncodeToString(bytesOf(2, 32)),
	}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	uploadEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/device/one-time-keys", handler.UploadOneTimeKeys)
	fallback := dmOneTimeKeyInput{
		KeyID: "fallback", PublicKey: base64.StdEncoding.EncodeToString(bytesOf(3, 32)),
		Signature: base64.StdEncoding.EncodeToString(bytesOf(4, 64)),
	}
	uploadBody, err := json.Marshal(dmUploadKeysRequest{
		DeviceID: targetDeviceID.String(),
		Keys: []dmOneTimeKeyInput{{
			KeyID: "one", PublicKey: base64.StdEncoding.EncodeToString(bytesOf(5, 32)),
			Signature: base64.StdEncoding.EncodeToString(bytesOf(6, 64)),
		}},
		FallbackKey: &fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	uploaded := doJSON(uploadEcho, http.MethodPost, "/api/dm/device/one-time-keys", string(uploadBody))
	if uploaded.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", uploaded.Code, uploaded.Body.String())
	}
	if _, err := store.GetDMFallbackKey(context.Background(), targetDeviceID); err != nil {
		t.Fatalf("fallback key was not stored: %v", err)
	}
	store.keys[targetDeviceID] = nil

	path := fmt.Sprintf("/api/dm/users/%s/key-bundle?device_id=%s&include_one_time_key=true", targetID, targetDeviceID)
	bundleEcho := newDMTestEcho(user, http.MethodGet, "/api/dm/users/:id/key-bundle", handler.GetKeyBundle)
	rec := doJSON(bundleEcho, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var bundleResponse dmRequestEnvelope[dmKeyBundle]
	if err := json.Unmarshal(rec.Body.Bytes(), &bundleResponse); err != nil {
		t.Fatalf("decode bundle response: %v", err)
	}
	if bundleResponse.Data.OneTimeKey != nil || bundleResponse.Data.FallbackKey == nil ||
		bundleResponse.Data.FallbackKey.KeyID != "fallback" {
		t.Fatalf("expected fallback-only bundle, got %+v", bundleResponse.Data)
	}
}

func TestReplacingSenderDevicePreservesSentInbox(t *testing.T) {
	user := viewerUser()
	recipientID := uuid.New()
	oldDeviceID, newDeviceID, messageID := uuid.New(), uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.devices[user.ID] = db.DmDevice{DeviceID: oldDeviceID, UserID: user.ID}
	store.devices[recipientID] = db.DmDevice{DeviceID: uuid.New(), UserID: recipientID}
	store.inbox[messageID] = db.ListPendingDMInboxRow{
		MessageID: messageID, SenderUserID: user.ID, SenderDeviceID: oldDeviceID,
		RecipientDeviceID: store.devices[recipientID].DeviceID, Sequence: 1,
		Ciphertext: []byte("ciphertext"), Protocol: "olm", ProtocolVersion: 1,
	}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/device", handler.RegisterDevice)
	curveKey := base64.StdEncoding.EncodeToString(bytesOf(1, 32))
	edKey := base64.StdEncoding.EncodeToString(bytesOf(2, 32))
	body := fmt.Sprintf(`{"device_id":%q,"identity_curve25519":%q,"identity_ed25519":%q}`, newDeviceID, curveKey, edKey)

	rec := doJSON(e, http.MethodPost, "/api/dm/device", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-registration status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := store.inbox[messageID]; !ok {
		t.Fatal("sent inbox message was deleted when the sender replaced its device")
	}
	if store.devices[user.ID].DeviceID != newDeviceID {
		t.Fatalf("active device = %s, want %s", store.devices[user.ID].DeviceID, newDeviceID)
	}
}

func TestPollAndAcknowledgeDMMessage(t *testing.T) {
	user := viewerUser()
	deviceID, messageID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.devices[user.ID] = db.DmDevice{DeviceID: deviceID, UserID: user.ID}
	store.inbox[messageID] = db.ListPendingDMInboxRow{
		MessageID: messageID, RecipientDeviceID: deviceID, Sequence: 1,
		Ciphertext: []byte("ciphertext"), Protocol: "olm", ProtocolVersion: 1,
	}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := echo.New()
	e.Validator = testValidator{v: validatorForDMTests()}
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"}, Queries: testUserStore{user: user},
	}))
	e.GET("/api/dm/inbox", handler.PollInbox)
	e.POST("/api/dm/inbox/:message_id/ack", handler.AcknowledgeMessage)

	pollPath := fmt.Sprintf("/api/dm/inbox?device_id=%s&after=0", deviceID)
	poll := doJSON(e, http.MethodGet, pollPath, "")
	if poll.Code != http.StatusOK {
		t.Fatalf("poll status = %d, body=%s", poll.Code, poll.Body.String())
	}
	var pollResponse dmRequestEnvelope[dmPollResponse]
	if err := json.Unmarshal(poll.Body.Bytes(), &pollResponse); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	if len(pollResponse.Data.Messages) != 1 || pollResponse.Data.Messages[0].Ciphertext != base64.RawStdEncoding.EncodeToString([]byte("ciphertext")) {
		t.Fatalf("poll response = %+v", pollResponse.Data)
	}

	ackPath := fmt.Sprintf("/api/dm/inbox/%s/ack", messageID)
	ackBody := fmt.Sprintf(`{"device_id":%q}`, deviceID)
	if rec := doJSON(e, http.MethodPost, ackPath, ackBody); rec.Code != http.StatusNoContent {
		t.Fatalf("ack status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(e, http.MethodPost, ackPath, ackBody); rec.Code != http.StatusNoContent {
		t.Fatalf("repeat ack status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := store.inbox[messageID].Ciphertext; got != nil {
		t.Fatalf("ack did not clear ciphertext: %q", got)
	}
}

func TestDMEndpointsHideNonMutualFollow(t *testing.T) {
	user := viewerUser()
	targetID := uuid.New()
	senderDeviceID, targetDeviceID := uuid.New(), uuid.New()
	store := newDMFakeStore()
	store.eligible = false
	store.devices[user.ID] = db.DmDevice{DeviceID: senderDeviceID, UserID: user.ID}
	store.devices[targetID] = db.DmDevice{DeviceID: targetDeviceID, UserID: targetID}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)

	bundlePath := fmt.Sprintf("/api/dm/users/%s/key-bundle?device_id=%s&include_one_time_key=false", targetID, senderDeviceID)
	bundleEcho := newDMTestEcho(user, http.MethodGet, "/api/dm/users/:id/key-bundle", handler.GetKeyBundle)
	assertErrCode(t, doJSON(bundleEcho, http.MethodGet, bundlePath, ""), http.StatusNotFound, shared.CodeNotFound)

	sendEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/messages", handler.SendMessage)
	body := makeBodyWithClientID(t, senderDeviceID, targetID, targetDeviceID, uuid.New(), "non-mutual")
	assertErrCode(t, doJSON(sendEcho, http.MethodPost, "/api/dm/messages", body), http.StatusNotFound, shared.CodeNotFound)
}

func TestDMDeviceSpecificEndpointsRejectStaleDevice(t *testing.T) {
	user := viewerUser()
	staleDeviceID := uuid.New()
	store := newDMFakeStore()
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)

	uploadEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/device/one-time-keys", handler.UploadOneTimeKeys)
	uploadBody := fmt.Sprintf(`{"device_id":%q,"keys":[{"key_id":"k","public_key":%q,"signature":%q}]}`,
		staleDeviceID, base64.RawStdEncoding.EncodeToString(bytesOf(1, 32)), base64.RawStdEncoding.EncodeToString(bytesOf(2, 64)))
	assertErrCode(t, doJSON(uploadEcho, http.MethodPost, "/api/dm/device/one-time-keys", uploadBody),
		http.StatusConflict, shared.CodeDMDeviceStale)

	pollEcho := newDMTestEcho(user, http.MethodGet, "/api/dm/inbox", handler.PollInbox)
	pollPath := fmt.Sprintf("/api/dm/inbox?device_id=%s&after=0", staleDeviceID)
	assertErrCode(t, doJSON(pollEcho, http.MethodGet, pollPath, ""), http.StatusConflict, shared.CodeDMDeviceStale)

	ackEcho := newDMTestEcho(user, http.MethodPost, "/api/dm/inbox/:message_id/ack", handler.AcknowledgeMessage)
	ackPath := fmt.Sprintf("/api/dm/inbox/%s/ack", uuid.New())
	ackBody := fmt.Sprintf(`{"device_id":%q}`, staleDeviceID)
	assertErrCode(t, doJSON(ackEcho, http.MethodPost, ackPath, ackBody), http.StatusConflict, shared.CodeDMDeviceStale)
}

func TestDMAckRejectsMessageForAnotherDevice(t *testing.T) {
	user := viewerUser()
	activeDeviceID, otherDeviceID := uuid.New(), uuid.New()
	messageID := uuid.New()
	store := newDMFakeStore()
	store.devices[user.ID] = db.DmDevice{DeviceID: activeDeviceID, UserID: user.ID}
	store.inbox[messageID] = db.ListPendingDMInboxRow{MessageID: messageID, RecipientDeviceID: otherDeviceID}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/inbox/:message_id/ack", handler.AcknowledgeMessage)

	body := fmt.Sprintf(`{"device_id":%q}`, activeDeviceID)
	path := fmt.Sprintf("/api/dm/inbox/%s/ack", messageID)
	assertErrCode(t, doJSON(e, http.MethodPost, path, body), http.StatusNotFound, shared.CodeNotFound)
	if store.acknowledged[messageID] {
		t.Fatal("message addressed to another device was acknowledged")
	}
}

func TestDMPollValidatesAfterAndLimit(t *testing.T) {
	user := viewerUser()
	deviceID := uuid.New()
	store := newDMFakeStore()
	store.devices[user.ID] = db.DmDevice{DeviceID: deviceID, UserID: user.ID}
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodGet, "/api/dm/inbox", handler.PollInbox)

	base := "/api/dm/inbox?device_id=" + deviceID.String()
	for _, target := range []string{
		base,
		base + "&after=-1",
		base + "&after=0&limit=0",
		base + "&after=0&limit=101",
		base + "&after=not-a-number",
	} {
		assertErrCode(t, doJSON(e, http.MethodGet, target, ""), http.StatusBadRequest, shared.CodeValidationError)
	}
	if rec := doJSON(e, http.MethodGet, base+"&after=0&limit=100", ""); rec.Code != http.StatusOK {
		t.Fatalf("valid poll status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestDMRejectsInactiveUser(t *testing.T) {
	user := viewerUser()
	user.IsActive = false
	store := newDMFakeStore()
	handler := newDMHandlerForTest(store, dmFakeRunner{store: store}, 168*time.Hour)
	e := newDMTestEcho(user, http.MethodPost, "/api/dm/messages", handler.SendMessage)
	body := makeBodyWithClientID(t, uuid.New(), uuid.New(), uuid.New(), uuid.New(), "inactive")
	assertErrCode(t, doJSON(e, http.MethodPost, "/api/dm/messages", body), http.StatusUnauthorized, shared.CodeUnauthorized)
}

func makeBodyWithClientID(t *testing.T, senderDeviceID, recipientID, recipientDeviceID, clientMessageID uuid.UUID, content string) string {
	t.Helper()
	request := dmSendRequest{
		DeviceID: senderDeviceID.String(), RecipientUserID: recipientID.String(),
		RecipientDeviceID: recipientDeviceID.String(), ClientMessageID: clientMessageID.String(),
		Protocol: "olm", ProtocolVersion: 1, MessageType: 1,
		Ciphertext: base64.StdEncoding.EncodeToString([]byte(content)),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestDecodeDMKeyReturnsUnpaddedCanonicalBase64(t *testing.T) {
	raw := bytesOf(7, 32)
	inputs := map[string]string{
		"padded": base64.StdEncoding.EncodeToString(raw),
		"raw":    base64.RawStdEncoding.EncodeToString(raw),
	}
	want := base64.RawStdEncoding.EncodeToString(raw)
	for name, input := range inputs {
		got, err := decodeDMKey(input, 32)
		if err != nil {
			t.Fatalf("%s input: decodeDMKey: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s input = %q, want %q", name, got, want)
		}
	}
	if _, err := decodeDMKey("!!!", 32); err == nil {
		t.Fatal("invalid base64 must be rejected")
	}
	if _, err := decodeDMKey(base64.RawStdEncoding.EncodeToString(bytesOf(7, 31)), 32); err == nil {
		t.Fatal("wrong key length must be rejected")
	}
}

func TestDMStoreErrorDoesNotLeakInternalDetails(t *testing.T) {
	err := dmStoreError("store DM ciphertext", errors.New("pq: duplicate key value violates unique constraint dm_inbox_pkey"))
	_, code, message, _ := shared.ClassifyError(err)
	if code != shared.CodeInternalError {
		t.Fatalf("code = %q, want %q", code, shared.CodeInternalError)
	}
	for _, leaked := range []string{"duplicate key", "dm_inbox_pkey", "store DM ciphertext"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("message leaked %q: %s", leaked, message)
		}
	}
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}

func validatorForDMTests() *validator.Validate {
	return validator.New()
}

var _ dmTxStore = (*dmFakeStore)(nil)
