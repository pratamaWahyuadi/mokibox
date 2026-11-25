package dm

import (
	"crypto/ed25519"
	"fmt"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/crypto/goolm/account"
	"maunium.net/go/mautrix/crypto/goolm/session"
	"maunium.net/go/mautrix/id"
)

const oneTimeKeySignaturePrefix = "mokibox-dm-one-time-key-v1"

type Identity struct {
	Curve25519 string `json:"identity_curve25519"`
	Ed25519    string `json:"identity_ed25519"`
}

type OneTimeKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

func NewAccount() (*account.Account, error) {
	acc, err := account.NewAccount()
	if err != nil {
		return nil, fmt.Errorf("create olm account: %w", err)
	}
	return acc, nil
}

func IdentityKeys(acc *account.Account) (Identity, error) {
	if acc == nil {
		return Identity{}, fmt.Errorf("olm account is nil")
	}
	ed, curve, err := acc.IdentityKeys()
	if err != nil {
		return Identity{}, fmt.Errorf("read olm identity keys: %w", err)
	}
	return Identity{Curve25519: string(curve), Ed25519: string(ed)}, nil
}

func PickleAccount(acc *account.Account, key []byte) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("olm account is nil")
	}
	pickled, err := acc.Pickle(key)
	if err != nil {
		return "", fmt.Errorf("pickle olm account: %w", err)
	}
	return encodeBase64(pickled), nil
}

func UnpickleAccount(value string, key []byte) (*account.Account, error) {
	raw, err := decodeBase64(value, 0)
	if err != nil {
		return nil, fmt.Errorf("decode olm account pickle: %w", err)
	}
	acc, err := account.AccountFromPickled(raw, key)
	if err != nil {
		return nil, fmt.Errorf("unpickle olm account: %w", err)
	}
	return acc, nil
}

func GenerateOneTimeKeys(acc *account.Account, count uint) ([]OneTimeKey, error) {
	if acc == nil {
		return nil, fmt.Errorf("olm account is nil")
	}
	maxKeys := uint(account.MaxOneTimeKeys)
	if count == 0 || count > maxKeys {
		return nil, fmt.Errorf("one-time key count must be between 1 and %d", maxKeys)
	}
	if err := acc.GenOneTimeKeys(count); err != nil {
		return nil, fmt.Errorf("generate one-time keys: %w", err)
	}
	keys, err := acc.OneTimeKeys()
	if err != nil {
		return nil, fmt.Errorf("read one-time keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no unpublished one-time keys available")
	}
	out := make([]OneTimeKey, 0, len(keys))
	for keyID, publicKey := range keys {
		public, err := normalizeBase64(string(publicKey), 32)
		if err != nil {
			return nil, fmt.Errorf("invalid generated one-time key: %w", err)
		}
		payload := []byte(oneTimeKeySignaturePrefix + "\n" + keyID + "\n" + public)
		signature, err := acc.Sign(payload)
		if err != nil {
			return nil, fmt.Errorf("sign one-time key: %w", err)
		}
		signed, err := normalizeBase64(string(signature), ed25519.SignatureSize)
		if err != nil {
			return nil, fmt.Errorf("invalid one-time key signature: %w", err)
		}
		out = append(out, OneTimeKey{KeyID: keyID, PublicKey: public, Signature: signed})
	}
	return out, nil
}

func GenerateFallbackKey(acc *account.Account) (*OneTimeKey, error) {
	if acc == nil {
		return nil, fmt.Errorf("olm account is nil")
	}
	if err := acc.GenFallbackKey(); err != nil {
		return nil, fmt.Errorf("generate olm fallback key: %w", err)
	}
	keys := acc.FallbackKeyUnpublished()
	if len(keys) != 1 {
		return nil, fmt.Errorf("expected one unpublished fallback key, got %d", len(keys))
	}
	for keyID, publicKey := range keys {
		public, err := normalizeBase64(string(publicKey), 32)
		if err != nil {
			return nil, fmt.Errorf("invalid generated fallback key: %w", err)
		}
		payload := []byte(oneTimeKeySignaturePrefix + "\n" + keyID + "\n" + public)
		signature, err := acc.Sign(payload)
		if err != nil {
			return nil, fmt.Errorf("sign fallback key: %w", err)
		}
		signed, err := normalizeBase64(string(signature), ed25519.SignatureSize)
		if err != nil {
			return nil, fmt.Errorf("invalid fallback key signature: %w", err)
		}
		return &OneTimeKey{KeyID: keyID, PublicKey: public, Signature: signed}, nil
	}
	return nil, fmt.Errorf("no unpublished fallback key available")
}

func VerifyOneTimeKeySignature(identityEd25519 string, key OneTimeKey) error {
	public, err := decodeBase64(identityEd25519, ed25519.PublicKeySize)
	if err != nil {
		return fmt.Errorf("invalid peer ed25519 identity key: %w", err)
	}
	keyPublic, err := normalizeBase64(key.PublicKey, 32)
	if err != nil {
		return fmt.Errorf("invalid peer one-time key: %w", err)
	}
	signature, err := decodeBase64(key.Signature, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("invalid peer one-time key signature: %w", err)
	}
	payload := []byte(oneTimeKeySignaturePrefix + "\n" + key.KeyID + "\n" + keyPublic)
	if !ed25519.Verify(ed25519.PublicKey(public), payload, signature) {
		return fmt.Errorf("peer one-time key signature does not match identity key")
	}
	return nil
}

