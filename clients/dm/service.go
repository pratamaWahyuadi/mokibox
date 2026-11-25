package dm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/crypto/goolm/account"
	"maunium.net/go/mautrix/crypto/goolm/session"
)

type Service struct {
	Client    *Client
	Store     *Store
	Trust     *Trust
	mu        sync.Mutex
	pickleKey []byte
}

func NewService(client *Client, store *Store, trust *Trust, pickleKey []byte) (*Service, error) {
	if client == nil {
		return nil, fmt.Errorf("client is nil")
	}
	if store == nil {
		return nil, fmt.Errorf("store is nil")
	}
	if trust == nil {
		return nil, fmt.Errorf("trust store is nil")
	}
	if len(pickleKey) != pickleKeyBytes {
		return nil, fmt.Errorf("pickle key must be %d bytes", pickleKeyBytes)
	}
	if err := trust.Load(); err != nil {
		return nil, err
	}
	key := append([]byte(nil), pickleKey...)
	return &Service{Client: client, Store: store, Trust: trust, pickleKey: key}, nil
}

func (s *Service) Init(userID string) (*State, Identity, error) {
	if _, err := uuid.Parse(userID); err != nil {
		return nil, Identity{}, fmt.Errorf("invalid user id: %w", err)
	}
	if s.Store.Exists() {
		return nil, Identity{}, fmt.Errorf("state file already exists; refusing to overwrite")
	}
	deviceID, err := NewDeviceID()
	if err != nil {
		return nil, Identity{}, err
	}
	acc, err := NewAccount()
	if err != nil {
		return nil, Identity{}, err
	}
	identity, err := IdentityKeys(acc)
	if err != nil {
		return nil, Identity{}, err
	}
	pickled, err := PickleAccount(acc, s.pickleKey)
	if err != nil {
		return nil, Identity{}, err
	}
	st := &State{
		Version:       stateVersion,
		UserID:        userID,
		DeviceID:      deviceID,
		AccountPickle: pickled,
		Sessions:      make(map[string]string),
		PendingAcks:   make(map[string]int64),
		InboxAfter:    0,
	}
	if err := s.Store.Save(st); err != nil {
		return nil, Identity{}, err
	}
	return st, identity, nil
}

func (s *Service) load() (*State, *account.Account, error) {
	st, err := s.Store.Load()
	if err != nil {
		return nil, nil, err
	}
	acc, err := UnpickleAccount(st.AccountPickle, s.pickleKey)
	if err != nil {
		return nil, nil, err
	}
	return st, acc, nil
}

func (s *Service) Identity() (Identity, error) {
	acc, err := s.loadAccountOnly()
	if err != nil {
		return Identity{}, err
	}
	return IdentityKeys(acc)
}

func (s *Service) loadAccountOnly() (*account.Account, error) {
	st, err := s.Store.Load()
	if err != nil {
		return nil, err
	}
	return UnpickleAccount(st.AccountPickle, s.pickleKey)
}

func (s *Service) Register(ctx context.Context) (RegisterDeviceResult, error) {
	st, acc, err := s.load()
	if err != nil {
		return RegisterDeviceResult{}, err
	}
	identity, err := IdentityKeys(acc)
	if err != nil {
		return RegisterDeviceResult{}, err
	}
	return s.Client.RegisterDevice(ctx, st.DeviceID, identity)
}

func (s *Service) UploadKeys(ctx context.Context, count uint) (int, error) {
	st, acc, err := s.load()
	if err != nil {
		return 0, err
	}
	keys, err := GenerateOneTimeKeys(acc, count)
	if err != nil {
		return 0, err
	}
	st.AccountPickle, err = PickleAccount(acc, s.pickleKey)
	if err != nil {
		return 0, err
	}
	if err := s.Store.Save(st); err != nil {
		return 0, err
	}
	accepted, err := s.Client.UploadOneTimeKeys(ctx, st.DeviceID, keys)
	if err != nil {
		return 0, err
	}
	if accepted != len(keys) {
		return accepted, fmt.Errorf("server accepted %d of %d one-time keys", accepted, len(keys))
	}
	acc.MarkKeysAsPublished()
	st.AccountPickle, err = PickleAccount(acc, s.pickleKey)
	if err != nil {
		return accepted, err
	}
	if err := s.Store.Save(st); err != nil {
		return accepted, err
	}
	return accepted, nil
}

