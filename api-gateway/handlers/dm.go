package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const (
	dmPublicKeySize          = 32
	dmMaxOneTimeKeysPerBatch = 100
	dmMaxOneTimeKeysStored   = 1000
	dmMaxCiphertextBytes     = 64 * 1024
	dmMaxRegistrationBody    = 128 * 1024
	dmDefaultPollLimit       = 50
	dmMaxPollLimit           = 100
)

type dmTxStore interface {
	RevokeDMDeviceForUser(ctx context.Context, userID uuid.UUID) error
	CreateDMDevice(ctx context.Context, arg db.CreateDMDeviceParams) (db.DmDevice, error)
	InsertDMOneTimeKey(ctx context.Context, arg db.InsertDMOneTimeKeyParams) (int64, error)
	CountDMOneTimeKeys(ctx context.Context, deviceID uuid.UUID) (int64, error)
	GetDMDeviceByUser(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetDMDeviceForUpdate(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetEligibleDMDeviceForBundle(ctx context.Context, arg db.GetEligibleDMDeviceForBundleParams) (db.DmDevice, error)
	ClaimDMOneTimeKey(ctx context.Context, deviceID uuid.UUID) (db.ClaimDMOneTimeKeyRow, error)
	GetEligibleDMRecipientDeviceForSend(ctx context.Context, arg db.GetEligibleDMRecipientDeviceForSendParams) (uuid.UUID, error)
	IncrementDMDeviceSequence(ctx context.Context, id uuid.UUID) (int64, error)
	GetDMInboxMessageByClientID(ctx context.Context, arg db.GetDMInboxMessageByClientIDParams) (db.GetDMInboxMessageByClientIDRow, error)
	InsertDMInboxMessage(ctx context.Context, arg db.InsertDMInboxMessageParams) (db.InsertDMInboxMessageRow, error)
}

type dmStore interface {
	dmTxStore
	ListDMInboxAfter(ctx context.Context, arg db.ListDMInboxAfterParams) ([]db.ListDMInboxAfterRow, error)
	AckDMInboxMessage(ctx context.Context, arg db.AckDMInboxMessageParams) (int64, error)
	DeleteExpiredDMInbox(ctx context.Context) (int64, error)
}

type DMHandler struct {
	Store dmStore
	Run   txRunner[dmTxStore]
	TTL   time.Duration
}

func NewDMHandler(queries *db.Queries, dbHandle *sql.DB, ttl time.Duration) (*DMHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewDMHandler: queries is nil")
	}
	if dbHandle == nil {
		return nil, fmt.Errorf("NewDMHandler: db is nil")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("NewDMHandler: inbox TTL must be positive")
	}
	return &DMHandler{
		Store: queries,
		Run: NewSQLTxRunner(dbHandle, func(tx *sql.Tx) dmTxStore {
			return queries.WithTx(tx)
		}),
		TTL: ttl,
	}, nil
}

func (h *DMHandler) RegisterRoutes(api *echo.Group) {
	api.POST("/dm/device", h.RegisterDevice)
	api.POST("/dm/device/keys", h.UploadOneTimeKeys)
	api.GET("/dm/users/:id/key-bundle", h.GetKeyBundle)
	api.POST("/dm/messages", h.SendMessage)
	api.GET("/dm/inbox", h.PollInbox)
	api.POST("/dm/inbox/:id/ack", h.AcknowledgeMessage)
}

type dmOneTimeKeyInput struct {
	KeyID     string `json:"key_id"`
	PublicKey []byte `json:"public_key"`
}

type dmRegisterDeviceRequest struct {
	SigningKey    []byte              `json:"signing_key"`
	IdentityKey   []byte              `json:"identity_key"`
	FallbackKeyID string              `json:"fallback_key_id"`
	FallbackKey   []byte              `json:"fallback_key"`
	OneTimeKeys   []dmOneTimeKeyInput `json:"one_time_keys"`
}

type dmUploadKeysRequest struct {
	OneTimeKeys []dmOneTimeKeyInput `json:"one_time_keys"`
}

type dmDeviceResponse struct {
	DeviceID     uuid.UUID `json:"device_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

type dmPublicKey struct {
	KeyID     string `json:"key_id"`
	PublicKey []byte `json:"public_key"`
}

type dmKeyBundleResponse struct {
	UserID      uuid.UUID    `json:"user_id"`
	DeviceID    uuid.UUID    `json:"device_id"`
	SigningKey  []byte       `json:"signing_key"`
	IdentityKey []byte       `json:"identity_key"`
	OneTimeKey  *dmPublicKey `json:"one_time_key,omitempty"`
	FallbackKey *dmPublicKey `json:"fallback_key,omitempty"`
}

type dmSendRequest struct {
	RecipientUserID uuid.UUID `json:"recipient_user_id"`
	Version         int16     `json:"version"`
	ClientMessageID uuid.UUID `json:"client_message_id"`
	Ciphertext      []byte    `json:"ciphertext"`
}

type dmSendResponse struct {
	MessageID       uuid.UUID `json:"message_id"`
	Sequence        int64     `json:"sequence"`
	ClientMessageID uuid.UUID `json:"client_message_id"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type dmInboxMessage struct {
	MessageID       uuid.UUID `json:"message_id"`
	Sequence        int64     `json:"sequence"`
	SenderUserID    uuid.UUID `json:"sender_user_id"`
	SenderDeviceID  uuid.UUID `json:"sender_device_id"`
	Version         int16     `json:"version"`
	Ciphertext      []byte    `json:"ciphertext"`
	ClientMessageID uuid.UUID `json:"client_message_id"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type dmInboxResponse struct {
	Messages  []dmInboxMessage `json:"messages"`
	NextAfter int64            `json:"next_after"`
}

func (h *DMHandler) RegisterDevice(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	var req dmRegisterDeviceRequest
	if err := bindDMJSON(c, &req, dmMaxRegistrationBody); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid device registration"))
	}
	if err := validateDMRegistration(req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}

	deviceID, err := uuid.NewRandom()
	if err != nil {
		return dmInternal(c, "generate device id", err)
	}
	var device db.DmDevice
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		if err := qtx.RevokeDMDeviceForUser(c.Request().Context(), user.ID); err != nil {
			return fmt.Errorf("revoke previous device: %w", err)
		}
		var fallbackID sql.NullString
		if req.FallbackKeyID != "" {
			fallbackID = sql.NullString{String: req.FallbackKeyID, Valid: true}
		}
		created, err := qtx.CreateDMDevice(c.Request().Context(), db.CreateDMDeviceParams{
			ID:            deviceID,
			UserID:        user.ID,
			SigningKey:    req.SigningKey,
			IdentityKey:   req.IdentityKey,
			FallbackKeyID: fallbackID,
			FallbackKey:   req.FallbackKey,
		})
		if err != nil {
			return fmt.Errorf("create device: %w", err)
		}
		device = created
		if err := insertDMKeys(c.Request().Context(), qtx, deviceID, req.OneTimeKeys); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, shared.ErrValidation) {
			return shared.RespondError(c, err)
		}
		return dmInternal(c, "register device", err)
	}
	return shared.RespondCreated(c, dmDeviceResponse{DeviceID: device.ID, RegisteredAt: device.RegisteredAt})
}

func (h *DMHandler) UploadOneTimeKeys(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	var req dmUploadKeysRequest
	if err := bindDMJSON(c, &req, dmMaxRegistrationBody); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid one-time key request"))
	}
	if err := validateDMKeys(req.OneTimeKeys); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	var deviceID uuid.UUID
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		device, err := qtx.GetDMDeviceForUpdate(c.Request().Context(), user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "device not registered")
		}
		if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		count, err := qtx.CountDMOneTimeKeys(c.Request().Context(), device.ID)
		if err != nil {
			return fmt.Errorf("count one-time keys: %w", err)
		}
		if count+int64(len(req.OneTimeKeys)) > dmMaxOneTimeKeysStored {
			return shared.Wrap(shared.ErrValidation, "stored one-time key limit exceeded")
		}
		if err := insertDMKeys(c.Request().Context(), qtx, device.ID, req.OneTimeKeys); err != nil {
			return err
		}
		deviceID = device.ID
		return nil
	})
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrValidation) {
			return shared.RespondError(c, err)
		}
		return dmInternal(c, "upload one-time keys", err)
	}
	return shared.RespondOK(c, map[string]any{"device_id": deviceID, "uploaded": len(req.OneTimeKeys)})
}

func (h *DMHandler) GetKeyBundle(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil || targetID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid recipient id"))
	}
	var bundle dmKeyBundleResponse
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		device, err := qtx.GetEligibleDMDeviceForBundle(c.Request().Context(), db.GetEligibleDMDeviceForBundleParams{
			FolloweeID: user.ID,
			UserID:     targetID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "recipient not found")
		}
		if err != nil {
			return fmt.Errorf("load eligible recipient device: %w", err)
		}
		bundle = dmKeyBundleResponse{
			UserID:      device.UserID,
			DeviceID:    device.ID,
			SigningKey:  device.SigningKey,
			IdentityKey: device.IdentityKey,
		}
		key, err := qtx.ClaimDMOneTimeKey(c.Request().Context(), device.ID)
		if err == nil {
			bundle.OneTimeKey = &dmPublicKey{KeyID: key.KeyID, PublicKey: key.PublicKey}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("claim one-time key: %w", err)
		}
		if device.FallbackKeyID.Valid && len(device.FallbackKey) == dmPublicKeySize {
			bundle.FallbackKey = &dmPublicKey{KeyID: device.FallbackKeyID.String, PublicKey: device.FallbackKey}
			return nil
		}
		return shared.Wrap(shared.ErrDMKeysExhausted, "recipient has no available prekey")
	})
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrDMKeysExhausted) {
			return shared.RespondError(c, err)
		}
		return dmInternal(c, "fetch key bundle", err)
	}
	return shared.RespondOK(c, bundle)
}

func (h *DMHandler) SendMessage(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	var req dmSendRequest
	if err := bindDMJSON(c, &req, dmMaxCiphertextBytes+4096); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message envelope"))
	}
	if req.RecipientUserID == uuid.Nil || req.ClientMessageID == uuid.Nil ||
		req.RecipientUserID == user.ID || req.Version != 1 ||
		len(req.Ciphertext) == 0 || len(req.Ciphertext) > dmMaxCiphertextBytes {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message envelope"))
	}

	var response dmSendResponse
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		senderDevice, err := qtx.GetDMDeviceByUser(c.Request().Context(), user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "sender device not registered")
		}
		if err != nil {
			return fmt.Errorf("load sender device: %w", err)
		}
		existing, err := qtx.GetDMInboxMessageByClientID(c.Request().Context(), db.GetDMInboxMessageByClientIDParams{
			SenderDeviceID:  senderDevice.ID,
			ClientMessageID: req.ClientMessageID,
		})
		if err == nil {
			if existing.RecipientUserID != req.RecipientUserID ||
				existing.Version != req.Version ||
				!bytes.Equal(existing.Ciphertext, req.Ciphertext) {
				return shared.Wrap(shared.ErrDMConflict, "client message id was reused")
			}
			response = dmSendResponse{
				MessageID:       existing.ID,
				Sequence:        existing.SequenceNo,
				ClientMessageID: existing.ClientMessageID,
				CreatedAt:       existing.CreatedAt,
				ExpiresAt:       existing.ExpiresAt,
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check send idempotency: %w", err)
		}

		recipientDeviceID, err := qtx.GetEligibleDMRecipientDeviceForSend(c.Request().Context(), db.GetEligibleDMRecipientDeviceForSendParams{
			ID:     senderDevice.ID,
			UserID: req.RecipientUserID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "recipient not found")
		}
		if err != nil {
			return fmt.Errorf("check recipient eligibility: %w", err)
		}
		sequence, err := qtx.IncrementDMDeviceSequence(c.Request().Context(), recipientDeviceID)
		if err != nil {
			return fmt.Errorf("allocate inbox sequence: %w", err)
		}
		inserted, err := qtx.InsertDMInboxMessage(c.Request().Context(), db.InsertDMInboxMessageParams{
			RecipientDeviceID: recipientDeviceID,
			RecipientUserID:   req.RecipientUserID,
			SequenceNo:        sequence,
			SenderUserID:      user.ID,
			SenderDeviceID:    senderDevice.ID,
			Version:           req.Version,
			Ciphertext:        req.Ciphertext,
			ClientMessageID:   req.ClientMessageID,
			ExpiresInSeconds:  h.TTL.Seconds(),
		})
		if errors.Is(err, sql.ErrNoRows) {
			existing, err = qtx.GetDMInboxMessageByClientID(c.Request().Context(), db.GetDMInboxMessageByClientIDParams{
				SenderDeviceID:  senderDevice.ID,
				ClientMessageID: req.ClientMessageID,
			})
			if err != nil {
				return fmt.Errorf("load concurrent idempotent send: %w", err)
			}
			if existing.RecipientUserID != req.RecipientUserID ||
				existing.Version != req.Version ||
				!bytes.Equal(existing.Ciphertext, req.Ciphertext) {
				return shared.Wrap(shared.ErrDMConflict, "client message id was reused")
			}
			response = dmSendResponse{
				MessageID:       existing.ID,
				Sequence:        existing.SequenceNo,
				ClientMessageID: existing.ClientMessageID,
				CreatedAt:       existing.CreatedAt,
				ExpiresAt:       existing.ExpiresAt,
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert inbox message: %w", err)
		}
		response = dmSendResponse{
			MessageID:       inserted.ID,
			Sequence:        inserted.SequenceNo,
			ClientMessageID: inserted.ClientMessageID,
			CreatedAt:       inserted.CreatedAt,
			ExpiresAt:       inserted.ExpiresAt,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrDMConflict) {
			return shared.RespondError(c, err)
		}
		return dmInternal(c, "send message", err)
	}
	return shared.RespondCreated(c, response)
}

func (h *DMHandler) PollInbox(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	after, err := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	if err != nil || after < 0 {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "after must be a non-negative sequence"))
	}
	limit := dmDefaultPollLimit
	if raw := c.QueryParam("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > dmMaxPollLimit {
			return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "limit must be between 1 and 100"))
		}
	}
	device, err := h.Store.GetDMDeviceByUser(c.Request().Context(), user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "device not registered"))
	}
	if err != nil {
		return dmInternal(c, "load polling device", err)
	}
	if _, err := h.Store.DeleteExpiredDMInbox(c.Request().Context()); err != nil {
		return dmInternal(c, "expire inbox messages", err)
	}
	rows, err := h.Store.ListDMInboxAfter(c.Request().Context(), db.ListDMInboxAfterParams{
		RecipientDeviceID: device.ID,
		SequenceNo:        after,
		Limit:             int32(limit),
	})
	if err != nil {
		return dmInternal(c, "poll inbox", err)
	}
	response := dmInboxResponse{
		Messages:  make([]dmInboxMessage, 0, len(rows)),
		NextAfter: after,
	}
	for _, row := range rows {
		response.Messages = append(response.Messages, dmInboxMessage{
			MessageID:       row.ID,
			Sequence:        row.SequenceNo,
			SenderUserID:    row.SenderUserID,
			SenderDeviceID:  row.SenderDeviceID,
			Version:         row.Version,
			Ciphertext:      row.Ciphertext,
			ClientMessageID: row.ClientMessageID,
			CreatedAt:       row.CreatedAt,
			ExpiresAt:       row.ExpiresAt,
		})
		response.NextAfter = row.SequenceNo
	}
	return shared.RespondOK(c, response)
}

func (h *DMHandler) AcknowledgeMessage(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	messageID, err := uuid.Parse(c.Param("id"))
	if err != nil || messageID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message id"))
	}
	device, err := h.Store.GetDMDeviceByUser(c.Request().Context(), user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "device not registered"))
	}
	if err != nil {
		return dmInternal(c, "load acknowledgement device", err)
	}
	if _, err := h.Store.AckDMInboxMessage(c.Request().Context(), db.AckDMInboxMessageParams{
		ID:                messageID,
		RecipientDeviceID: device.ID,
	}); err != nil {
		return dmInternal(c, "acknowledge inbox message", err)
	}
	return c.NoContent(http.StatusNoContent)
}

func dmCurrentUser(c echo.Context) (*db.User, error) {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return nil, shared.Wrap(shared.ErrUnauthorized, "no authenticated user")
	}
	return user, nil
}

func bindDMJSON(c echo.Context, dst any, maxBytes int64) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxBytes)
	if err := c.Bind(dst); err != nil {
		return fmt.Errorf("bind DM JSON: %w", err)
	}
	return nil
}

func validateDMRegistration(req dmRegisterDeviceRequest) error {
	if len(req.SigningKey) != dmPublicKeySize || len(req.IdentityKey) != dmPublicKeySize {
		return fmt.Errorf("signing_key and identity_key must each contain 32 bytes")
	}
	if len(req.FallbackKey) != 0 && len(req.FallbackKey) != dmPublicKeySize {
		return fmt.Errorf("fallback_key must contain 32 bytes")
	}
	if (req.FallbackKeyID == "") != (len(req.FallbackKey) == 0) {
		return fmt.Errorf("fallback_key_id and fallback_key must be supplied together")
	}
	if req.FallbackKeyID != "" && !validDMKeyID(req.FallbackKeyID) {
		return fmt.Errorf("fallback_key_id must be 1 to 64 characters without surrounding whitespace")
	}
	return validateDMKeys(req.OneTimeKeys)
}

func validateDMKeys(keys []dmOneTimeKeyInput) error {
	if len(keys) > dmMaxOneTimeKeysPerBatch {
		return fmt.Errorf("at most %d one-time keys may be uploaded at once", dmMaxOneTimeKeysPerBatch)
	}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if !validDMKeyID(key.KeyID) {
			return fmt.Errorf("one-time key id must be 1 to 64 characters without surrounding whitespace")
		}
		if len(key.PublicKey) != dmPublicKeySize {
			return fmt.Errorf("one-time public keys must contain 32 bytes")
		}
		if _, ok := seen[key.KeyID]; ok {
			return fmt.Errorf("one-time key ids must be unique within a request")
		}
		seen[key.KeyID] = struct{}{}
	}
	return nil
}

func validDMKeyID(keyID string) bool {
	if keyID == "" || strings.TrimSpace(keyID) != keyID ||
		!utf8.ValidString(keyID) || utf8.RuneCountInString(keyID) > 64 {
		return false
	}
	for _, char := range keyID {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func insertDMKeys(ctx context.Context, qtx dmTxStore, deviceID uuid.UUID, keys []dmOneTimeKeyInput) error {
	for _, key := range keys {
		rows, err := qtx.InsertDMOneTimeKey(ctx, db.InsertDMOneTimeKeyParams{
			DeviceID:  deviceID,
			KeyID:     key.KeyID,
			PublicKey: key.PublicKey,
		})
		if err != nil {
			return fmt.Errorf("insert one-time key: %w", err)
		}
		if rows == 0 {
			return shared.Wrap(shared.ErrValidation, "one-time key id is already registered")
		}
	}
	return nil
}

func dmInternal(c echo.Context, operation string, err error) error {
	slog.Error("direct-message relay failed", "operation", operation, "err", err)
	return shared.RespondError(c, shared.Wrap(shared.ErrInternal, operation))
}