func NewOutboundSession(acc *account.Account, identity Identity, key OneTimeKey) (*session.OlmSession, error) {
	curve, err := normalizeBase64(identity.Curve25519, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid peer curve25519 identity key: %w", err)
	}
	oneTime, err := normalizeBase64(key.PublicKey, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid peer one-time key: %w", err)
	}
	created, err := acc.NewOutboundSession(id.Curve25519(curve), id.Curve25519(oneTime))
	if err != nil {
		return nil, fmt.Errorf("create outbound olm session: %w", err)
	}
	result, ok := created.(*session.OlmSession)
	if !ok {
		return nil, fmt.Errorf("unexpected outbound olm session type %T", created)
	}
	return result, nil
}

func NewInboundSession(acc *account.Account, senderCurve25519, ciphertext string) (*session.OlmSession, error) {
	sender, err := normalizeBase64(senderCurve25519, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid sender curve25519 identity key: %w", err)
	}
	raw, err := decodeBase64(ciphertext, 0)
	if err != nil {
		return nil, fmt.Errorf("invalid olm ciphertext: %w", err)
	}
	if len(raw) > maxCiphertextBytes {
		return nil, fmt.Errorf("olm ciphertext exceeds %d bytes", maxCiphertextBytes)
	}
	identity := id.Curve25519(sender)
	created, err := acc.NewInboundSessionFrom(&identity, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: create inbound session: %v", ErrInvalidCiphertext, err)
	}
	result, ok := created.(*session.OlmSession)
	if !ok {
		return nil, fmt.Errorf("unexpected inbound olm session type %T", created)
	}
	if err := acc.RemoveOneTimeKeys(result); err != nil {
		return nil, fmt.Errorf("remove consumed one-time key: %w", err)
	}
	return result, nil
}

func PickleSession(sess *session.OlmSession, key []byte) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("olm session is nil")
	}
	pickled, err := sess.Pickle(key)
	if err != nil {
		return "", fmt.Errorf("pickle olm session: %w", err)
	}
	return encodeBase64(pickled), nil
}

func UnpickleSession(value string, key []byte) (*session.OlmSession, error) {
	raw, err := decodeBase64(value, 0)
	if err != nil {
		return nil, fmt.Errorf("decode olm session pickle: %w", err)
	}
	sess, err := session.OlmSessionFromPickled(raw, key)
	if err != nil {
		return nil, fmt.Errorf("unpickle olm session: %w", err)
	}
	return sess, nil
}

func MatchesInboundSession(sess *session.OlmSession, senderCurve25519, ciphertext string) (bool, error) {
	sender, err := normalizeBase64(senderCurve25519, 32)
	if err != nil {
		return false, fmt.Errorf("invalid sender curve25519 identity key: %w", err)
	}
	matches, err := sess.MatchesInboundSessionFrom(sender, ciphertext)
	if err != nil {
		return false, fmt.Errorf("%w: match inbound session: %v", ErrInvalidCiphertext, err)
	}
	return matches, nil
}

func Encrypt(sess *session.OlmSession, plaintext []byte) (int16, string, error) {
	if len(plaintext) == 0 {
		return 0, "", fmt.Errorf("plaintext is empty")
	}
	if len(plaintext) > maxCiphertextBytes {
		return 0, "", fmt.Errorf("plaintext exceeds %d bytes", maxCiphertextBytes)
	}
	msgType, ciphertext, err := sess.Encrypt(plaintext)
	if err != nil {
		return 0, "", fmt.Errorf("encrypt olm message: %w", err)
	}
	raw, err := decodeBase64(string(ciphertext), 0)
	if err != nil {
		return 0, "", fmt.Errorf("encode olm ciphertext: %w", err)
	}
	if len(raw) == 0 || len(raw) > maxCiphertextBytes {
		return 0, "", fmt.Errorf("olm ciphertext size %d is invalid", len(raw))
	}
	return int16(msgType), string(ciphertext), nil
}

func Decrypt(sess *session.OlmSession, ciphertext string, msgType int16) ([]byte, error) {
	var olmType id.OlmMsgType
	switch msgType {
	case 0:
		olmType = id.OlmMsgTypePreKey
	case 1:
		olmType = id.OlmMsgTypeMsg
	default:
		return nil, fmt.Errorf("unsupported olm message type %d", msgType)
	}
	plaintext, err := sess.Decrypt(ciphertext, olmType)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOlmDecrypt, err)
	}
	return plaintext, nil
}

func NewDeviceID() (string, error) {
	id := uuid.New()
	if id == uuid.Nil {
		return "", fmt.Errorf("generated nil device id")
	}
	return id.String(), nil
}

func NewMessageID() (string, error) {
	id := uuid.New()
	if id == uuid.Nil {
		return "", fmt.Errorf("generated nil message id")
	}
	return id.String(), nil
}
