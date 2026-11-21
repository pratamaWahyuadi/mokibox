package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	dmMaxProtocolLength      = 64
	dmMaxKeyBundleBytes      = 64 * 1024
	dmMaxKeyMaterialBytes    = 4 * 1024
	dmMaxOneTimeKeysPerBatch = 100
	dmMaxOneTimeKeysStored   = 1000
	dmMaxCiphertextBytes     = 64 * 1024
	dmMaxRegistrationBody    = 1 * 1024 * 1024
	dmMaxSendBody            = 96 * 1024
	dmMaxAckBody             = 4 * 1024
	dmMaxPendingPerDevice    = 1000
	dmDefaultPollLimit       = 50
	dmMaxPollLimit           = 100
)

type dmTxStore interface {
	DeleteDMDevicesByUser(ctx context.Context, userID uuid.UUID) error
	CreateDMDevice(ctx context.Context, arg db.CreateDMDeviceParams) (db.DmDevice, error)
	InsertDMOneTimeKey(ctx context.Context, arg db.InsertDMOneTimeKeyParams) (int64, error)
	CountDMOneTimeKeys(ctx context.Context, deviceID uuid.UUID) (int64, error)
	CountPendingDMInboxForRecipient(ctx context.Context, deviceID uuid.UUID) (int64, error)
	GetDMDeviceByUser(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetDMDeviceByUserForKeyShare(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetDMDeviceForUpdate(ctx context.Context, userID uuid.UUID) (db.DmDevice, error)
	GetEligibleDMDeviceForBundle(ctx context.Context, arg db.GetEligibleDMDeviceForBundleParams) (db.DmDevice, error)
	ClaimDMOneTimeKey(ctx context.Context, deviceID uuid.UUID) (db.ClaimDMOneTimeKeyRow, error)
	GetEligibleDMRecipientDeviceForSend(ctx context.Context, arg db.GetEligibleDMRecipientDeviceForSendParams) (db.GetEligibleDMRecipientDeviceForSendRow, error)
	IncrementDMDeviceSequence(ctx context.Context, id uuid.UUID) (int64, error)
	GetDMSendIdempotencyByClientID(ctx context.Context, arg db.GetDMSendIdempotencyByClientIDParams) (db.GetDMSendIdempotencyByClientIDRow, error)
	InsertDMSendIdempotency(ctx context.Context, arg db.InsertDMSendIdempotencyParams) (db.InsertDMSendIdempotencyRow, error)
	InsertDMInboxMessage(ctx context.Context, arg db.InsertDMInboxMessageParams) (db.InsertDMInboxMessageRow, error)
}

type dmStore interface {
	dmTxStore
	ListDMInboxAfter(ctx context.Context, arg db.ListDMInboxAfterParams) ([]db.ListDMInboxAfterRow, error)
	AckDMInboxMessage(ctx context.Context, arg db.AckDMInboxMessageParams) (int64, error)
}

type DMHandler struct {
	Store dmStore
	Run   txRunner[dmTxStore]
	TTL   time.Duration
}

type DMRateLimits struct {
	KeyBundle echo.MiddlewareFunc
	Send      echo.MiddlewareFunc
}

var errDMDuplicateSend = errors.New("duplicate idempotent send")

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

func (h *DMHandler) RegisterRoutes(api *echo.Group, limits DMRateLimits) {
	api.POST("/dm/device", h.RegisterDevice)
	api.POST("/dm/device/keys", h.UploadOneTimeKeys)
	api.GET("/dm/users/:id/key-bundle", h.GetKeyBundle, optionalDMRateLimit(limits.KeyBundle)...)
	api.POST("/dm/messages", h.SendMessage, optionalDMRateLimit(limits.Send)...)
	api.GET("/dm/inbox", h.PollInbox)
	api.POST("/dm/inbox/:id/ack", h.AcknowledgeMessage)
}

func optionalDMRateLimit(limit echo.MiddlewareFunc) []echo.MiddlewareFunc {
	if limit == nil {
		return nil
	}
	return []echo.MiddlewareFunc{limit}
}

type dmOneTimeKeyInput struct {
	KeyID       string `json:"key_id"`
	KeyMaterial []byte `json:"key_material"`
}

type dmRegisterDeviceRequest struct {
	Protocol            string              `json:"protocol"`
	Version             int32               `json:"version"`
	KeyBundle           []byte              `json:"key_bundle"`
	FallbackKeyID       string              `json:"fallback_key_id"`
	FallbackKeyMaterial []byte              `json:"fallback_key_material"`
	OneTimeKeys         []dmOneTimeKeyInput `json:"one_time_keys"`
}

type dmUploadKeysRequest struct {
	DeviceID    uuid.UUID           `json:"device_id"`
	OneTimeKeys []dmOneTimeKeyInput `json:"one_time_keys"`
}

type dmAckRequest struct {
	DeviceID uuid.UUID `json:"device_id"`
}

type dmDeviceResponse struct {
	Protocol     string    `json:"protocol"`
	Version      int32     `json:"version"`
	DeviceID     uuid.UUID `json:"device_id"`
	RegisteredAt time.Time `json:"registered_at"`
}

type dmKeyMaterial struct {
	KeyID       string `json:"key_id"`
	KeyMaterial []byte `json:"key_material"`
}

type dmKeyBundleResponse struct {
	UserID      uuid.UUID      `json:"user_id"`
	DeviceID    uuid.UUID      `json:"device_id"`
	Protocol    string         `json:"protocol"`
	Version     int32          `json:"version"`
	KeyBundle   []byte         `json:"key_bundle"`
	OneTimeKey  *dmKeyMaterial `json:"one_time_key,omitempty"`
	FallbackKey *dmKeyMaterial `json:"fallback_key,omitempty"`
}

type dmSendRequest struct {
	SenderDeviceID    uuid.UUID `json:"sender_device_id"`
	RecipientUserID   uuid.UUID `json:"recipient_user_id"`
	RecipientDeviceID uuid.UUID `json:"recipient_device_id"`
	Protocol          string    `json:"protocol"`
	Version           int32     `json:"version"`
	ClientMessageID   uuid.UUID `json:"client_message_id"`
	Ciphertext        []byte    `json:"ciphertext"`
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
	Protocol        string    `json:"protocol"`
	Version         int32     `json:"version"`
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
		if err := qtx.DeleteDMDevicesByUser(c.Request().Context(), user.ID); err != nil {
			return fmt.Errorf("revoke previous device: %w", err)
		}
		var fallbackID sql.NullString
		if req.FallbackKeyID != "" {
			fallbackID = sql.NullString{String: req.FallbackKeyID, Valid: true}
		}
		created, err := qtx.CreateDMDevice(c.Request().Context(), db.CreateDMDeviceParams{
			ID:                  deviceID,
			UserID:              user.ID,
			Protocol:            req.Protocol,
			ProtocolVersion:     req.Version,
			KeyBundle:           req.KeyBundle,
			FallbackKeyID:       fallbackID,
			FallbackKeyMaterial: req.FallbackKeyMaterial,
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
	return shared.RespondCreated(c, dmDeviceResponse{
		Protocol: device.Protocol, Version: device.ProtocolVersion,
		DeviceID: device.ID, RegisteredAt: device.RegisteredAt,
	})
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
	if req.DeviceID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "device_id is required"))
	}
	if err := validateDMKeys(req.OneTimeKeys); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	var deviceID uuid.UUID
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		device, err := qtx.GetDMDeviceForUpdate(c.Request().Context(), user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active")
		}
		if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		if device.ID != req.DeviceID {
			return shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active")
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
		if errors.Is(err, shared.ErrDMDeviceStale) || errors.Is(err, shared.ErrValidation) {
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
	senderDeviceID, err := uuid.Parse(c.QueryParam("device_id"))
	if err != nil || senderDeviceID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "device_id is required and must be a UUID"))
	}
	var bundle dmKeyBundleResponse
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		senderDevice, err := qtx.GetDMDeviceByUserForKeyShare(c.Request().Context(), user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active")
		}
		if err != nil {
			return fmt.Errorf("load requesting device: %w", err)
		}
		if senderDevice.ID != senderDeviceID {
			return shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active")
		}
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
			UserID:    device.UserID,
			DeviceID:  device.ID,
			Protocol:  device.Protocol,
			Version:   device.ProtocolVersion,
			KeyBundle: device.KeyBundle,
		}
		key, err := qtx.ClaimDMOneTimeKey(c.Request().Context(), device.ID)
		if err == nil {
			bundle.OneTimeKey = &dmKeyMaterial{KeyID: key.KeyID, KeyMaterial: key.KeyMaterial}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("claim one-time key: %w", err)
		}
		if device.FallbackKeyID.Valid && len(device.FallbackKeyMaterial) > 0 {
			bundle.FallbackKey = &dmKeyMaterial{
				KeyID: device.FallbackKeyID.String, KeyMaterial: device.FallbackKeyMaterial,
			}
			return nil
		}
		return shared.Wrap(shared.ErrDMKeysExhausted, "recipient has no one-time or fallback key")
	})
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrDMKeysExhausted) ||
			errors.Is(err, shared.ErrDMDeviceStale) {
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
	if err := bindDMJSON(c, &req, dmMaxSendBody); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message envelope"))
	}
	if req.SenderDeviceID == uuid.Nil || req.RecipientDeviceID == uuid.Nil ||
		req.RecipientUserID == uuid.Nil || req.ClientMessageID == uuid.Nil ||
		req.RecipientUserID == user.ID || !validDMProtocol(req.Protocol) || req.Version <= 0 ||
		len(req.Ciphertext) == 0 || len(req.Ciphertext) > dmMaxCiphertextBytes {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message envelope"))
	}

	ciphertextDigest := sha256.Sum256(req.Ciphertext)
	var response dmSendResponse
	err = h.Run.Run(c.Request().Context(), func(qtx dmTxStore) error {
		senderDevice, err := qtx.GetDMDeviceByUserForKeyShare(c.Request().Context(), user.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrDMDeviceStale, "sender device is no longer active")
		}
		if err != nil {
			return fmt.Errorf("load sender device: %w", err)
		}
		if senderDevice.ID != req.SenderDeviceID {
			return shared.Wrap(shared.ErrDMDeviceStale, "sender device is no longer active")
		}
		existing, err := qtx.GetDMSendIdempotencyByClientID(c.Request().Context(), db.GetDMSendIdempotencyByClientIDParams{
			SenderDeviceID:  senderDevice.ID,
			ClientMessageID: req.ClientMessageID,
		})
		if err == nil {
			if !dmSendMatchesIdempotency(existing, req, ciphertextDigest) {
				return shared.Wrap(shared.ErrDMConflict, "client message id was reused")
			}
			response = dmSendResponseFromIdempotency(existing, req.ClientMessageID)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check send idempotency: %w", err)
		}

		recipientDevice, err := qtx.GetEligibleDMRecipientDeviceForSend(c.Request().Context(), db.GetEligibleDMRecipientDeviceForSendParams{
			ID:     senderDevice.ID,
			UserID: req.RecipientUserID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "recipient not found")
		}
		if err != nil {
			return fmt.Errorf("check recipient eligibility: %w", err)
		}
		if recipientDevice.ID != req.RecipientDeviceID {
			return shared.Wrap(shared.ErrDMDeviceStale, "recipient device is no longer active")
		}
		if senderDevice.Protocol != req.Protocol || senderDevice.ProtocolVersion != req.Version ||
			recipientDevice.Protocol != req.Protocol || recipientDevice.ProtocolVersion != req.Version {
			return shared.Wrap(shared.ErrDMProtocolMismatch, "message protocol does not match registered devices")
		}
		sequence, err := qtx.IncrementDMDeviceSequence(c.Request().Context(), recipientDevice.ID)
		if err != nil {
			return fmt.Errorf("allocate inbox sequence: %w", err)
		}
		pending, err := qtx.CountPendingDMInboxForRecipient(c.Request().Context(), recipientDevice.ID)
		if err != nil {
			return fmt.Errorf("count pending recipient messages: %w", err)
		}
		if pending >= dmMaxPendingPerDevice {
			return shared.Wrap(shared.ErrDMInboxFull, "recipient inbox has reached its pending message limit")
		}

		messageID := uuid.New()
		idempotency, err := qtx.InsertDMSendIdempotency(c.Request().Context(), db.InsertDMSendIdempotencyParams{
			SenderDeviceID:    senderDevice.ID,
			SenderUserID:      user.ID,
			ClientMessageID:   req.ClientMessageID,
			RecipientUserID:   req.RecipientUserID,
			RecipientDeviceID: recipientDevice.ID,
			Protocol:          req.Protocol,
			Version:           req.Version,
			CiphertextDigest:  ciphertextDigest[:],
			MessageID:         messageID,
			SequenceNo:        sequence,
			ExpiresInSeconds:  h.TTL.Seconds(),
		})
		if errors.Is(err, sql.ErrNoRows) {
			return errDMDuplicateSend
		}
		if err != nil {
			return fmt.Errorf("reserve send idempotency key: %w", err)
		}
		inserted, err := qtx.InsertDMInboxMessage(c.Request().Context(), db.InsertDMInboxMessageParams{
			ID:                idempotency.MessageID,
			RecipientDeviceID: recipientDevice.ID,
			RecipientUserID:   req.RecipientUserID,
			SequenceNo:        sequence,
			SenderUserID:      user.ID,
			SenderDeviceID:    senderDevice.ID,
			Protocol:          req.Protocol,
			Version:           req.Version,
			Ciphertext:        req.Ciphertext,
			ClientMessageID:   req.ClientMessageID,
			CreatedAt:         idempotency.CreatedAt,
			ExpiresAt:         idempotency.ExpiresAt,
		})
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
	if errors.Is(err, errDMDuplicateSend) {
		activeDevice, lookupErr := h.Store.GetDMDeviceByUser(c.Request().Context(), user.ID)
		if errors.Is(lookupErr, sql.ErrNoRows) || (lookupErr == nil && activeDevice.ID != req.SenderDeviceID) {
			return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "sender device is no longer active"))
		}
		if lookupErr != nil {
			return dmInternal(c, "reload sender device after duplicate send", lookupErr)
		}
		existing, lookupErr := h.Store.GetDMSendIdempotencyByClientID(c.Request().Context(), db.GetDMSendIdempotencyByClientIDParams{
			SenderDeviceID:  req.SenderDeviceID,
			ClientMessageID: req.ClientMessageID,
		})
		if lookupErr != nil {
			return dmInternal(c, "load concurrent idempotent send", lookupErr)
		}
		if !dmSendMatchesIdempotency(existing, req, ciphertextDigest) {
			return shared.RespondError(c, shared.Wrap(shared.ErrDMConflict, "client message id was reused"))
		}
		response = dmSendResponseFromIdempotency(existing, req.ClientMessageID)
		return shared.RespondCreated(c, response)
	}
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) || errors.Is(err, shared.ErrDMConflict) ||
			errors.Is(err, shared.ErrDMProtocolMismatch) || errors.Is(err, shared.ErrDMDeviceStale) ||
			errors.Is(err, shared.ErrDMInboxFull) {
			return shared.RespondError(c, err)
		}
		return dmInternal(c, "send message", err)
	}
	return shared.RespondCreated(c, response)
}

