package dm

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type relayDevice struct {
	UserID      string
	DeviceID    string
	Curve       string
	Ed          string
	OneTimeKeys []OneTimeKey
	Claimed     map[string]bool
}

type relay struct {
	mu          sync.Mutex
	devices     map[string]*relayDevice
	messages    map[string]InboxMessage
	acked       map[string]bool
	failAckOnce bool
}

func newRelay() *relay {
	return &relay{
		devices:  make(map[string]*relayDevice),
		messages: make(map[string]InboxMessage),
		acked:    make(map[string]bool),
	}
}

func (r *relay) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/dm/device", r.registerDevice)
	mux.HandleFunc("POST /api/dm/device/one-time-keys", r.uploadKeys)
	mux.HandleFunc("GET /api/dm/users/{id}/key-bundle", r.keyBundle)
	mux.HandleFunc("POST /api/dm/messages", r.sendMessage)
	mux.HandleFunc("GET /api/dm/inbox", r.pollInbox)
	mux.HandleFunc("POST /api/dm/inbox/{message_id}/ack", r.ack)
	return mux
}

func (r *relay) registerDevice(w http.ResponseWriter, req *http.Request) {
	var body struct {
		DeviceID           string `json:"device_id"`
		IdentityCurve25519 string `json:"identity_curve25519"`
		IdentityEd25519    string `json:"identity_ed25519"`
	}
	if err := decodeRelayJSON(req, &body); err != nil {
		writeRelayError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	userID := userFromRequest(req)
	r.mu.Lock()
	r.devices[userID] = &relayDevice{
		UserID: userID, DeviceID: body.DeviceID, Curve: body.IdentityCurve25519,
		Ed: body.IdentityEd25519, Claimed: make(map[string]bool),
	}
	r.mu.Unlock()
	writeRelayData(w, http.StatusOK, map[string]any{"device_id": body.DeviceID, "replaced_device": false})
}

func (r *relay) uploadKeys(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Keys []OneTimeKey `json:"keys"`
	}
	if err := decodeRelayJSON(req, &body); err != nil {
		writeRelayError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	r.mu.Lock()
	device := r.devices[userFromRequest(req)]
	if device == nil {
		r.mu.Unlock()
		writeRelayError(w, http.StatusConflict, "DM_DEVICE_STALE", "device is not active")
		return
	}
	device.OneTimeKeys = append(device.OneTimeKeys, body.Keys...)
	r.mu.Unlock()
	writeRelayData(w, http.StatusOK, map[string]any{"accepted": len(body.Keys)})
}

func (r *relay) keyBundle(w http.ResponseWriter, req *http.Request) {
	target := req.PathValue("id")
	include, err := strconv.ParseBool(req.URL.Query().Get("include_one_time_key"))
	if err != nil {
		include = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	device := r.devices[target]
	if device == nil {
		writeRelayError(w, http.StatusNotFound, "NOT_FOUND", "target not found")
		return
	}
	bundle := map[string]any{
		"user_id":             device.UserID,
		"device_id":           device.DeviceID,
		"identity_curve25519": device.Curve,
		"identity_ed25519":    device.Ed,
		"one_time_key":        nil,
	}
	if include {
		for _, key := range device.OneTimeKeys {
			if device.Claimed[key.KeyID] {
				continue
			}
			device.Claimed[key.KeyID] = true
			bundle["one_time_key"] = key
			break
		}
		if bundle["one_time_key"] == nil {
			writeRelayError(w, http.StatusConflict, "DM_KEY_UNAVAILABLE", "no one-time key")
			return
		}
	}
	writeRelayData(w, http.StatusOK, bundle)
}

func (r *relay) sendMessage(w http.ResponseWriter, req *http.Request) {
	var body SendRequest
	if err := decodeRelayJSON(req, &body); err != nil {
		writeRelayError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	sender := userFromRequest(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	device := r.devices[body.RecipientUserID]
	if device == nil || device.DeviceID != body.RecipientDeviceID {
		writeRelayError(w, http.StatusConflict, "DM_DEVICE_STALE", "recipient device changed")
		return
	}
	found := false
	for _, candidate := range r.devices {
		if candidate.DeviceID == body.DeviceID && candidate.UserID == sender {
			found = true
			break
		}
	}
	if !found {
		writeRelayError(w, http.StatusConflict, "DM_DEVICE_STALE", "sender device changed")
		return
	}
	messageID := uuid.NewString()
	sequence := int64(len(r.messages) + 1)
	now := time.Now().UTC()
	message := InboxMessage{
		MessageID: messageID, SenderUserID: sender, SenderDeviceID: body.DeviceID,
		RecipientUserID: body.RecipientUserID, RecipientDeviceID: body.RecipientDeviceID,
		Sequence: sequence, Protocol: body.Protocol, ProtocolVersion: body.ProtocolVersion,
		MessageType: body.MessageType, Ciphertext: body.Ciphertext,
		CreatedAt: now, ExpiresAt: now.Add(168 * time.Hour),
	}
	r.messages[messageID] = message
	writeRelayData(w, http.StatusCreated, map[string]any{
		"message_id": messageID, "sequence": sequence, "expires_at": message.ExpiresAt,
	})
}

func (r *relay) pollInbox(w http.ResponseWriter, req *http.Request) {
	deviceID := req.URL.Query().Get("device_id")
	after, err := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeRelayError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid after")
		return
	}
	limit, err := strconv.Atoi(req.URL.Query().Get("limit"))
	if err != nil || limit < 1 || limit > 100 {
		limit = 100
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]InboxMessage, 0)
	for _, message := range r.messages {
		if message.RecipientDeviceID != deviceID || r.acked[message.MessageID] || message.Sequence <= after {
			continue
		}
		messages = append(messages, message)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	hasMore := len(messages) > limit
	if hasMore {
		messages = messages[:limit]
	}
	nextAfter := after
	if len(messages) > 0 {
		nextAfter = messages[len(messages)-1].Sequence
	}
	writeRelayData(w, http.StatusOK, PollResult{Messages: messages, NextAfter: nextAfter, HasMore: hasMore})
}

func (r *relay) ack(w http.ResponseWriter, req *http.Request) {
	var body struct {
		DeviceID string `json:"device_id"`
	}
	if err := decodeRelayJSON(req, &body); err != nil {
		writeRelayError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
		return
	}
	messageID := req.PathValue("message_id")
	r.mu.Lock()
	defer r.mu.Unlock()
	message, ok := r.messages[messageID]
	if !ok || message.RecipientDeviceID != body.DeviceID {
		writeRelayError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if r.failAckOnce {
		r.failAckOnce = false
		writeRelayError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "ack failed")
		return
	}
	r.acked[messageID] = true
	w.WriteHeader(http.StatusNoContent)
}

func (r *relay) message(id string) (InboxMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	message, ok := r.messages[id]
	if !ok {
		return InboxMessage{}, errors.New("message not found")
	}
	return message, nil
}

func (r *relay) isAcked(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acked[id]
}

func userFromRequest(req *http.Request) string {
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
}

func decodeRelayJSON(req *http.Request, dst any) error {
	defer req.Body.Close()
	return json.NewDecoder(req.Body).Decode(dst)
}

func writeRelayData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeRelayError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

func newTestServer(t *testing.T, relay *relay) *httptest.Server {
	t.Helper()
	return httptest.NewServer(relay.mux())
}

func newTestService(t *testing.T, baseURL, userID string) *Service {
	t.Helper()
	value, err := GeneratePickleKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePickleKey(value)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(baseURL, userID)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "state.json"))
	trust := NewTrust(filepath.Join(dir, "trust.json"))
	service, err := NewService(client, store, trust, key)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func setupPairedServices(t *testing.T, baseURL, aliceUser, bobUser string) (*Service, *Service) {
	t.Helper()
	alice := newTestService(t, baseURL, aliceUser)
	bob := newTestService(t, baseURL, bobUser)
	if _, _, err := alice.Init(aliceUser); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bob.Init(bobUser); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.UploadKeys(t.Context(), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.UploadKeys(t.Context(), 4); err != nil {
		t.Fatal(err)
	}
	aliceInfo, err := alice.PeerInfo(t.Context(), bobUser)
	if err != nil {
		t.Fatal(err)
	}
	bobInfo, err := bob.PeerInfo(t.Context(), aliceUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Verify(t.Context(), bobUser, aliceInfo.DeviceID, aliceInfo.FormattedFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Verify(t.Context(), aliceUser, bobInfo.DeviceID, bobInfo.FormattedFingerprint); err != nil {
		t.Fatal(err)
	}
	return alice, bob
}

func TestEndToEndSendReceive(t *testing.T) {
	relay := newRelay()
	server := newTestServer(t, relay)
	defer server.Close()
	aliceUser := uuid.NewString()
	bobUser := uuid.NewString()
	alice, bob := setupPairedServices(t, server.URL, aliceUser, bobUser)

	if _, err := alice.Send(t.Context(), bobUser, "halo dari alice", ""); err != nil {
		t.Fatal(err)
	}
	received, err := bob.Receive(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 || string(received[0].Plaintext) != "halo dari alice" {
		t.Fatalf("unexpected received messages %+v", received)
	}
	if !relay.isAcked(received[0].MessageID) {
		t.Fatal("message was not acknowledged")
	}
	again, err := bob.Receive(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected empty inbox, got %d messages", len(again))
	}

	if _, err := bob.Send(t.Context(), aliceUser, "balasan bob", ""); err != nil {
		t.Fatal(err)
	}
	replies, err := alice.Receive(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || string(replies[0].Plaintext) != "balasan bob" {
		t.Fatalf("unexpected replies %+v", replies)
	}
}

func TestReceiveRetriesAckWithoutDecryptingTwice(t *testing.T) {
	relay := newRelay()
	server := newTestServer(t, relay)
	defer server.Close()
	aliceUser := uuid.NewString()
	bobUser := uuid.NewString()
	alice, bob := setupPairedServices(t, server.URL, aliceUser, bobUser)

	result, err := alice.Send(t.Context(), bobUser, "ack retry", "")
	if err != nil {
		t.Fatal(err)
	}
	relay.mu.Lock()
	relay.failAckOnce = true
	relay.mu.Unlock()
	if _, err := bob.Receive(t.Context(), 10); err == nil {
		t.Fatal("expected first ack to fail")
	}
	retried, err := bob.Receive(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried) != 1 || !retried[0].AlreadyDelivered {
		t.Fatalf("unexpected retry result %+v", retried)
	}
	if !relay.isAcked(result.MessageID) {
		t.Fatal("retry did not acknowledge message")
	}
}
