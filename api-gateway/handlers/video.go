// Package handlers - video.go implements the upload
// intent + confirm endpoints defined in
// planning/04_api_contracts.md section 4 and LLD
// section 7 (Fase 4):
//
//	POST /api/videos/upload-intent
//	POST /api/videos/confirm
//
// Both endpoints sit behind the auth middleware, so the
// current *db.User is always available via
// middleware.UserFromContext.
//
// Errors are funnelled through shared.RespondError so
// the wire format stays consistent with the rest of
// the API. No handler ever calls c.JSON directly, and
// no error is ever swallowed or replaced with a generic
// string before reaching the central error sink
// (per hermes-go-idiomatic).
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// Upload-intent and confirm size + expiry limits. The
// numbers are wire-level contracts (planning/04_api_contracts.md
// section 4) so changing them is a breaking change for
// clients and a separate task.
const (
	// MinUploadBytes is the lower bound for an accepted
	// upload. Anything smaller is treated as a stray
	// half-upload and rejected as UPLOAD_SIZE_INVALID.
	MinUploadBytes int64 = 1024
	// MaxUploadBytes matches the schema's allowed range
	// (200 MB). Confirmed via HeadObject because the
	// presigned PUT cannot enforce it (per LLD A3).
	MaxUploadBytes int64 = 200 * 1024 * 1024
	// uploadContentType is the value the client must put
	// on the PUT to the presigned URL. It is also the
	// value baked into the signature so the R2-side
	// Content-Type check passes.
	uploadContentType = "application/octet-stream"
)

// VideoHandler holds the dependencies for the upload
// intent + confirm endpoints. SQLDB is the
// database/sql handle used for the confirm transaction:
// sqlc's Queries.WithTx requires a *sql.Tx, which only
// *sql.DB can produce (via pgx/v5/stdlib). The user
// handler still uses Queries directly via pgxpool, so
// this file does not touch pgx.ErrNoRows semantics.
type VideoHandler struct {
	Queries *db.Queries
	SQLDB   *sql.DB
	R2      *shared.R2Client
	Queue   *asynq.Client
	Cfg     *shared.APIConfig
}

// NewVideoHandler builds a VideoHandler with all
// dependencies injected. SQLDB must be the *sql.DB
// opened from DATABASE_URL via pgx/v5/stdlib so the
// confirm transaction can call Queries.WithTx. A nil
// SQLDB causes the constructor to return an error so
// a misconfigured main.go fails fast at startup, not
// on the first confirm.
func NewVideoHandler(queries *db.Queries, sqldb *sql.DB, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) (*VideoHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewVideoHandler: queries is nil")
	}
	if sqldb == nil {
		return nil, fmt.Errorf("NewVideoHandler: sqldb is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewVideoHandler: r2 is nil")
	}
	if queue == nil {
		return nil, fmt.Errorf("NewVideoHandler: queue is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewVideoHandler: cfg is nil")
	}
	return &VideoHandler{
		Queries: queries,
		SQLDB:   sqldb,
		R2:      r2,
		Queue:   queue,
		Cfg:     cfg,
	}, nil
}

// uploadIntentRequest is the body of POST
// /api/videos/upload-intent. Title is required and
// bounded by the API contract; description is optional
// with the same upper bound as the contract (5000
// characters - we are conservative here and accept
// either nil or empty string).
type uploadIntentRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// uploadIntentResponse is the on-the-wire shape of
// upload-intent's success envelope. It mirrors
// planning/04_api_contracts.md section 4 exactly.
// CreatedAt / ExpiresAt are RFC3339 UTC strings so the
// client sees a stable format regardless of the
// Go-side time.Time serialisation.
type uploadIntentResponse struct {
	VideoID       string            `json:"video_id"`
	R2Key         string            `json:"r2_key"`
	HTTPMethod    string            `json:"http_method"`
	UploadURL     string            `json:"upload_url"`
	UploadHeaders map[string]string `json:"upload_headers"`
	MinSizeBytes  int64             `json:"min_size_bytes"`
	MaxSizeBytes  int64             `json:"max_size_bytes"`
	ExpiresAt     string            `json:"expires_at"`
}