type PeerInfo struct {
	UserID               string
	DeviceID             string
	Identity             Identity
	Fingerprint          string
	FormattedFingerprint string
}

func (s *Service) PeerInfo(ctx context.Context, userID string) (PeerInfo, error) {
	st, err := s.Store.Load()
	if err != nil {
		return PeerInfo{}, err
	}
	bundle, err := s.Client.GetKeyBundle(ctx, st.DeviceID, userID, false)
	if err != nil {
		return PeerInfo{}, err
	}
	return s.peerInfoFromBundle(bundle)
}

func (s *Service) peerInfoFromBundle(bundle KeyBundle) (PeerInfo, error) {
	fingerprint, err := Fingerprint(bundle.UserID, bundle.DeviceID, bundle.Identity.Curve25519, bundle.Identity.Ed25519)
	if err != nil {
		return PeerInfo{}, err
	}
	return PeerInfo{
		UserID:               bundle.UserID,
		DeviceID:             bundle.DeviceID,
		Identity:             bundle.Identity,
		Fingerprint:          fingerprint,
		FormattedFingerprint: FormatFingerprint(fingerprint),
	}, nil
}

func (s *Service) Verify(ctx context.Context, userID, deviceID, fingerprint string) (PeerInfo, error) {
	return s.verify(ctx, userID, deviceID, fingerprint, false)
}

func (s *Service) ForceVerify(ctx context.Context, userID, deviceID, fingerprint string) (PeerInfo, error) {
	return s.verify(ctx, userID, deviceID, fingerprint, true)
}

func (s *Service) verify(ctx context.Context, userID, deviceID, fingerprint string, force bool) (PeerInfo, error) {
	normalized := NormalizeFingerprint(fingerprint)
	if len(normalized) != 64 {
		return PeerInfo{}, fmt.Errorf("%w: expected 64 hex characters", ErrInvalidTrust)
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return PeerInfo{}, fmt.Errorf("%w: %v", ErrInvalidTrust, err)
	}
	info, err := s.PeerInfo(ctx, userID)
	if err != nil {
		return PeerInfo{}, err
	}
	if info.DeviceID != deviceID {
		return PeerInfo{}, fmt.Errorf("%w: target device changed", ErrFingerprintChanged)
	}
	if info.Fingerprint != normalized {
		return PeerInfo{}, fmt.Errorf("%w: supplied fingerprint does not match key bundle", ErrInvalidTrust)
	}
	if pinned, ok := s.Trust.Get(info.UserID, info.DeviceID); ok && pinned != normalized && !force {
		return PeerInfo{}, fmt.Errorf("%w: pinned %s; supplied %s; use ForceVerify to rotate", ErrFingerprintChanged, pinned, normalized)
	}
	if err := s.Trust.Put(info.UserID, info.DeviceID, normalized, time.Now()); err != nil {
		return PeerInfo{}, err
	}
	return info, nil
}

func (s *Service) assertTrusted(bundle KeyBundle) (string, error) {
	info, err := s.peerInfoFromBundle(bundle)
	if err != nil {
		return "", err
	}
	trusted, ok := s.Trust.Get(info.UserID, info.DeviceID)
	if !ok {
		return "", fmt.Errorf("%w: %s / %s", ErrPeerNotVerified, info.UserID, info.DeviceID)
	}
	if trusted != info.Fingerprint {
		return "", fmt.Errorf("%w: %s / %s", ErrFingerprintChanged, info.UserID, info.DeviceID)
	}
	return info.Fingerprint, nil
}

