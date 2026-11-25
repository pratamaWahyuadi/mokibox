package dm

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFingerprint(t *testing.T) {
	userID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	deviceID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	curve := base64.RawStdEncoding.EncodeToString(bytesOf(0x01, 32))
	ed := base64.RawStdEncoding.EncodeToString(bytesOf(0x02, 32))

	got, err := Fingerprint(userID.String(), deviceID.String(), curve, ed)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	payload := append([]byte("mokibox-dm-fingerprint-v1\x00"), userID[:]...)
	payload = append(payload, deviceID[:]...)
	payload = append(payload, bytesOf(0x01, 32)...)
	payload = append(payload, bytesOf(0x02, 32)...)
	sum := sha256.Sum256(payload)
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	if got != want {
		t.Fatalf("fingerprint = %s, want %s", got, want)
	}
	if formatted := FormatFingerprint(got); strings.Count(formatted, " ") != 15 {
		t.Fatalf("unexpected formatted fingerprint %q", formatted)
	}
	if NormalizeFingerprint(strings.ToLower(FormatFingerprint(got))) != got {
		t.Fatalf("normalize did not restore fingerprint")
	}
}

func TestOneTimeKeySignatures(t *testing.T) {
	acc, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := IdentityKeys(acc)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := GenerateOneTimeKeys(acc, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 4 {
		t.Fatalf("got %d keys, want 4", len(keys))
	}
	for _, key := range keys {
		if err := VerifyOneTimeKeySignature(identity.Ed25519, key); err != nil {
			t.Fatalf("verify one-time key: %v", err)
		}
	}
	bad := keys[0]
	bad.Signature = keys[1].Signature
	if err := VerifyOneTimeKeySignature(identity.Ed25519, bad); err == nil {
		t.Fatal("expected signature mismatch")
	}
}

func TestOlmRoundTrip(t *testing.T) {
	aliceAccount, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	bobAccount, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	aliceIdentity, err := IdentityKeys(aliceAccount)
	if err != nil {
		t.Fatal(err)
	}
	bobIdentity, err := IdentityKeys(bobAccount)
	if err != nil {
		t.Fatal(err)
	}
	bobKeys, err := GenerateOneTimeKeys(bobAccount, 1)
	if err != nil {
		t.Fatal(err)
	}
	aliceSession, err := NewOutboundSession(aliceAccount, bobIdentity, bobKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	firstType, firstCiphertext, err := Encrypt(aliceSession, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if firstType != 0 {
		t.Fatalf("first message type = %d, want 0", firstType)
	}
	bobSession, err := NewInboundSession(bobAccount, aliceIdentity.Curve25519, firstCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := Decrypt(bobSession, firstCiphertext, firstType)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "first" {
		t.Fatalf("decrypted %q", plaintext)
	}
	encoded, err := PickleSession(bobSession, bytesOf(0x07, 32))
	if err != nil {
		t.Fatal(err)
	}
	bobSession, err = UnpickleSession(encoded, bytesOf(0x07, 32))
	if err != nil {
		t.Fatal(err)
	}
	replyType, replyCiphertext, err := Encrypt(bobSession, []byte("reply"))
	if err != nil {
		t.Fatal(err)
	}
	if replyType != 1 {
		t.Fatalf("reply message type = %d, want 1", replyType)
	}
	plaintext, err = Decrypt(aliceSession, replyCiphertext, replyType)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "reply" {
		t.Fatalf("decrypted %q", plaintext)
	}
	secondType, secondCiphertext, err := Encrypt(aliceSession, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if secondType != 1 {
		t.Fatalf("second message type = %d, want 1", secondType)
	}
	plaintext, err = Decrypt(bobSession, secondCiphertext, secondType)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "second" {
		t.Fatalf("decrypted %q", plaintext)
	}
	if _, err := Decrypt(bobSession, secondCiphertext, secondType); err == nil {
		t.Fatal("expected replay to fail")
	}
}

func TestStateStore(t *testing.T) {
	acc, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	key := bytesOf(0x09, pickleKeyBytes)
	pickled, err := PickleAccount(acc, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store := NewStore(path)
	original := &State{
		UserID:        uuid.NewString(),
		DeviceID:      uuid.NewString(),
		AccountPickle: pickled,
		Sessions:      map[string]string{"peer:device": "session"},
		PendingAcks:   map[string]int64{"message": 3},
		InboxAfter:    2,
	}
	if err := store.Save(original); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state file mode = %o", info.Mode().Perm())
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InboxAfter != 2 || loaded.Sessions["peer:device"] != "session" || loaded.PendingAcks["message"] != 3 {
		t.Fatalf("unexpected loaded state %+v", loaded)
	}
	if _, err := UnpickleAccount(loaded.AccountPickle, key); err != nil {
		t.Fatalf("unpickle account from state: %v", err)
	}
}

func TestEncryptRejectsLargePlaintext(t *testing.T) {
	acc, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := GenerateOneTimeKeys(acc, 1)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := IdentityKeys(acc)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewOutboundSession(acc, identity, keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Encrypt(sess, make([]byte, maxCiphertextBytes+1)); err == nil {
		t.Fatal("expected oversized plaintext to fail")
	}
}

func TestSendRequiresVerifiedFingerprint(t *testing.T) {
	relay := newRelay()
	server := newTestServer(t, relay)
	defer server.Close()

	aliceUser := uuid.NewString()
	bobUser := uuid.NewString()
	alice := newTestService(t, server.URL, aliceUser)
	bob := newTestService(t, server.URL, bobUser)
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
	if _, err := bob.UploadKeys(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Send(t.Context(), bobUser, "blocked", ""); !errors.Is(err, ErrPeerNotVerified) {
		t.Fatalf("Send error = %v, want ErrPeerNotVerified", err)
	}
	info, err := alice.PeerInfo(t.Context(), bobUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Verify(t.Context(), bobUser, info.DeviceID, info.FormattedFingerprint); err != nil {
		t.Fatal(err)
	}
	result, err := alice.Send(t.Context(), bobUser, "allowed", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Sequence == 0 {
		t.Fatal("expected a positive sequence")
	}
	if _, err := relay.message(result.MessageID); err != nil {
		t.Fatalf("relay did not store message: %v", err)
	}
}

func TestBaseURLRequiresHTTPSForRemoteHosts(t *testing.T) {
	if _, err := NewClient("http://example.com", "token"); err == nil {
		t.Fatal("http to a non-loopback host must be rejected")
	}
	if _, err := NewClient("http://127.0.0.1:8080", "token"); err != nil {
		t.Fatalf("loopback http was rejected: %v", err)
	}
	if _, err := NewClient("https://example.com", "token"); err != nil {
		t.Fatalf("https was rejected: %v", err)
	}
}

func TestVerifyRejectsRotationUntilForced(t *testing.T) {
	relay := newRelay()
	server := newTestServer(t, relay)
	defer server.Close()

	aliceUser := uuid.NewString()
	bobUser := uuid.NewString()
	alice, _ := setupPairedServices(t, server.URL, aliceUser, bobUser)

	initial, err := alice.PeerInfo(t.Context(), bobUser)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := NewAccount()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := IdentityKeys(acc)
	if err != nil {
		t.Fatal(err)
	}
	relay.mu.Lock()
	relay.devices[bobUser].Curve = identity.Curve25519
	relay.devices[bobUser].Ed = identity.Ed25519
	relay.mu.Unlock()

	rotated, err := alice.PeerInfo(t.Context(), bobUser)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Fingerprint == initial.Fingerprint {
		t.Fatal("rotated identity produced the same fingerprint")
	}
	if _, err := alice.Verify(t.Context(), bobUser, rotated.DeviceID, rotated.FormattedFingerprint); !errors.Is(err, ErrFingerprintChanged) {
		t.Fatalf("Verify error = %v, want ErrFingerprintChanged", err)
	}
	if _, err := alice.ForceVerify(t.Context(), bobUser, rotated.DeviceID, rotated.FormattedFingerprint); err != nil {
		t.Fatalf("ForceVerify failed: %v", err)
	}
}

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for i := range out {
		out[i] = value
	}
	return out
}
