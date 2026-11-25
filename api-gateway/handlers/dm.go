package handlers

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const (
	dmRequestBodyLimit = 32 << 10
	dmMaxCiphertext    = 16 << 10
	dmMaxInboxPending  = 1000
	dmMaxPollLimit     = 100
)

type dmStore interface {
	GetDMDeviceByUser(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetDMDeviceByUserAndID(ctx context.Context, arg db.GetDMDeviceByUserAndIDParams) (db.DmDevice, error)
	GetDMTargetDevice(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	IsDMMutualFollow(ctx context.Context, arg db.IsDMMutualFollowParams) (bool, error)
	InsertDMOneTimeKey(ctx context.Context, arg db.InsertDMOneTimeKeyParams) (int64, error)
	GetDMFallbackKey(ctx context.Context, deviceID uuid.UUID) (db.DmFallbackKey, error)
	ClaimDMOneTimeKey(ctx context.Context, deviceID uuid.UUID) (db.ClaimDMOneTimeKeyRow, error)
	GetDMIdempotency(ctx context.Context, arg db.GetDMIdempotencyParams) (db.GetDMIdempotencyRow, error)
	ListPendingDMInbox(ctx context.Context, arg db.ListPendingDMInboxParams) ([]db.ListPendingDMInboxRow, error)
	AcknowledgeDMInboxMessage(ctx context.Context, arg db.AcknowledgeDMInboxMessageParams) (int64, error)
	GetDMInboxMessageForDevice(ctx context.Context, arg db.GetDMInboxMessageForDeviceParams) (uuid.UUID, error)
}

type dmTxStore interface {
	dmStore
	LockActiveDMUser(ctx context.Context, userID uuid.UUID) (uuid.UUID, error)
	GetDMDeviceForUpdate(ctx context.Context, arg db.GetDMDeviceForUpdateParams) (db.DmDevice, error)
	DeleteDMDevicesByUser(ctx context.Context, userID uuid.UUID) (int64, error)
	CreateDMDevice(ctx context.Context, arg db.CreateDMDeviceParams) (db.DmDevice, error)
	CreateDMIdempotency(ctx context.Context, arg db.CreateDMIdempotencyParams) (db.CreateDMIdempotencyRow, error)
	UpsertDMFallbackKey(ctx context.Context, arg db.UpsertDMFallbackKeyParams) (db.DmFallbackKey, error)
	CountPendingDMInbox(ctx context.Context, recipientDeviceID uuid.UUID) (int64, error)
	AllocateDMSequence(ctx context.Context, deviceID uuid.UUID) (int64, error)
	CreateDMInboxMessage(ctx context.Context, arg db.CreateDMInboxMessageParams) (db.CreateDMInboxMessageRow, error)
	SetDMIdempotencySequence(ctx context.Context, arg db.SetDMIdempotencySequenceParams) (int64, error)
}

type DMHandler struct {
	Queries dmStore
	Run     txRunner[dmTxStore]
	TTL     time.Duration
}

func NewDMHandler(queries *db.Queries, dbHandle *sql.DB, ttl time.Duration) (*DMHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewDMHandler: queries is nil")
	}
	if dbHandle == nil {
		return nil, fmt.Errorf("NewDMHandler: db is nil")
	}
	if ttl != 168*time.Hour {
		return nil, fmt.Errorf("NewDMHandler: inbox TTL must be 168h")
	}
	return &DMHandler{
		Queries: queries,
		Run: NewSQLTxRunner(dbHandle, func(tx *sql.Tx) dmTxStore {
			return queries.WithTx(tx)
		}),
		TTL: ttl,
	}, nil
}

func newDMHandlerForTest(queries dmStore, runner txRunner[dmTxStore], ttl time.Duration) *DMHandler {
	return &DMHandler{Queries: queries, Run: runner, TTL: ttl}
}

type dmRequestEnvelope[T any] struct {
	Data T `json:"data"`
}

type dmRegisterDeviceRequest struct {
	DeviceID           string `json:"device_id"`
	IdentityCurve25519 string `json:"identity_curve25519"`
	IdentityEd25519    string `json:"identity_ed25519"`
}

type dmRegisterDeviceResponse struct {
	DeviceID       uuid.UUID `json:"device_id"`
	ReplacedDevice bool      `json:"replaced_device"`
}

func (h *DMHandler) RegisterDevice(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req dmRegisterDeviceRequest
	if err := bindDMBody(c, &req); err != nil {
		return err
	}
	deviceID, err := parseDMUUID(req.DeviceID, "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	curveKey, err := decodeDMKey(req.IdentityCurve25519, 32)
	if err != nil {
		return dmValidation(c, "identity_curve25519", "must be a base64-encoded 32-byte public key")
	}
	edKey, err := decodeDMKey(req.IdentityEd25519, 32)
	if err != nil {
		return dmValidation(c, "identity_ed25519", "must be a base64-encoded 32-byte public key")
	}

	replaced := false
	err = h.Run.Run(c.Request().Context(), func(q dmTxStore) error {
		if _, err := q.LockActiveDMUser(c.Request().Context(), user.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrUnauthorized, "user is inactive")
			}
			return dmStoreError("lock user for device registration", err)
		}

		current, err := q.GetDMDeviceByUser(c.Request().Context(), user.ID)
		switch {
		case err == nil:
			if current.DeviceID == deviceID {
				if current.IdentityCurve25519 != curveKey || current.IdentityEd25519 != edKey {
					return shared.Wrap(shared.ErrDMDeviceStale, "identity key rotation requires a new device_id")
				}
				return nil
			}
			if _, err := q.DeleteDMDevicesByUser(c.Request().Context(), user.ID); err != nil {
				return dmStoreError("replace DM device", err)
			}
			replaced = true
		case errors.Is(err, sql.ErrNoRows):
		default:
			return dmStoreError("read existing DM device", err)
		}

		_, err = q.CreateDMDevice(c.Request().Context(), db.CreateDMDeviceParams{
			DeviceID:           deviceID,
			UserID:             user.ID,
			IdentityCurve25519: curveKey,
			IdentityEd25519:    edKey,
		})
		if err != nil {
			return dmStoreError("create DM device", err)
		}
		return nil
	})
	if err != nil {
		return shared.RespondError(c, err)
	}
	return c.JSON(http.StatusOK, dmRequestEnvelope[dmRegisterDeviceResponse]{
		Data: dmRegisterDeviceResponse{DeviceID: deviceID, ReplacedDevice: replaced},
	})
}