// UploadIntent creates or reuses a PENDING_UPLOAD row
// and returns a presigned PUT URL for the client to
// upload the raw video to R2.
//
// Behaviour (per API contract + LLD section 7):
//   - If the user has no PENDING_UPLOAD row, a new row
//     is created with r2_key = uploads/<userID>/<newID>/source.mp4.
//     Returns 201 Created.
//   - If the user already has a PENDING_UPLOAD row,
//     that row is reused and its r2_key is rotated to
//     uploads/<userID>/<existingID>/source.mp4. The
//     previous r2_key is enqueued for best-effort
//     cleanup so an orphaned half-upload is not kept
//     forever. Returns 200 OK.
//
// The presigned PUT expires after Cfg.PresignUploadExpiry
// (default 15m). The client must PUT with header
// Content-Type: application/octet-stream.
func (h *VideoHandler) UploadIntent(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req uploadIntentRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}

	title, titleOK := normaliseOptional(req.Title)
	if !titleOK {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "title is required").
			WithDetails(shared.FieldError{Field: "title", Message: "must be between 1 and 200 characters"}))
	}
	if len(title) > 200 {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "title too long").
			WithDetails(shared.FieldError{Field: "title", Message: "must be between 1 and 200 characters"}))
	}
	desc, _ := normaliseOptional(req.Description)
	if len(desc) > 5000 {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "description too long").
			WithDetails(shared.FieldError{Field: "description", Message: "must be at most 5000 characters"}))
	}

	ctx := c.Request().Context()
	existing, err := h.Queries.GetPendingVideoByUser(ctx, user.ID)
	created := false
	var videoID uuid.UUID
	var r2Key string
	var oldKey string

	if err != nil {
		// Distinguish "no row" from a real DB failure.
		// GetPendingVideoByUser is wired via *sql.DB so
		// both sql.ErrNoRows (stdlib) and pgx.ErrNoRows
		// (defence in depth) are checked.
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			videoID = uuid.New()
			r2Key = uploadKey(user.ID, videoID)
			inserted, ierr := h.Queries.InsertVideo(ctx, db.InsertVideoParams{
				UserID:      user.ID,
				R2Key:       r2Key,
				Title:       nullString(title),
				Description: nullString(desc),
			})
			if ierr != nil {
				slog.Error("InsertVideo failed", "err", ierr, "user_id", user.ID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "create pending video"))
			}
			videoID = inserted.ID
			created = true
		} else {
			slog.Error("GetPendingVideoByUser failed", "err", err, "user_id", user.ID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load pending video"))
		}
	} else {
		// Reuse the existing row. Rotate r2_key to the
		// canonical "<userID>/<existingID>/source.mp4"
		// path. Note: DEVIATION - we intentionally do NOT
		// refresh title/description on the existing row
		// because the acceptance criteria only require
		// r2_key rotation and the sqlc surface does not
		// expose a query that updates title/description
		// while preserving status = PENDING_UPLOAD.
		videoID = existing.ID
		oldKey = existing.R2Key
		r2Key = uploadKey(user.ID, existing.ID)
		if oldKey == r2Key {
			// Defensive: if the canonical key happens
			// to match the stored key (very first
			// upload-intent after a previous successful
			// upload where the row was reused), skip
			// the UPDATE - it would be a no-op write
			// that still bumps updated_at and wastes a
			// round trip.
		} else {
			updated, uerr := h.Queries.UpdatePendingVideoR2Key(ctx, db.UpdatePendingVideoR2KeyParams{
				ID:    existing.ID,
				R2Key: r2Key,
			})
			if uerr != nil {
				slog.Error("UpdatePendingVideoR2Key failed", "err", uerr, "video_id", existing.ID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "rotate pending video key"))
			}
			videoID = updated.ID
		}
	}

	expiry := h.Cfg.PresignUploadExpiry
	if expiry <= 0 {
		expiry = 15 * time.Minute
	}
	uploadURL, perr := h.R2.PresignPut(ctx, r2Key, uploadContentType, expiry)
	if perr != nil {
		slog.Error("PresignPut failed", "err", perr, "r2_key", r2Key)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "presign upload url"))
	}

	if !created && oldKey != "" && oldKey != r2Key {
		// Best-effort cleanup of the previous key. If
		// enqueue fails the user still gets a usable
		// presigned URL; the orphan R2 object will be
		// picked up by future cleanup paths.
		if _, qerr := shared.EnqueueCleanupObjects(h.Queue, shared.CleanupObjectsPayload{Keys: []string{oldKey}}); qerr != nil {
			slog.Warn("enqueue cleanup old upload key failed", "err", qerr, "old_key", oldKey, "new_key", r2Key)
		}
	}

	resp := uploadIntentResponse{
		VideoID:    videoID.String(),
		R2Key:      r2Key,
		HTTPMethod: http.MethodPut,
		UploadURL:  uploadURL,
		UploadHeaders: map[string]string{
			"Content-Type": uploadContentType,
		},
		MinSizeBytes: MinUploadBytes,
		MaxSizeBytes: MaxUploadBytes,
		ExpiresAt:    time.Now().UTC().Add(expiry).Format(time.RFC3339),
	}

	if created {
		return shared.RespondCreated(c, resp)
	}
	return shared.RespondOK(c, resp)
}