func dmSendMatchesIdempotency(existing db.GetDMSendIdempotencyByClientIDRow, req dmSendRequest, digest [sha256.Size]byte) bool {
	return existing.RecipientUserID == req.RecipientUserID &&
		existing.RecipientDeviceID == req.RecipientDeviceID &&
		existing.Protocol == req.Protocol &&
		existing.Version == req.Version &&
		bytes.Equal(existing.CiphertextDigest, digest[:])
}

func dmSendResponseFromIdempotency(existing db.GetDMSendIdempotencyByClientIDRow, clientMessageID uuid.UUID) dmSendResponse {
	return dmSendResponse{
		MessageID:       existing.MessageID,
		Sequence:        existing.SequenceNo,
		ClientMessageID: clientMessageID,
		CreatedAt:       existing.CreatedAt,
		ExpiresAt:       existing.ExpiresAt,
	}
}

func (h *DMHandler) PollInbox(c echo.Context) error {
	user, err := dmCurrentUser(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	requestedDeviceID, err := uuid.Parse(c.QueryParam("device_id"))
	if err != nil || requestedDeviceID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "device_id is required and must be a UUID"))
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
		return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active"))
	}
	if err != nil {
		return dmInternal(c, "load polling device", err)
	}
	if device.ID != requestedDeviceID {
		return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active"))
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
			Protocol:        row.Protocol,
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
	var req dmAckRequest
	if err := bindDMJSON(c, &req, dmMaxAckBody); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid acknowledgement request"))
	}
	if req.DeviceID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "device_id is required"))
	}
	messageID, err := uuid.Parse(c.Param("id"))
	if err != nil || messageID == uuid.Nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid message id"))
	}
	device, err := h.Store.GetDMDeviceByUser(c.Request().Context(), user.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active"))
	}
	if err != nil {
		return dmInternal(c, "load acknowledgement device", err)
	}
	if device.ID != req.DeviceID {
		return shared.RespondError(c, shared.Wrap(shared.ErrDMDeviceStale, "device is no longer active"))
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
	if !validDMProtocol(req.Protocol) || req.Version <= 0 {
		return fmt.Errorf("protocol and version are required and must be valid")
	}
	if len(req.KeyBundle) == 0 || len(req.KeyBundle) > dmMaxKeyBundleBytes {
		return fmt.Errorf("key_bundle must contain 1 to %d bytes", dmMaxKeyBundleBytes)
	}
	if len(req.FallbackKeyMaterial) > dmMaxKeyMaterialBytes {
		return fmt.Errorf("fallback_key_material exceeds the maximum size")
	}
	if (req.FallbackKeyID == "") != (len(req.FallbackKeyMaterial) == 0) {
		return fmt.Errorf("fallback_key_id and fallback_key_material must be supplied together")
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
		if len(key.KeyMaterial) == 0 || len(key.KeyMaterial) > dmMaxKeyMaterialBytes {
			return fmt.Errorf("one-time key material must contain 1 to %d bytes", dmMaxKeyMaterialBytes)
		}
		if _, ok := seen[key.KeyID]; ok {
			return fmt.Errorf("one-time key ids must be unique within a request")
		}
		seen[key.KeyID] = struct{}{}
	}
	return nil
}

func validDMProtocol(protocol string) bool {
	if !validDMKeyID(protocol) || len(protocol) > dmMaxProtocolLength {
		return false
	}
	for _, char := range protocol {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-') {
			return false
		}
	}
	return true
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
			DeviceID:    deviceID,
			KeyID:       key.KeyID,
			KeyMaterial: key.KeyMaterial,
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
