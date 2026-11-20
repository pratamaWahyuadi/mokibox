package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/labstack/echo/v4"
	_ "golang.org/x/image/webp"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const maxAvatarBytes int64 = 5 * 1024 * 1024

type avatarStore interface {
	SetPendingAvatarUpload(ctx context.Context, arg db.SetPendingAvatarUploadParams) (db.SetPendingAvatarUploadRow, error)
	ConfirmAvatarUpload(ctx context.Context, arg db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error)
	ClearPendingAvatarUpload(ctx context.Context, arg db.ClearPendingAvatarUploadParams) (int64, error)
}

type AvatarHandler struct {
	Queries avatarStore
	R2      r2ObjectStore
	Queue   taskEnqueuer
	Cfg     *shared.APIConfig
}

func NewAvatarHandler(queries *db.Queries, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) (*AvatarHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewAvatarHandler: queries is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewAvatarHandler: r2 is nil")
	}
	if queue == nil {
		return nil, fmt.Errorf("NewAvatarHandler: queue is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewAvatarHandler: config is nil")
	}
	return NewAvatarHandlerForTest(queries, r2, asynqEnqueuer{client: queue}, cfg), nil
}

func NewAvatarHandlerForTest(queries avatarStore, r2 r2ObjectStore, queue taskEnqueuer, cfg *shared.APIConfig) *AvatarHandler {
	return &AvatarHandler{Queries: queries, R2: r2, Queue: queue, Cfg: cfg}
}

type avatarUploadIntentRequest struct {
	ContentType string `json:"content_type"`
}

type avatarUploadIntentResponse struct {
	AvatarKey     string            `json:"avatar_key"`
	HTTPMethod    string            `json:"http_method"`
	UploadURL     string            `json:"upload_url"`
	UploadHeaders map[string]string `json:"upload_headers"`
	MaxSizeBytes  int64             `json:"max_size_bytes"`
	ExpiresAt     string            `json:"expires_at"`
}

type avatarConfirmRequest struct {
	AvatarKey string `json:"avatar_key"`
}

type avatarConfirmResponse struct {
	AvatarURL string `json:"avatar_url"`
}

func (h *AvatarHandler) UploadIntent(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Queue == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "avatar handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req avatarUploadIntentRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	contentType := strings.ToLower(strings.TrimSpace(req.ContentType))
	extension, ok := avatarExtension(contentType)
	if !ok {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "unsupported avatar image type").
			WithDetails(shared.FieldError{Field: "content_type", Message: "must be image/jpeg, image/png, or image/webp"}))
	}

	ctx := c.Request().Context()
	expiry := h.Cfg.PresignUploadExpiry
	if expiry <= 0 {
		expiry = 15 * time.Minute
	}
	expiresAt := time.Now().UTC().Add(expiry)
	key := fmt.Sprintf("avatars/%s/%s.%s", user.ID, uuid.NewString(), extension)

	pending, err := h.Queries.SetPendingAvatarUpload(ctx, db.SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: key, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: expiresAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("SetPendingAvatarUpload failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "save avatar upload intent"))
	}

	uploadURL, err := h.R2.PresignPut(ctx, key, contentType, expiry)
	if err != nil {
		slog.Error("PresignPut for avatar failed", "err", err, "user_id", user.ID, "avatar_key", key)
		if _, clearErr := h.Queries.ClearPendingAvatarUpload(ctx, db.ClearPendingAvatarUploadParams{
			ID: user.ID, PendingAvatarKey: sql.NullString{String: key, Valid: true},
		}); clearErr != nil {
			slog.Error("clear failed avatar upload intent", "err", clearErr, "user_id", user.ID)
		}
		h.enqueueAvatarCleanup(user.ID, key)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign avatar upload"))
	}

	if pending.PreviousPendingAvatarKey.Valid && pending.PreviousPendingAvatarKey.String != "" {
		h.enqueueAvatarCleanup(user.ID, pending.PreviousPendingAvatarKey.String)
	}

	return shared.RespondCreated(c, avatarUploadIntentResponse{
		AvatarKey:  key,
		HTTPMethod: http.MethodPut,
		UploadURL:  uploadURL,
		UploadHeaders: map[string]string{
			"Content-Type": contentType,
		},
		MaxSizeBytes: maxAvatarBytes,
		ExpiresAt:    expiresAt.Format(time.RFC3339),
	})
}