// normaliseOptional trims whitespace from an optional
// string field and reports whether a non-empty value
// is present. The contract says title is required and
// description is optional; we treat "missing" and
// "empty after trim" identically so a whitespace-only
// title is rejected with the same validation error as
// a missing one.
func normaliseOptional(p *string) (string, bool) {
	if p == nil {
		return "", false
	}
	s := strings.TrimSpace(*p)
	return s, s != ""
}

// uploadKey is the canonical R2 key for the raw upload
// of a given video. Path layout matches LLD_PLAN
// section 7 and the API contract example exactly:
// uploads/<userID>/<videoID>/source.mp4.
func uploadKey(userID, videoID uuid.UUID) string {
	return fmt.Sprintf("uploads/%s/%s/source.mp4", userID.String(), videoID.String())
}

// confirmRequest is the body of POST /api/videos/confirm.
// video_id and r2_key are both required and non-empty.
type confirmRequest struct {
	VideoID *string `json:"video_id"`
	R2Key   *string `json:"r2_key"`
}

// confirmResponse is the on-the-wire shape of confirm's
// success envelope per planning/04_api_contracts.md
// section 4. retry_count is always 0 on success because
// ConfirmVideoProcessing resets it.
type confirmResponse struct {
	VideoID    string `json:"video_id"`
	Status     string `json:"status"`
	RetryCount int    `json:"retry_count"`
}