func (s *Service) Send(ctx context.Context, recipientUserID, plaintext, clientMessageID string) (SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parsedRecipient, err := uuid.Parse(recipientUserID)
	if err != nil || parsedRecipient == uuid.Nil {
		return SendResult{}, fmt.Errorf("invalid recipient user id")
	}
	if clientMessageID == "" {
		var err error
		clientMessageID, err = NewMessageID()
		if err != nil {
			return SendResult{}, err
		}
	}
	parsedMessageID, err := uuid.Parse(clientMessageID)
	if err != nil || parsedMessageID == uuid.Nil {
		return SendResult{}, fmt.Errorf("invalid client message id")
	}
	st, _, err := s.load()
	if err != nil {
		return SendResult{}, err
	}
	identityBundle, err := s.Client.GetKeyBundle(ctx, st.DeviceID, recipientUserID, false)
	if err != nil {
		return SendResult{}, err
	}
	if _, err := s.assertTrusted(identityBundle); err != nil {
		return SendResult{}, err
	}
	key := peerKey(identityBundle.UserID, identityBundle.DeviceID)
	sess, err := s.sessionForSend(ctx, st, identityBundle, key)
	if err != nil {
		return SendResult{}, err
	}
	msgType, ciphertext, err := Encrypt(sess, []byte(plaintext))
	if err != nil {
		return SendResult{}, err
	}
	encodedSession, err := PickleSession(sess, s.pickleKey)
	if err != nil {
		return SendResult{}, err
	}
	st.Sessions[key] = encodedSession
	if err := s.Store.Save(st); err != nil {
		return SendResult{}, err
	}
	req := SendRequest{
		DeviceID:          st.DeviceID,
		RecipientUserID:   identityBundle.UserID,
		RecipientDeviceID: identityBundle.DeviceID,
		ClientMessageID:   clientMessageID,
		Protocol:          "olm",
		ProtocolVersion:   1,
		MessageType:       msgType,
		Ciphertext:        ciphertext,
	}
	return s.Client.SendMessage(ctx, req)
}

func (s *Service) sessionForSend(ctx context.Context, st *State, identityBundle KeyBundle, key string) (*session.OlmSession, error) {
	if encoded, ok := st.Sessions[key]; ok {
		return UnpickleSession(encoded, s.pickleKey)
	}
	acc, err := UnpickleAccount(st.AccountPickle, s.pickleKey)
	if err != nil {
		return nil, err
	}
	bundle, err := s.Client.GetKeyBundle(ctx, st.DeviceID, identityBundle.UserID, true)
	if err != nil {
		return nil, err
	}
	if bundle.UserID != identityBundle.UserID || bundle.DeviceID != identityBundle.DeviceID ||
		bundle.Identity.Curve25519 != identityBundle.Identity.Curve25519 ||
		bundle.Identity.Ed25519 != identityBundle.Identity.Ed25519 {
		return nil, fmt.Errorf("%w: recipient identity changed while fetching one-time key", ErrFingerprintChanged)
	}
	if _, err := s.assertTrusted(bundle); err != nil {
		return nil, err
	}
	if bundle.OneTimeKey == nil {
		return nil, fmt.Errorf("recipient has no one-time key available")
	}
	if err := VerifyOneTimeKeySignature(bundle.Identity.Ed25519, *bundle.OneTimeKey); err != nil {
		return nil, err
	}
	return NewOutboundSession(acc, bundle.Identity, *bundle.OneTimeKey)
}

type DecryptedMessage struct {
	MessageID        string
	SenderUserID     string
	SenderDeviceID   string
	Sequence         int64
	Plaintext        []byte
	AlreadyDelivered bool
}