func (h *AvatarHandler) ConfirmUpload(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Queue == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "avatar handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req avatarConfirmRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	contentType, valid := avatarContentTypeForKey(user.ID, req.AvatarKey)
	if !valid {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "invalid avatar key").
			WithDetails(shared.FieldError{Field: "avatar_key", Message: "must be an upload key issued for the authenticated user"}))
	}

	ctx := c.Request().Context()
	size, err := h.R2.HeadObject(ctx, req.AvatarKey)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrUploadMissing, "avatar upload not found"))
		}
		slog.Error("HeadObject for avatar failed", "err", err, "user_id", user.ID, "avatar_key", req.AvatarKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "inspect avatar upload"))
	}
	if size <= 0 || size > maxAvatarBytes {
		return h.rejectAvatarUpload(c, user.ID, req.AvatarKey,
			shared.Wrap(shared.ErrUploadSizeInvalid, fmt.Sprintf("avatar size %d is outside allowed range", size)))
	}

	imageBytes, err := h.R2.GetObject(ctx, req.AvatarKey, maxAvatarBytes)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrUploadMissing, "avatar upload not found"))
		}
		slog.Error("GetObject for avatar failed", "err", err, "user_id", user.ID, "avatar_key", req.AvatarKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "read avatar upload"))
	}
	if err := validateAvatarImage(imageBytes, contentType); err != nil {
		return h.rejectAvatarUpload(c, user.ID, req.AvatarKey,
			shared.NewAPIError(shared.CodeValidationError, "uploaded file is not a valid supported image").
				WithDetails(shared.FieldError{Field: "avatar_key", Message: err.Error()}))
	}

	avatarURL, err := h.R2.PresignGet(ctx, req.AvatarKey, presignTTL(h.Cfg))
	if err != nil {
		slog.Error("PresignGet for avatar failed", "err", err, "user_id", user.ID, "avatar_key", req.AvatarKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign avatar URL"))
	}
	if avatarURL == "" {
		slog.Error("PresignGet for avatar returned an empty URL", "user_id", user.ID, "avatar_key", req.AvatarKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign avatar URL"))
	}

	updated, err := h.Queries.ConfirmAvatarUpload(ctx, db.ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: req.AvatarKey, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			cleared, clearErr := h.Queries.ClearPendingAvatarUpload(ctx, db.ClearPendingAvatarUploadParams{
				ID: user.ID, PendingAvatarKey: sql.NullString{String: req.AvatarKey, Valid: true},
			})
			if clearErr != nil {
				slog.Error("ClearPendingAvatarUpload after expired avatar intent failed", "err", clearErr, "user_id", user.ID, "avatar_key", req.AvatarKey)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "clear expired avatar upload"))
			}
			if cleared > 0 {
				h.enqueueAvatarCleanup(user.ID, req.AvatarKey)
			}
			return shared.RespondError(c, shared.Wrap(shared.ErrAvatarUploadConflict, "avatar upload intent expired or was replaced"))
		}
		slog.Error("ConfirmAvatarUpload failed", "err", err, "user_id", user.ID, "avatar_key", req.AvatarKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "save avatar"))
	}
	if updated.PreviousAvatarKey.Valid && updated.PreviousAvatarKey.String != "" {
		h.enqueueAvatarCleanup(user.ID, updated.PreviousAvatarKey.String)
	}

	return shared.RespondOK(c, avatarConfirmResponse{AvatarURL: avatarURL})
}

func (h *AvatarHandler) rejectAvatarUpload(c echo.Context, userID uuid.UUID, key string, responseErr error) error {
	cleared, err := h.Queries.ClearPendingAvatarUpload(c.Request().Context(), db.ClearPendingAvatarUploadParams{
		ID: userID, PendingAvatarKey: sql.NullString{String: key, Valid: true},
	})
	if err != nil {
		slog.Error("ClearPendingAvatarUpload after rejected avatar failed", "err", err, "user_id", userID, "avatar_key", key)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "clear invalid avatar upload"))
	}
	if cleared > 0 {
		h.enqueueAvatarCleanup(userID, key)
	}
	return shared.RespondError(c, responseErr)
}

func (h *AvatarHandler) enqueueAvatarCleanup(userID uuid.UUID, key string) {
	if _, err := h.Queue.EnqueueCleanupObjects(shared.CleanupObjectsPayload{Keys: []string{key}}); err != nil {
		slog.Warn("enqueue avatar cleanup failed", "err", err, "user_id", userID, "avatar_key", key)
	}
}

func avatarExtension(contentType string) (string, bool) {
	switch contentType {
	case "image/jpeg":
		return "jpg", true
	case "image/png":
		return "png", true
	case "image/webp":
		return "webp", true
	default:
		return "", false
	}
}

func avatarContentTypeForKey(userID uuid.UUID, key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "avatars" || parts[1] != userID.String() {
		return "", false
	}
	filename := strings.Split(parts[2], ".")
	if len(filename) != 2 {
		return "", false
	}
	if _, err := uuid.Parse(filename[0]); err != nil {
		return "", false
	}
	switch filename[1] {
	case "jpg":
		return "image/jpeg", true
	case "png":
		return "image/png", true
	case "webp":
		return "image/webp", true
	default:
		return "", false
	}
}

func validateAvatarImage(data []byte, expectedContentType string) error {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid image: %w", err)
	}
	contentTypeByFormat := map[string]string{
		"jpeg": "image/jpeg",
		"png":  "image/png",
		"webp": "image/webp",
	}
	actualContentType, ok := contentTypeByFormat[format]
	if !ok || actualContentType != expectedContentType {
		return fmt.Errorf("file content does not match %s", expectedContentType)
	}
	return nil
}
