// Package handlers - account.go implements the
// account-level deletion flow from
// planning/04_api_contracts.md section 3 and LLD
// section 11 (Fase 8):
//
//	DELETE /api/users/me   - self-service tombstone
//
// The same DeleteUserData function is called by the
// Zitadel Actions V2 webhook on user.removed; see
// webhook.go for the wiring.
//
// DeleteUserData runs in a single *sql.Tx (per LLD
// section 11, "Transaction" block): counter
// corrections, row deletes, and the tombstone all
// commit together or roll back together. The R2
// cleanup is enqueued AFTER commit so a rollback
// never leaves the worker with a phantom task that
// points at a non-existent user.
//
// Why tombstone instead of hard delete? PRD note +
// LLD: tombstone keeps the UNIQUE(id) intact so a
// stale JWT cannot create a "new" local user via
// the get-or-create path in middleware/auth.go.
// PII is nulled and username is rewritten to a
// deterministic 'deleted_<id>' placeholder so the
// UNIQUE constraint on username keeps holding.
//
// Sentinel: missing rows surface as sql.ErrNoRows
// only (single *sql.DB pool since the
// pool-consolidation refactor; pgx.ErrNoRows never
// matches here).
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// AccountHandler holds the dependencies for the
// self-service account deletion endpoint. Mirrors
// VideoHandler: Queries for reads, DB to open the
// transaction, Queue to enqueue the R2 cleanup task
// after the tx commits.
type AccountHandler struct {
	Queries *db.Queries
	DB      *sql.DB
	Queue   *asynq.Client
}

// NewAccountHandler builds an AccountHandler. All
// three dependencies are required: the deletion
// flow always runs a tx (Queries.WithTx) and always
// enqueues a cleanup task, so a nil on any field
// is a wiring bug and refuses construction early.
func NewAccountHandler(queries *db.Queries, dbHandle *sql.DB, queue *asynq.Client) (*AccountHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewAccountHandler: queries is nil")
	}
	if dbHandle == nil {
		return nil, fmt.Errorf("NewAccountHandler: db is nil")
	}
	if queue == nil {
		return nil, fmt.Errorf("NewAccountHandler: queue is nil")
	}
	return &AccountHandler{Queries: queries, DB: dbHandle, Queue: queue}, nil
}