func (s *Service) Receive(ctx context.Context, limit int) ([]DecryptedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100")
	}
	st, acc, err := s.load()
	if err != nil {
		return nil, err
	}
	result, err := s.Client.PollInbox(ctx, st.DeviceID, st.InboxAfter, limit)
	if err != nil {
		return nil, err
	}
	identityCache := make(map[string]KeyBundle)
	delivered := make([]DecryptedMessage, 0, len(result.Messages))
	for _, message := range result.Messages {
		if err := validateInboxMessage(st.DeviceID, message); err != nil {
			return delivered, err
		}
		key := peerKey(message.SenderUserID, message.SenderDeviceID)
		if _, pending := st.PendingAcks[message.MessageID]; pending {
			delivered = append(delivered, DecryptedMessage{
				MessageID: message.MessageID, SenderUserID: message.SenderUserID,
				SenderDeviceID: message.SenderDeviceID, Sequence: message.Sequence,
				AlreadyDelivered: true,
			})
		} else {
			plaintext, sess, err := s.decrypt(ctx, st, acc, message, identityCache, key)
			if err != nil {
				return delivered, err
			}
			encodedSession, err := PickleSession(sess, s.pickleKey)
			if err != nil {
				return delivered, err
			}
			encodedAccount, err := PickleAccount(acc, s.pickleKey)
			if err != nil {
				return delivered, err
			}
			st.Sessions[key] = encodedSession
			st.AccountPickle = encodedAccount
			st.PendingAcks[message.MessageID] = message.Sequence
			if err := s.Store.Save(st); err != nil {
				return delivered, err
			}
			delivered = append(delivered, DecryptedMessage{
				MessageID: message.MessageID, SenderUserID: message.SenderUserID,
				SenderDeviceID: message.SenderDeviceID, Sequence: message.Sequence,
				Plaintext: plaintext,
			})
		}
		if err := s.ack(ctx, st, message); err != nil {
			return delivered, err
		}
		delete(st.PendingAcks, message.MessageID)
		if message.Sequence > st.InboxAfter {
			st.InboxAfter = message.Sequence
		}
		if err := s.Store.Save(st); err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

func validateInboxMessage(deviceID string, message InboxMessage) error {
	messageID, err := uuid.Parse(message.MessageID)
	if err != nil || messageID == uuid.Nil {
		return fmt.Errorf("%w: invalid message id", ErrInvalidEnvelope)
	}
	if _, err := uuid.Parse(message.SenderUserID); err != nil {
		return fmt.Errorf("%w: invalid sender user id", ErrInvalidEnvelope)
	}
	if _, err := uuid.Parse(message.SenderDeviceID); err != nil {
		return fmt.Errorf("%w: invalid sender device id", ErrInvalidEnvelope)
	}
	if message.Protocol != "olm" || message.ProtocolVersion != 1 {
		return fmt.Errorf("%w: unsupported protocol envelope", ErrInvalidEnvelope)
	}
	if message.MessageType != 0 && message.MessageType != 1 {
		return fmt.Errorf("%w: unsupported message type %d", ErrInvalidEnvelope, message.MessageType)
	}
	if message.RecipientDeviceID != deviceID {
		return fmt.Errorf("%w: message addressed to another device", ErrInvalidEnvelope)
	}
	raw, err := decodeBase64(message.Ciphertext, 0)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
	}
	if len(raw) == 0 || len(raw) > maxCiphertextBytes {
		return fmt.Errorf("%w: ciphertext size %d is invalid", ErrInvalidEnvelope, len(raw))
	}
	return nil
}

func (s *Service) decrypt(ctx context.Context, st *State, acc *account.Account, message InboxMessage, cache map[string]KeyBundle, key string) ([]byte, *session.OlmSession, error) {
	bundle, ok := cache[key]
	if !ok {
		var err error
		bundle, err = s.Client.GetKeyBundle(ctx, st.DeviceID, message.SenderUserID, false)
		if err != nil {
			return nil, nil, err
		}
		if bundle.DeviceID != message.SenderDeviceID {
			return nil, nil, fmt.Errorf("%w: sender device changed", ErrFingerprintChanged)
		}
		if _, err := s.assertTrusted(bundle); err != nil {
			return nil, nil, err
		}
		cache[key] = bundle
	}
	if message.MessageType == 1 {
		encoded, ok := st.Sessions[key]
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", ErrNoSession, key)
		}
		sess, err := UnpickleSession(encoded, s.pickleKey)
		if err != nil {
			return nil, nil, err
		}
		plaintext, err := Decrypt(sess, message.Ciphertext, message.MessageType)
		if err != nil {
			return nil, nil, err
		}
		return plaintext, sess, nil
	}
	if encoded, ok := st.Sessions[key]; ok {
		sess, err := UnpickleSession(encoded, s.pickleKey)
		if err != nil {
			return nil, nil, err
		}
		matches, err := MatchesInboundSession(sess, bundle.Identity.Curve25519, message.Ciphertext)
		if err != nil {
			return nil, nil, err
		}
		if matches {
			plaintext, err := Decrypt(sess, message.Ciphertext, message.MessageType)
			if err != nil {
				return nil, nil, err
			}
			return plaintext, sess, nil
		}
	}
	sess, err := NewInboundSession(acc, bundle.Identity.Curve25519, message.Ciphertext)
	if err != nil {
		return nil, nil, err
	}
	plaintext, err := Decrypt(sess, message.Ciphertext, message.MessageType)
	if err != nil {
		return nil, nil, err
	}
	return plaintext, sess, nil
}

func (s *Service) ack(ctx context.Context, st *State, message InboxMessage) error {
	err := s.Client.Acknowledge(ctx, st.DeviceID, message.MessageID)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}
