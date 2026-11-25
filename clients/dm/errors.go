package dm

import "errors"

var (
	ErrPeerNotVerified    = errors.New("peer fingerprint is not verified")
	ErrFingerprintChanged = errors.New("peer fingerprint changed")
	ErrNoSession          = errors.New("no olm session for peer")
	ErrInvalidEnvelope    = errors.New("invalid olm envelope")
	ErrInvalidTrust       = errors.New("invalid fingerprint")
	ErrInvalidCiphertext  = errors.New("invalid olm ciphertext")
	ErrOlmDecrypt         = errors.New("olm decrypt failed")
	ErrOutboxConflict     = errors.New("outbox entry does not match retry")
)