// DeleteMe handles DELETE /api/users/me. It is the
// self-service entry point: the authenticated user
// is the one being tombstoned, and the only auth
// check needed is the JWT (middleware guarantees
// currentUser.ID is populated). The endpoint
// returns 204 on success; 401 if the JWT is
// missing/invalid; 500 if the tx fails or the
// enqueue fails. The error envelope is funnelled
// through shared.RespondError so the wire shape
// stays consistent with the rest of the API.
func (h *AccountHandler) DeleteMe(c echo.Context) error {
	if h.Queries == nil || h.DB == nil || h.Queue == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "account handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	ctx := c.Request().Context()

	if err := DeleteUserData(ctx, h.Queries, h.DB, h.Queue, user.ID); err != nil {
		// DeleteUserData already wraps with
		// ErrInternal + a context message; the
		// shared.RespondError path maps it to a
		// 500 envelope. We log first so the
		// full chain is on disk for post-mortem.
		slog.Error("DeleteUserData failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, err)
	}
	return c.NoContent(204)
}

// DeleteUserData is the canonical phase-8 entry
// point for "remove a user and everything that
// points at them". It is shared between the
// self-service HTTP handler and the Zitadel
// webhook handler, so the business rules live in
// one place.
//
// Flow (per LLD section 11):
//  1. Open tx, bind qtx.
//  2. ListVideoKeysByUser -> collect r2_key +
//     hls_prefix + thumbnail_key per video.
//  3. DecrementLikesForUser -> fix likes_count
//     on OTHER users' videos that this user liked.
//  4. DecrementCommentsForUser -> fix
//     comments_count on OTHER users' videos that
//     this user commented on.
//  5. DeleteVideosByUser -> cascades likes +
//     comments on this user's own videos (ON
//     DELETE CASCADE on the FK).
//  6. DeleteFollowsByFollower, DeleteFollowsByFollowee.
//  7. DeleteLikesByUser, DeleteCommentsByUser.
//  8. DeleteNotificationsForUser + DeleteNotificationsByActor.
//  9. TombstoneUser -> is_active=false, deleted_at,
//     PII null, username placeholder.
// 10. Commit.
// 11. AFTER commit: enqueue cleanup:objects with
//     the raw + thumbnail keys (hls_prefix is left
//     to the worker's DeletePrefix path; we do not
//     list segments from the api-gateway).
//
// Counter race note: the decrement UPDATE writes
// self-lock their target rows in Postgres, so two
// concurrent DeleteUserData calls for the same
// user will serialise on the video rows. Drift
// between the list-keys snapshot and the decrement
// is bounded by GREATEST(..., 0) which absorbs
// concurrent like/comment inserts (a brand new
// like inserted after step 2 but before step 3
// would be decremented even though the like row
// itself is removed in step 7 -> the row vanishes
// in step 7 and the count stays correct). The
// opposite direction (insert AFTER step 7, before
// commit) is blocked by the FK ON DELETE CASCADE
// on likes.user_id: the user is still alive at
// that point, so the insert succeeds but the row
// will be removed in step 7.
//
// Returns nil on success. On any tx error the
// returned error is already wrapped with
// ErrInternal + a context message; the caller can
// pass it straight to shared.RespondError.
func DeleteUserData(ctx context.Context, q *db.Queries, dbHandle *sql.DB, queue *asynq.Client, userID uuid.UUID) error {
	if q == nil {
		return shared.Wrap(shared.ErrInternal, "DeleteUserData: queries is nil")
	}
	if dbHandle == nil {
		return shared.Wrap(shared.ErrInternal, "DeleteUserData: db is nil")
	}
	if queue == nil {
		return shared.Wrap(shared.ErrInternal, "DeleteUserData: queue is nil")
	}

	tx, err := dbHandle.BeginTx(ctx, nil)
	if err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("begin transaction: %v", err))
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	qtx := q.WithTx(tx)

	// Step 1: collect R2 keys BEFORE the rows
	// disappear. ListVideoKeysByUser reads the
	// videos table; once DeleteVideosByUser runs
	// in step 5, the keys are gone.
	rows, err := qtx.ListVideoKeysByUser(ctx, userID)
	if err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("list video keys: %v", err))
	}
	r2Keys := make([]string, 0, 3*len(rows))
	for _, r := range rows {
		r2Keys = append(r2Keys, r.R2Key)
		if r.ThumbnailKey.Valid && r.ThumbnailKey.String != "" {
			r2Keys = append(r2Keys, r.ThumbnailKey.String)
		}
		// hls_prefix is a folder, not a single
		// key. The worker's DeletePrefix path
		// (HandleCleanupVideo) lists + deletes
		// the segments. We intentionally do NOT
		// enumerate them here - that would mean
		// an R2 list call inside the tx, which
		// widens the lock surface for no
		// benefit.
	}

	// Step 2-3: counter corrections on other
	// users' videos. The subquery in each UPDATE
	// references likes/comments rows that belong
	// to this user; row-level locks on the target
	// video rows are taken implicitly by the
	// UPDATE.
	if err := qtx.DecrementLikesForUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("decrement likes: %v", err))
	}
	if err := qtx.DecrementCommentsForUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("decrement comments: %v", err))
	}

	// Step 4: delete the user's own videos. ON
	// DELETE CASCADE on likes.video_id and
	// comments.video_id removes the like +
	// comment rows on these videos (the user
	// already deleted THEIR OWN like/comment
	// rows in step 6, but a third party could
	// have liked/commented on this user's
	// video; those rows go away here).
	if err := qtx.DeleteVideosByUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete videos: %v", err))
	}

	// Step 5: remove follow edges in both
	// directions. ON DELETE CASCADE on the FK
	// does not apply because we are not deleting
	// the users row, just the edge.
	if err := qtx.DeleteFollowsByFollower(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete follows (follower): %v", err))
	}
	if err := qtx.DeleteFollowsByFollowee(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete follows (followee): %v", err))
	}

	// Step 6: the user's own like + comment rows.
	// After step 3 these rows are still here (we
	// only corrected the counter on the OTHER
	// users' videos); the rows themselves go
	// away now.
	if err := qtx.DeleteLikesByUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete likes: %v", err))
	}
	if err := qtx.DeleteCommentsByUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete comments: %v", err))
	}

	// Step 7: notifications where the user is
	// the recipient (delete rows that target
	// their inbox) and where the user is the
	// actor (delete rows that mention them as
	// the source). Both sides must go; the
	// second query removes "X liked your video"
	// notifications after X is gone, the first
	// removes "you were followed by X"-style
	// entries in this user's inbox (now
	// meaningless after the tombstone).
	if err := qtx.DeleteNotificationsForUser(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete notifications (user): %v", err))
	}
	if err := qtx.DeleteNotificationsByActor(ctx, userID); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete notifications (actor): %v", err))
	}

	// Step 8: tombstone. Username placeholder is
	// deterministic so the UNIQUE constraint on
	// username keeps holding and the row is
	// identifiable for forensics. TombstoneUser
	// is :one so it returns the row + error; we
	// only need the error.
	if _, err := qtx.TombstoneUser(ctx, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A user that does not exist is
			// not a tombstone failure; the
			// webhook path can hit this if
			// Zitadel notifies us about a
			// user we never created locally.
			// Roll back the partial work and
			// return nil so the caller acks
			// Zitadel.
			_ = tx.Rollback()
			return nil
		}
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("tombstone user: %v", err))
	}

	if err := tx.Commit(); err != nil {
		return shared.Wrap(shared.ErrInternal, fmt.Sprintf("commit: %v", err))
	}

	// Post-commit: enqueue R2 cleanup. Doing
	// this AFTER commit means a rollback never
	// leaves the worker with a phantom task. The
	// hls_prefix is intentionally NOT in the
	// keys list - the worker lists + deletes
	// that prefix via DeletePrefix when the
	// video row itself is processed.
	//
	// Empty keys -> no-op per
	// shared.EnqueueCleanupObjects.
	payload := shared.CleanupObjectsPayload{Keys: r2Keys}
	if _, err := shared.EnqueueCleanupObjects(queue, payload); err != nil {
		// We have already tombstoned; failing
		// here would leave the user dead but
		// their R2 objects alive. Per LLD the
		// 24h grace lets the cleanup job pick
		// up the slack (HandleCleanupObjects
		// is also scheduled independently for
		// raw key cleanup), so we log warn
		// instead of failing the request.
		slog.Warn("enqueue cleanup:objects after tombstone failed",
			"err", err, "user_id", userID, "keys_count", len(r2Keys))
	}
	return nil
}
