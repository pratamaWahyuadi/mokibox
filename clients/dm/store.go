package dm

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	stateVersion    = 2
	oldStateVersion = 1
	trustVersion    = 1
	pickleKeyBytes  = 32
	outboxTTL       = 7 * 24 * time.Hour
	quarantineTTL   = 7 * 24 * time.Hour
)

type OutboxEntry struct {
	Request       SendRequest `json:"request"`
	PlaintextHash string      `json:"plaintext_hash"`
	ExpiresAt     time.Time   `json:"expires_at"`
	Result        *SendResult `json:"result,omitempty"`
}

type QuarantineEntry struct {
	Sequence       int64     `json:"sequence"`
	SenderUserID   string    `json:"sender_user_id"`
	SenderDeviceID string    `json:"sender_device_id"`
	Reason         string    `json:"reason"`
	Ciphertext     string    `json:"ciphertext"`
	FailedAt       time.Time `json:"failed_at"`
}

type State struct {
	Version       int                         `json:"version"`
	UserID        string                      `json:"user_id"`
	DeviceID      string                      `json:"device_id"`
	AccountPickle string                      `json:"account_pickle"`
	Sessions      map[string]string           `json:"sessions"`
	PendingAcks   map[string]int64            `json:"pending_acks"`
	InboxAfter    int64                       `json:"inbox_after"`
	Outbox        map[string]*OutboxEntry     `json:"outbox"`
	Quarantine    map[string]*QuarantineEntry `json:"quarantine"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if st.Version != stateVersion && st.Version != oldStateVersion {
		return nil, fmt.Errorf("unsupported state version %d", st.Version)
	}
	if st.UserID == "" || st.DeviceID == "" || st.AccountPickle == "" {
		return nil, fmt.Errorf("state is missing device identity")
	}
	if st.Sessions == nil {
		st.Sessions = make(map[string]string)
	}
	if st.PendingAcks == nil {
		st.PendingAcks = make(map[string]int64)
	}
	if st.Outbox == nil {
		st.Outbox = make(map[string]*OutboxEntry)
	}
	if st.Quarantine == nil {
		st.Quarantine = make(map[string]*QuarantineEntry)
	}
	st.Version = stateVersion
	if st.InboxAfter < 0 {
		return nil, fmt.Errorf("state has invalid inbox cursor")
	}
	return &st, nil
}

func (s *Store) Save(st *State) error {
	if st == nil {
		return fmt.Errorf("state is nil")
	}
	st.Version = stateVersion
	if st.Outbox == nil {
		st.Outbox = make(map[string]*OutboxEntry)
	}
	if st.Quarantine == nil {
		st.Quarantine = make(map[string]*QuarantineEntry)
	}
	now := time.Now()
	for messageID, entry := range st.Outbox {
		if entry != nil && !entry.ExpiresAt.After(now) {
			delete(st.Outbox, messageID)
		}
	}
	for messageID, entry := range st.Quarantine {
		if entry != nil && !entry.FailedAt.Add(quarantineTTL).After(now) {
			delete(st.Quarantine, messageID)
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	raw = append(raw, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	return writePrivateFile(s.path, raw)
}

func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

type TrustEntry struct {
	Fingerprint string    `json:"fingerprint"`
	VerifiedAt  time.Time `json:"verified_at"`
}

type TrustStore struct {
	Version int                   `json:"version"`
	Peers   map[string]TrustEntry `json:"peers"`
}

type Trust struct {
	path string
	mu   sync.Mutex
	data TrustStore
}

func NewTrust(path string) *Trust {
	return &Trust{path: path, data: TrustStore{Version: trustVersion, Peers: make(map[string]TrustEntry)}}
}

func (t *Trust) Load() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		t.data = TrustStore{Version: trustVersion, Peers: make(map[string]TrustEntry)}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trust store: %w", err)
	}
	var data TrustStore
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("decode trust store: %w", err)
	}
	if data.Version != trustVersion {
		return fmt.Errorf("unsupported trust store version %d", data.Version)
	}
	if data.Peers == nil {
		data.Peers = make(map[string]TrustEntry)
	}
	t.data = data
	return nil
}

func (t *Trust) Get(userID, deviceID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.data.Peers[peerKey(userID, deviceID)]
	if !ok {
		return "", false
	}
	return entry.Fingerprint, true
}

func (t *Trust) Put(userID, deviceID, fingerprint string, verifiedAt time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := TrustStore{Version: t.data.Version, Peers: make(map[string]TrustEntry, len(t.data.Peers)+1)}
	for key, entry := range t.data.Peers {
		next.Peers[key] = entry
	}
	next.Peers[peerKey(userID, deviceID)] = TrustEntry{Fingerprint: fingerprint, VerifiedAt: verifiedAt.UTC()}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust store: %w", err)
	}
	raw = append(raw, '\n')
	if err := writePrivateFile(t.path, raw); err != nil {
		return err
	}
	t.data = next
	return nil
}

func (t *Trust) List() map[string]TrustEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]TrustEntry, len(t.data.Peers))
	for key, entry := range t.data.Peers {
		out[key] = entry
	}
	return out
}

func peerKey(userID, deviceID string) string {
	return userID + ":" + deviceID
}

func writePrivateFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if info, err := os.Stat(dir); err == nil && info.IsDir() && info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("set state directory permissions: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".dm-state-")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("set state file permissions: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	return nil
}

func GeneratePickleKey() (string, error) {
	key := make([]byte, pickleKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate pickle key: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(key), nil
}

func ParsePickleKey(value string) ([]byte, error) {
	key, err := decodeBase64(value, pickleKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("pickle key must be base64 of %d bytes: %w", pickleKeyBytes, err)
	}
	return key, nil
}