type dmOneTimeKeyInput struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

type dmUploadKeysRequest struct {
	DeviceID    string              `json:"device_id"`
	Keys        []dmOneTimeKeyInput `json:"keys"`
	FallbackKey *dmOneTimeKeyInput  `json:"fallback_key"`
}

type dmUploadKeysResponse struct {
	Accepted int `json:"accepted"`
}

func (h *DMHandler) UploadOneTimeKeys(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req dmUploadKeysRequest
	if err := bindDMBody(c, &req); err != nil {
		return err
	}
	deviceID, err := parseDMUUID(req.DeviceID, "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	if len(req.Keys) == 0 || len(req.Keys) > 100 {
		return dmValidation(c, "keys", "must contain between 1 and 100 keys")
	}

	type normalizedKey struct {
		keyID     string
		publicKey string
		signature string
	}
	keys := make([]normalizedKey, 0, len(req.Keys))
	seen := make(map[string]struct{}, len(req.Keys))
	for i, key := range req.Keys {
		if len(key.KeyID) == 0 || len(key.KeyID) > 128 {
			return dmValidation(c, fmt.Sprintf("keys[%d].key_id", i), "must contain 1 to 128 characters")
		}
		if _, exists := seen[key.KeyID]; exists {
			return dmValidation(c, fmt.Sprintf("keys[%d].key_id", i), "must be unique in the request")
		}
		seen[key.KeyID] = struct{}{}
		publicKey, err := decodeDMKey(key.PublicKey, 32)
		if err != nil {
			return dmValidation(c, fmt.Sprintf("keys[%d].public_key", i), "must be a base64-encoded 32-byte public key")
		}
		signature, err := decodeDMKey(key.Signature, 64)
		if err != nil {
			return dmValidation(c, fmt.Sprintf("keys[%d].signature", i), "must be a base64-encoded 64-byte signature")
		}
		keys = append(keys, normalizedKey{keyID: key.KeyID, publicKey: publicKey, signature: signature})
	}
	var fallback *normalizedKey
	if req.FallbackKey != nil {
		if len(req.FallbackKey.KeyID) == 0 || len(req.FallbackKey.KeyID) > 128 {
			return dmValidation(c, "fallback_key.key_id", "must contain 1 to 128 characters")
		}
		publicKey, err := decodeDMKey(req.FallbackKey.PublicKey, 32)
		if err != nil {
			return dmValidation(c, "fallback_key.public_key", "must be a base64-encoded 32-byte public key")
		}
		signature, err := decodeDMKey(req.FallbackKey.Signature, 64)
		if err != nil {
			return dmValidation(c, "fallback_key.signature", "must be a base64-encoded 64-byte signature")
		}
		fallback = &normalizedKey{keyID: req.FallbackKey.KeyID, publicKey: publicKey, signature: signature}
	}

	accepted := 0
	err = h.Run.Run(c.Request().Context(), func(q dmTxStore) error {
		if _, err := q.GetDMDeviceForUpdate(c.Request().Context(), db.GetDMDeviceForUpdateParams{
			UserID: user.ID, DeviceID: deviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMDeviceStale, "device is not active")
			}
			return dmStoreError("lock device for one-time key upload", err)
		}
		for _, key := range keys {
			rows, err := q.InsertDMOneTimeKey(c.Request().Context(), db.InsertDMOneTimeKeyParams{
				DeviceID: deviceID, KeyID: key.keyID, PublicKey: key.publicKey, Signature: key.signature,
			})
			if err != nil {
				return dmStoreError("insert one-time key", err)
			}
			accepted += int(rows)
		}
		if fallback != nil {
			if _, err := q.UpsertDMFallbackKey(c.Request().Context(), db.UpsertDMFallbackKeyParams{
				DeviceID: deviceID, KeyID: fallback.keyID, PublicKey: fallback.publicKey, Signature: fallback.signature,
			}); err != nil {
				return dmStoreError("store fallback key", err)
			}
		}
		return nil
	})
	if err != nil {
		return shared.RespondError(c, err)
	}
	return c.JSON(http.StatusOK, dmRequestEnvelope[dmUploadKeysResponse]{
		Data: dmUploadKeysResponse{Accepted: accepted},
	})
}