// ConfirmUpload flips a PENDING_UPLOAD video to
// PROCESSING after validating the uploaded R2 object
// is present, in the allowed size range, and owned by
// the caller.
//
// Flow (per LLD_PLAN section 7):
//  1. Parse + validate body.
//  2. Begin tx, GetVideoByIDForUpdate (row lock).
//  3. Validate ownership / status / r2_key match.
//  4. HeadObject on R2:
//     - missing -> 409 UPLOAD_MISSING (tx still commits,
//       status stays PENDING_UPLOAD so the user can
//       retry).
//     - size outside 1 KB..200 MB -> 400
//       UPLOAD_SIZE_INVALID + enqueue cleanup of the
//       invalid object. Tx still commits so the row
//       stays PENDING_UPLOAD and the user can retry.
//  5. ConfirmVideoProcessing (PENDING_UPLOAD ->
//     PROCESSING, retry_count=0).
//  6. Enqueue transcode:video with asynq.MaxRetry(1).
//     If enqueue fails the tx is rolled back so the
//     video stays PENDING_UPLOAD and the client gets
//     500 to retry the confirm.
//
// Failure modes (all funnel through shared.RespondError):
//   - video not found, or video not owned by caller -> 404 NOT_FOUND
//   - status != PENDING_UPLOAD -> 409 VIDEO_STATUS_CONFLICT
//   - r2_key mismatch           -> 400 VALIDATION_ERROR
//   - R2 missing object         -> 409 UPLOAD_MISSING
//   - R2 size out of range      -> 400 UPLOAD_SIZE_INVALID
//   - enqueue failure           -> 500 INTERNAL_ERROR
func (h *VideoHandler) ConfirmUpload(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req confirmRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if req.VideoID == nil || strings.TrimSpace(*req.VideoID) == "" {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "video_id is required").
			WithDetails(shared.FieldError{Field: "video_id", Message: "must be a non-empty UUID"}))
	}
	videoID, err := uuid.Parse(*req.VideoID)
	if err != nil {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "invalid video_id").
			WithDetails(shared.FieldError{Field: "video_id", Message: "must be a valid UUID"}))
	}
	if req.R2Key == nil || strings.TrimSpace(*req.R2Key) == "" {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "r2_key is required").
			WithDetails(shared.FieldError{Field: "r2_key", Message: "must be a non-empty string"}))
	}
	bodyR2Key := strings.TrimSpace(*req.R2Key)

	ctx := c.Request().Context()

	// Begin tx so GetVideoByIDForUpdate + ConfirmVideoProcessing
	// form an atomic state transition. The whole confirm
	// pipeline runs under one tx so an enqueue failure
	// later can roll the row back to PENDING_UPLOAD.
	tx, err := h.SQLDB.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("BeginTx failed", "err", err, "user_id", user.ID, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "begin transaction"))
	}
	// Rollback is a no-op after a successful Commit, so
	// deferring it here is the standard idiom - the tx
	// is closed on every return path.
	defer func() { _ = tx.Rollback() }()

	qtx := h.Queries.WithTx(tx)
	video, err := qtx.GetVideoByIDForUpdate(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		slog.Error("GetVideoByIDForUpdate failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	if video.UserID != user.ID {
		// Anti-enumeration: a non-owner asking about a
		// video that exists gets the same response as a
		// missing video.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
	}
	if video.Status != "PENDING_UPLOAD" {
		return shared.RespondError(c, shared.Wrap(shared.ErrVideoStatusConflict, "video is not in a confirmable state"))
	}
	if video.R2Key != bodyR2Key {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "r2_key does not match video").
			WithDetails(shared.FieldError{Field: "r2_key", Message: "does not match the key on file"}))
	}

	// Validate the R2 object itself. Per LLD A3 the
	// presigned PUT cannot enforce content-length-range,
	// so we read the actual size here.
	size, herr := h.R2.HeadObject(ctx, video.R2Key)
	if herr != nil {
		if errors.Is(herr, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrUploadMissing, "uploaded object not found in storage"))
		}
		slog.Error("HeadObject failed", "err", herr, "r2_key", video.R2Key)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "inspect uploaded object"))
	}
	if size < MinUploadBytes || size > MaxUploadBytes {
		// Best-effort cleanup of the invalid object so
		// R2 doesn't keep accumulating garbage. Failures
		// are logged but do not change the response.
		if _, qerr := shared.EnqueueCleanupObjects(h.Queue, shared.CleanupObjectsPayload{Keys: []string{video.R2Key}}); qerr != nil {
			slog.Warn("enqueue cleanup invalid upload failed", "err", qerr, "r2_key", video.R2Key, "size", size)
		}
		return shared.RespondError(c, shared.NewAPIError(shared.CodeUploadSizeInvalid, fmt.Sprintf("uploaded object size %d is outside the allowed range", size)).
			WithDetails(shared.FieldError{Field: "r2_key", Message: fmt.Sprintf("size must be between %d and %d bytes", MinUploadBytes, MaxUploadBytes)}))
	}

	// Flip status inside the tx so the row lock holds
	// for the entire check-and-flip.
	confirmed, cerr := qtx.ConfirmVideoProcessing(ctx, db.ConfirmVideoProcessingParams{
		ID:     video.ID,
		UserID: video.UserID,
		R2Key:  video.R2Key,
	})
	if cerr != nil {
		if errors.Is(cerr, sql.ErrNoRows) || errors.Is(cerr, pgx.ErrNoRows) {
			// Race: another request mutated the row
			// between our FOR UPDATE and the UPDATE
			// (e.g. a concurrent confirm). Surface
			// 409 so the client can retry.
			return shared.RespondError(c, shared.Wrap(shared.ErrVideoStatusConflict, "video state changed concurrently"))
		}
		slog.Error("ConfirmVideoProcessing failed", "err", cerr, "video_id", video.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "confirm video"))
	}

	// Enqueue the transcode task.
	//
	// Retry model (two layers, do not confuse):
	//   1. asynq-level retry: asynq.MaxRetry(1) lets
	//      the queue itself retry the task ONCE on
	//      transient failures such as a Redis blip
	//      before dropping it. This is the queue's
	//      own safety net; the producer hands control
	//      over the moment EnqueueTranscode returns.
	//   2. application-level retry (handled by the
	//      transcoder worker in fase 5): the worker
	//      catches a transcode error, checks
	//      retry_count < 3 against the videos row, and
	//      re-enqueues a fresh transcode:video task
	//      with ProcessIn(30s * retry_count). This is
	//      where the PRD "retry maksimal 3 kali" rule
	//      is enforced - per the LLD retry section.
	//
	// The producer therefore sets MaxRetry(1) so a
	// stuck task fails fast into the application-level
	// retry path instead of being silently retried
	// by asynq forever.
	info, qerr := shared.EnqueueTranscode(h.Queue, shared.TranscodeVideoPayload{VideoID: confirmed.ID.String()}, asynq.MaxRetry(1))
	if qerr != nil {
		// Roll back so the row stays PENDING_UPLOAD
		// and the client can retry the confirm.
		if rerr := tx.Rollback(); rerr != nil {
			slog.Error("rollback after enqueue failure also failed", "err", rerr, "video_id", video.ID)
		}
		slog.Error("EnqueueTranscode failed", "err", qerr, "video_id", video.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "enqueue transcode"))
	}
	slog.Info("transcode task enqueued", "video_id", video.ID, "task_id", info.ID, "queue", info.Queue)

	if cerr := tx.Commit(); cerr != nil {
		slog.Error("tx.Commit failed", "err", cerr, "video_id", video.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit confirm transaction"))
	}

	return shared.RespondOK(c, confirmResponse{
		VideoID:    confirmed.ID.String(),
		Status:     confirmed.Status,
		RetryCount: int(confirmed.RetryCount),
	})
}

// nullString wraps a plain string in sql.NullString so
// the sqlc InsertVideo query can carry empty titles
// through the database/sql interface. Title and
// description are NULL when the client omitted them.
func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}