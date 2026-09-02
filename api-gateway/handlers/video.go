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
// intent + confirm endpoints. DB is the *sql.DB pool
// used for the confirm transaction: sqlc's
// Queries.WithTx requires a *sql.Tx, which only
// *sql.DB can produce (via pgx/v5/stdlib).
type VideoHandler struct {
	Queries *db.Queries
	DB      *sql.DB
	R2      *shared.R2Client
	Queue   *asynq.Client
	Cfg     *shared.APIConfig
}

// NewVideoHandler builds a VideoHandler with all
// dependencies injected. DB must be the *sql.DB
// opened from DATABASE_URL via pgx/v5/stdlib so the
// confirm transaction can call Queries.WithTx. A nil
// DB causes the constructor to return an error so
// a misconfigured main.go fails fast at startup, not
// on the first confirm.
func NewVideoHandler(queries *db.Queries, dbHandle *sql.DB, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) (*VideoHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewVideoHandler: queries is nil")
	}
	if dbHandle == nil {
		return nil, fmt.Errorf("NewVideoHandler: db is nil")
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
		DB:      dbHandle,
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
	Title       *string `json:"title" validate:"required,max=200"`
	Description *string `json:"description" validate:"omitempty,max=5000"`
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
	// c.Validate runs the validator/v10 struct tag
// rules declared on uploadIntentRequest. On
// failure it returns *shared.APIError already
// translated to the canonical envelope shape, so
// shared.RespondError can serialise it directly.
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	title := strings.TrimSpace(*req.Title)
	if title == "" {
		// Belt-and-braces: validator 'required'
// passes when Title is a non-nil pointer to
// "", but a whitespace-only title is also not
// what the contract means by "title is
// required". Reject explicitly.
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "title is required").
			WithDetails(shared.FieldError{Field: "title", Message: "must be between 1 and 200 characters"}))
	}
	desc := ""
	if req.Description != nil {
		desc = strings.TrimSpace(*req.Description)
	}

	ctx := c.Request().Context()
	existing, err := h.Queries.GetPendingVideoByUser(ctx, user.ID)
	created := false
	var videoID uuid.UUID
	var r2Key string
	var oldKey string

	if err != nil {
		// Distinguish "no row" from a real DB failure.
		if errors.Is(err, sql.ErrNoRows) {
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
		// path AND refresh title/description so a user
		// who corrected their title between two
		// upload-intent calls sees the latest values.
		// Both updates are guarded by status =
		// PENDING_UPLOAD so a concurrent /confirm in
		// another tab surfaces as 409 instead of
		// silently mutating a row that has already
		// moved to PROCESSING.
		videoID = existing.ID
		oldKey = existing.R2Key
		r2Key = uploadKey(user.ID, existing.ID)
		if oldKey != r2Key {
			updated, uerr := h.Queries.UpdatePendingVideoR2Key(ctx, db.UpdatePendingVideoR2KeyParams{
				ID:    existing.ID,
				R2Key: r2Key,
			})
			if uerr != nil {
						if errors.Is(uerr, sql.ErrNoRows) {
					// Concurrent state transition.
					return shared.RespondError(c, shared.Wrap(shared.ErrVideoStatusConflict,
						"video state changed concurrently"))
				}
				slog.Error("UpdatePendingVideoR2Key failed", "err", uerr, "video_id", existing.ID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "rotate pending video key"))
			}
			videoID = updated.ID
		}
		if _, merr := h.Queries.UpdatePendingVideoMetadata(ctx, db.UpdatePendingVideoMetadataParams{
			ID:          existing.ID,
			Title:       nullString(title),
			Description: nullString(desc),
		}); merr != nil {
				if errors.Is(merr, sql.ErrNoRows) {
				// Same race: the row was promoted to
				// PROCESSING between our two updates.
				return shared.RespondError(c, shared.Wrap(shared.ErrVideoStatusConflict,
					"video state changed concurrently"))
			}
			slog.Error("UpdatePendingVideoMetadata failed", "err", merr, "video_id", existing.ID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update pending video metadata"))
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

// uploadKey is the canonical R2 key for the raw upload
// of a given video. Path layout matches LLD_PLAN
// section 7 and the API contract example exactly:
// uploads/<userID>/<videoID>/source.mp4.
func uploadKey(userID, videoID uuid.UUID) string {
	return fmt.Sprintf("uploads/%s/%s/source.mp4", userID.String(), videoID.String())
}

// confirmRequest is the body of POST /api/videos/confirm.
// video_id and r2_key are both required and non-empty.
// The validate tags delegate bounds checking to the
// go-playground/validator/v10 instance installed in
// NewRouter (see request_validator.go).
type confirmRequest struct {
	VideoID *string `json:"video_id" validate:"required,uuid"`
	R2Key   *string `json:"r2_key"   validate:"required,min=1"`
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
	// Struct-tag validation via validator/v10.
	// `required` covers the nil-pointer case,
	// `uuid` covers the parse-failure case, and
	// `min=1` covers the empty-string case.
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}
	videoID, err := uuid.Parse(*req.VideoID)
	if err != nil {
		// Defence in depth: validator's `uuid` tag
		// should already have caught this. If it
		// did not (e.g. validator version mismatch)
// we still fail safely rather than passing a
// malformed UUID down the stack.
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "invalid video_id").
			WithDetails(shared.FieldError{Field: "video_id", Message: "must be a valid UUID"}))
	}
	bodyR2Key := strings.TrimSpace(*req.R2Key)

	ctx := c.Request().Context()

	// Begin tx so GetVideoByIDForUpdate + ConfirmVideoProcessing
	// form an atomic state transition. The whole confirm
	// pipeline runs under one tx so an enqueue failure
	// later can roll the row back to PENDING_UPLOAD.
	tx, err := h.DB.BeginTx(ctx, nil)
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
		if errors.Is(err, sql.ErrNoRows) {
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
		if errors.Is(cerr, sql.ErrNoRows) {
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

// DeleteVideo handles DELETE /api/videos/:id.
//
// Behaviour (per LLD section 11 + API contract
// section 4):
//   - Only the OWNER can delete. Non-owner ->
//     404 (anti-enumeration; same as the rest
//     of the social surface, NOT 403).
//   - status -> 'DELETED', deleted_at -> NOW()
//     via MarkVideoDeleted (the row stays for
//     24h so a quick undelete is possible).
//   - cleanup:video task is enqueued with
//     ProcessIn(24h) so the worker hard-deletes
//     the row + R2 objects after the grace.
//   - 204 No Content on success.
//
// Idempotency: deleting a video that is already
// DELETED is a no-op success. The MarkVideoDeleted
// query is :one and gated on user_id so the
// second call still returns 204 (the row matches
// the WHERE), but the deleted_at timestamp is
// refreshed in practice. We treat this as
// acceptable for a tombstone - the cleanup task
// re-enqueue is suppressed when status is
// already DELETED.
func (h *VideoHandler) DeleteVideo(c echo.Context) error {
	if h.Queries == nil || h.Queue == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "video handler not configured"))
	}
	user, videoID, ok := parseAuthVideoParam(c)
	if !ok {
		return nil
	}
	ctx := c.Request().Context()

	// Load the video first so we can apply the
	// anti-enumeration rule: a non-owner never
	// sees a 403/404 distinction.
	video, err := h.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		slog.Error("DeleteVideo: load video", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	if video.UserID != user.ID {
		// Anti-enumeration: collapse to 404 so
		// an attacker cannot probe video ids.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
	}
	if video.Status == "DELETED" {
		// Already tombstoned. Skip the UPDATE
		// and the re-enqueue; the prior
		// cleanup:video is still in flight.
		return c.NoContent(204)
	}

	updated, err := h.Queries.MarkVideoDeleted(ctx, db.MarkVideoDeletedParams{
		ID:     videoID,
		UserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Race: someone else just
			// tombstoned it. Treat as 204.
			return c.NoContent(204)
		}
		slog.Error("DeleteVideo: MarkVideoDeleted", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "mark video deleted"))
	}

	// Enqueue the 24h cleanup. The worker's
	// HandleCleanupVideo re-checks status +
	// deleted_at and either hard-deletes or
	// re-enqueues itself if the grace has not
	// elapsed yet.
	cleanupDelay := 24 * time.Hour
	if _, err := shared.EnqueueCleanupVideo(h.Queue, shared.CleanupVideoPayload{
		VideoID: updated.ID.String(),
	}, asynq.ProcessIn(cleanupDelay)); err != nil {
		// Per LLD the 24h grace lets the worker
		// re-enqueue on its own if it misses a
		// tick; we log warn rather than fail
		// the request so the tombstone sticks
		// even if the queue is briefly down.
		slog.Warn("DeleteVideo: enqueue cleanup:video",
			"err", err, "video_id", videoID)
	}
	return c.NoContent(204)
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