type dmKeyBundle struct {
	UserID             uuid.UUID        `json:"user_id"`
	DeviceID           uuid.UUID        `json:"device_id"`
	IdentityCurve25519 string           `json:"identity_curve25519"`
	IdentityEd25519    string           `json:"identity_ed25519"`
	OneTimeKey         *dmOneTimeKeyOut `json:"one_time_key"`
	FallbackKey        *dmOneTimeKeyOut `json:"fallback_key"`
}

type dmOneTimeKeyOut struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

func (h *DMHandler) GetKeyBundle(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	targetID, err := parseDMUUID(c.Param("id"), "id")
	if err != nil {
		return dmValidation(c, "id", "must be a valid UUID")
	}
	callerDeviceID, err := parseDMUUID(c.QueryParam("device_id"), "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	includeOneTimeKey := true
	if raw := c.QueryParam("include_one_time_key"); raw != "" {
		includeOneTimeKey, err = strconv.ParseBool(raw)
		if err != nil {
			return dmValidation(c, "include_one_time_key", "must be true or false")
		}
	}

	var bundle dmKeyBundle
	err = h.Run.Run(c.Request().Context(), func(q dmTxStore) error {
		if _, err := q.GetDMDeviceByUserAndID(c.Request().Context(), db.GetDMDeviceByUserAndIDParams{
			UserID: user.ID, DeviceID: callerDeviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMDeviceStale, "device is not active")
			}
			return dmStoreError("read requesting DM device", err)
		}
		eligible, err := q.IsDMMutualFollow(c.Request().Context(), db.IsDMMutualFollowParams{
			ID: user.ID, ID_2: targetID,
		})
		if err != nil {
			return dmStoreError("check DM eligibility", err)
		}
		if !eligible {
			return shared.Wrap(shared.ErrNotFound, "DM target not found")
		}
		target, err := q.GetDMTargetDevice(c.Request().Context(), targetID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMKeyUnavailable, "target has no active DM device")
			}
			return dmStoreError("read target DM device", err)
		}
		if _, err := q.GetDMDeviceForUpdate(c.Request().Context(), db.GetDMDeviceForUpdateParams{
			UserID: targetID, DeviceID: target.DeviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMKeyUnavailable, "target device is no longer active")
			}
			return dmStoreError("lock target DM device", err)
		}
		curveKey, err := decodeDMKey(target.IdentityCurve25519, 32)
		if err != nil {
			return dmStoreError("normalize target curve key", err)
		}
		edKey, err := decodeDMKey(target.IdentityEd25519, 32)
		if err != nil {
			return dmStoreError("normalize target ed25519 key", err)
		}
		bundle = dmKeyBundle{
			UserID:             target.UserID,
			DeviceID:           target.DeviceID,
			IdentityCurve25519: curveKey,
			IdentityEd25519:    edKey,
		}
		if includeOneTimeKey {
			key, err := q.ClaimDMOneTimeKey(c.Request().Context(), target.DeviceID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return dmStoreError("claim target one-time key", err)
			}
			if err == nil {
				publicKey, err := decodeDMKey(key.PublicKey, 32)
				if err != nil {
					return dmStoreError("normalize target one-time key", err)
				}
				signature, err := decodeDMKey(key.Signature, 64)
				if err != nil {
					return dmStoreError("normalize target one-time key signature", err)
				}
				bundle.OneTimeKey = &dmOneTimeKeyOut{
					KeyID: key.KeyID, PublicKey: publicKey, Signature: signature,
				}
			} else {
				fallback, err := q.GetDMFallbackKey(c.Request().Context(), target.DeviceID)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return shared.Wrap(shared.ErrDMKeyUnavailable, "target has no unclaimed one-time or fallback key")
					}
					return dmStoreError("read target fallback key", err)
				}
				publicKey, err := decodeDMKey(fallback.PublicKey, 32)
				if err != nil {
					return dmStoreError("normalize target fallback key", err)
				}
				signature, err := decodeDMKey(fallback.Signature, 64)
				if err != nil {
					return dmStoreError("normalize target fallback key signature", err)
				}
				bundle.FallbackKey = &dmOneTimeKeyOut{
					KeyID: fallback.KeyID, PublicKey: publicKey, Signature: signature,
				}
			}
		}
		return nil
	})
	if err != nil {
		return shared.RespondError(c, err)
	}
	return c.JSON(http.StatusOK, dmRequestEnvelope[dmKeyBundle]{Data: bundle})
}

type dmSendRequest struct {
	DeviceID          string `json:"device_id"`
	RecipientUserID   string `json:"recipient_user_id"`
	RecipientDeviceID string `json:"recipient_device_id"`
	ClientMessageID   string `json:"client_message_id"`
	Protocol          string `json:"protocol"`
	ProtocolVersion   int16  `json:"protocol_version"`
	MessageType       int16  `json:"message_type"`
	Ciphertext        string `json:"ciphertext"`
}

type dmSendResponse struct {
	MessageID uuid.UUID `json:"message_id"`
	Sequence  int64     `json:"sequence"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (h *DMHandler) SendMessage(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	var req dmSendRequest
	if err := bindDMBody(c, &req); err != nil {
		return err
	}
	senderDeviceID, err := parseDMUUID(req.DeviceID, "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	recipientID, err := parseDMUUID(req.RecipientUserID, "recipient_user_id")
	if err != nil {
		return dmValidation(c, "recipient_user_id", "must be a valid UUID")
	}
	recipientDeviceID, err := parseDMUUID(req.RecipientDeviceID, "recipient_device_id")
	if err != nil {
		return dmValidation(c, "recipient_device_id", "must be a valid UUID")
	}
	clientMessageID, err := parseDMUUID(req.ClientMessageID, "client_message_id")
	if err != nil {
		return dmValidation(c, "client_message_id", "must be a valid UUID")
	}
	if req.Protocol != "olm" || req.ProtocolVersion != 1 {
		return dmValidation(c, "protocol", "only olm protocol version 1 is supported")
	}
	if req.MessageType != 0 && req.MessageType != 1 {
		return dmValidation(c, "message_type", "must be 0 or 1")
	}
	ciphertext, _, err := decodeDMBase64(req.Ciphertext, dmMaxCiphertext)
	if err != nil {
		return dmValidation(c, "ciphertext", "must be valid base64 ciphertext up to 16 KiB")
	}
	canonical := struct {
		DeviceID          uuid.UUID `json:"device_id"`
		RecipientUserID   uuid.UUID `json:"recipient_user_id"`
		RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
		ClientMessageID   uuid.UUID `json:"client_message_id"`
		Protocol          string    `json:"protocol"`
		ProtocolVersion   int16     `json:"protocol_version"`
		MessageType       int16     `json:"message_type"`
		Ciphertext        []byte    `json:"ciphertext"`
	}{
		DeviceID: senderDeviceID, RecipientUserID: recipientID, RecipientDeviceID: recipientDeviceID,
		ClientMessageID: clientMessageID, Protocol: req.Protocol, ProtocolVersion: req.ProtocolVersion,
		MessageType: req.MessageType, Ciphertext: ciphertext,
	}
	canonicalBody, err := json.Marshal(canonical)
	if err != nil {
		return dmInternal(c, "canonicalize DM request", err)
	}
	digest := sha256.Sum256(canonicalBody)
	messageID := uuid.New()
	expiresAt := time.Now().UTC().Add(h.TTL)
	var response dmSendResponse
	status := http.StatusCreated

	err = h.Run.Run(c.Request().Context(), func(q dmTxStore) error {
		if _, err := q.GetDMDeviceByUserAndID(c.Request().Context(), db.GetDMDeviceByUserAndIDParams{
			UserID: user.ID, DeviceID: senderDeviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMDeviceStale, "device is not active")
			}
			return dmStoreError("read sending DM device", err)
		}

		created, err := q.CreateDMIdempotency(c.Request().Context(), db.CreateDMIdempotencyParams{
			SenderDeviceID: senderDeviceID, ClientMessageID: clientMessageID,
			RequestDigest: digest[:], MessageID: messageID, ExpiresAt: expiresAt,
		})
		if errors.Is(err, sql.ErrNoRows) {
			existing, err := q.GetDMIdempotency(c.Request().Context(), db.GetDMIdempotencyParams{
				SenderDeviceID: senderDeviceID, ClientMessageID: clientMessageID,
			})
			if err != nil {
				return dmStoreError("read DM idempotency record", err)
			}
			if !equalBytes(existing.RequestDigest, digest[:]) {
				return shared.Wrap(shared.ErrDMIdempotencyConflict, "client_message_id was used with different content")
			}
			if !existing.ResponseSequence.Valid {
				return dmStoreError("read incomplete DM idempotency record", errors.New("committed record has no response sequence"))
			}
			response = dmSendResponse{
				MessageID: existing.MessageID,
				Sequence:  existing.ResponseSequence.Int64,
				ExpiresAt: existing.ExpiresAt,
			}
			status = http.StatusOK
			return nil
		}
		if err != nil {
			return dmStoreError("reserve DM idempotency record", err)
		}

		eligible, err := q.IsDMMutualFollow(c.Request().Context(), db.IsDMMutualFollowParams{
			ID: user.ID, ID_2: recipientID,
		})
		if err != nil {
			return dmStoreError("check DM eligibility", err)
		}
		if !eligible {
			return shared.Wrap(shared.ErrNotFound, "DM recipient not found")
		}
		target, err := q.GetDMTargetDevice(c.Request().Context(), recipientID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMKeyUnavailable, "recipient has no active DM device")
			}
			return dmStoreError("read recipient DM device", err)
		}
		if target.DeviceID != recipientDeviceID {
			return shared.Wrap(shared.ErrDMDeviceStale, "recipient device changed; fetch and verify a new key bundle")
		}
		if _, err := q.GetDMDeviceForUpdate(c.Request().Context(), db.GetDMDeviceForUpdateParams{
			UserID: recipientID, DeviceID: recipientDeviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.Wrap(shared.ErrDMDeviceStale, "recipient device changed")
			}
			return dmStoreError("lock recipient DM device", err)
		}
		pending, err := q.CountPendingDMInbox(c.Request().Context(), recipientDeviceID)
		if err != nil {
			return dmStoreError("count pending DM messages", err)
		}
		if pending >= dmMaxInboxPending {
			return shared.Wrap(shared.ErrDMInboxFull, "recipient inbox is full")
		}
		sequence, err := q.AllocateDMSequence(c.Request().Context(), recipientDeviceID)
		if err != nil {
			return dmStoreError("allocate recipient sequence", err)
		}
		if _, err := q.CreateDMInboxMessage(c.Request().Context(), db.CreateDMInboxMessageParams{
			MessageID: messageID, SenderUserID: user.ID, SenderDeviceID: senderDeviceID,
			RecipientUserID: recipientID, RecipientDeviceID: recipientDeviceID,
			Sequence: sequence, ClientMessageID: clientMessageID, Protocol: req.Protocol,
			ProtocolVersion: req.ProtocolVersion, MessageType: req.MessageType,
			Ciphertext: ciphertext, ExpiresAt: expiresAt,
		}); err != nil {
			return dmStoreError("store DM ciphertext", err)
		}
		rows, err := q.SetDMIdempotencySequence(c.Request().Context(), db.SetDMIdempotencySequenceParams{
			SenderDeviceID: senderDeviceID, ClientMessageID: clientMessageID,
			ResponseSequence: sql.NullInt64{Int64: sequence, Valid: true},
		})
		if err != nil {
			return dmStoreError("complete DM idempotency record", err)
		}
		if rows != 1 {
			return dmStoreError("complete DM idempotency record", fmt.Errorf("updated %d rows", rows))
		}
		response = dmSendResponse{MessageID: created.MessageID, Sequence: sequence, ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		return shared.RespondError(c, err)
	}
	return c.JSON(status, dmRequestEnvelope[dmSendResponse]{Data: response})
}

type dmInboxMessage struct {
	MessageID         uuid.UUID `json:"message_id"`
	SenderUserID      uuid.UUID `json:"sender_user_id"`
	SenderDeviceID    uuid.UUID `json:"sender_device_id"`
	RecipientUserID   uuid.UUID `json:"recipient_user_id"`
	RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
	Sequence          int64     `json:"sequence"`
	Protocol          string    `json:"protocol"`
	ProtocolVersion   int16     `json:"protocol_version"`
	MessageType       int16     `json:"message_type"`
	Ciphertext        string    `json:"ciphertext"`
	CreatedAt         time.Time `json:"created_at"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type dmPollResponse struct {
	Messages  []dmInboxMessage `json:"messages"`
	NextAfter int64            `json:"next_after"`
	HasMore   bool             `json:"has_more"`
}

func (h *DMHandler) PollInbox(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	deviceID, err := parseDMUUID(c.QueryParam("device_id"), "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	rawAfter := c.QueryParam("after")
	if rawAfter == "" {
		return dmValidation(c, "after", "is required")
	}
	after, err := strconv.ParseInt(rawAfter, 10, 64)
	if err != nil || after < 0 {
		return dmValidation(c, "after", "must be a non-negative integer")
	}
	limit := dmMaxPollLimit
	if rawLimit := c.QueryParam("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 1 || parsed > dmMaxPollLimit {
			return dmValidation(c, "limit", "must be between 1 and 100")
		}
		limit = parsed
	}
	if _, err := h.Queries.GetDMDeviceByUserAndID(c.Request().Context(), db.GetDMDeviceByUserAndIDParams{
		UserID: user.ID, DeviceID: deviceID,
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is not active"))
		}
		return dmInternal(c, "read polling DM device", err)
	}
	rows, err := h.Queries.ListPendingDMInbox(c.Request().Context(), db.ListPendingDMInboxParams{
		RecipientDeviceID: deviceID, Sequence: after, Limit: int32(limit + 1),
	})
	if err != nil {
		return dmInternal(c, "list pending DM messages", err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	messages := make([]dmInboxMessage, 0, len(rows))
	nextAfter := after
	for _, row := range rows {
		messages = append(messages, dmInboxMessage{
			MessageID: row.MessageID, SenderUserID: row.SenderUserID,
			SenderDeviceID: row.SenderDeviceID, RecipientUserID: row.RecipientUserID,
			RecipientDeviceID: row.RecipientDeviceID, Sequence: row.Sequence,
			Protocol: row.Protocol, ProtocolVersion: row.ProtocolVersion,
			MessageType: row.MessageType, Ciphertext: base64.RawStdEncoding.EncodeToString(row.Ciphertext),
			CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
		})
		nextAfter = row.Sequence
	}
	return c.JSON(http.StatusOK, dmRequestEnvelope[dmPollResponse]{
		Data: dmPollResponse{Messages: messages, NextAfter: nextAfter, HasMore: hasMore},
	})
}

type dmAckRequest struct {
	DeviceID string `json:"device_id"`
}

func (h *DMHandler) AcknowledgeMessage(c echo.Context) error {
	if err := h.ready(c); err != nil {
		return err
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	messageID, err := parseDMUUID(c.Param("message_id"), "message_id")
	if err != nil {
		return dmValidation(c, "message_id", "must be a valid UUID")
	}
	var req dmAckRequest
	if err := bindDMBody(c, &req); err != nil {
		return err
	}
	deviceID, err := parseDMUUID(req.DeviceID, "device_id")
	if err != nil {
		return dmValidation(c, "device_id", "must be a valid UUID")
	}
	if _, err := h.Queries.GetDMDeviceByUserAndID(c.Request().Context(), db.GetDMDeviceByUserAndIDParams{
		UserID: user.ID, DeviceID: deviceID,
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is not active"))
		}
		return dmInternal(c, "read acknowledging DM device", err)
	}
	rows, err := h.Queries.AcknowledgeDMInboxMessage(c.Request().Context(), db.AcknowledgeDMInboxMessageParams{
		MessageID: messageID, RecipientDeviceID: deviceID,
	})
	if err != nil {
		return dmInternal(c, "acknowledge DM message", err)
	}
	if rows == 0 {
		if _, err := h.Queries.GetDMInboxMessageForDevice(c.Request().Context(), db.GetDMInboxMessageForDeviceParams{
			MessageID: messageID, RecipientDeviceID: deviceID,
		}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "DM message not found"))
			}
			return dmInternal(c, "verify acknowledged DM message", err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *DMHandler) ready(c echo.Context) error {
	if h == nil || h.Queries == nil || h.Run == nil || h.TTL != 168*time.Hour {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeInternalError, "internal server error"))
	}
	return nil
}

func bindDMBody(c echo.Context, dst any) error {
	req := c.Request()
	if req.Body == nil {
		return dmValidation(c, "body", "JSON body is required")
	}
	req.Body = http.MaxBytesReader(c.Response(), req.Body, dmRequestBodyLimit)
	if err := c.Bind(dst); err != nil {
		return dmValidation(c, "body", "must be valid JSON within the request size limit")
	}
	return nil
}

func parseDMUUID(value, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%s is not a valid UUID", field)
	}
	return id, nil
}

func decodeDMKey(value string, expectedBytes int) (string, error) {
	decoded, normalized, err := decodeDMBase64(value, expectedBytes)
	if err != nil || len(decoded) != expectedBytes {
		return "", errors.New("invalid key encoding")
	}
	return normalized, nil
}

func decodeDMBase64(value string, maxBytes int) ([]byte, string, error) {
	if len(value) == 0 || len(value) > maxBytes*2 {
		return nil, "", errors.New("encoded value is empty or too large")
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding.Strict(),
		base64.RawStdEncoding.Strict(),
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil && len(decoded) > 0 && len(decoded) <= maxBytes {
			return decoded, base64.RawStdEncoding.EncodeToString(decoded), nil
		}
	}
	return nil, "", errors.New("invalid base64")
}

func dmValidation(c echo.Context, field, message string) error {
	return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "Invalid DM request").
		WithDetails(shared.FieldError{Field: field, Message: message}))
}

func dmStoreError(operation string, err error) error {
	slog.Error("DM database operation failed", "operation", operation, "err", err)
	return shared.NewAPIError(shared.CodeInternalError, "internal server error").WithCause(err)
}

func dmInternal(c echo.Context, operation string, err error) error {
	slog.Error("DM request failed", "operation", operation, "err", err)
	return shared.RespondError(c, shared.NewAPIError(shared.CodeInternalError, "internal server error").WithCause(err))
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
