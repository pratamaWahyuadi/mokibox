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
// deleteUserData runs in a single tx (per LLD
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
// Testability refactor: AccountHandler holds the
// consumer-side interfaces (accountTxRunner +
// taskEnqueuer) so the whole cascade is unit-testable
// with stubs; the exported DeleteUserData adapter
// keeps the webhook.go call site unchanged.
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

// accountTxStore is the delete-cascade tx body: the 10
// qtx calls DeleteUserData performs, bound to the
// transaction. Satisfied by *db.Queries via WithTx.
type accountTxStore interface {
	ListVideoKeysByUser(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error)
	DecrementLikesForUser(ctx context.Context, userID uuid.UUID) error
	DecrementCommentsForUser(ctx context.Context, userID uuid.UUID) error
	DeleteVideosByUser(ctx context.Context, userID uuid.UUID) error
	DeleteFollowsByFollower(ctx context.Context, followerID uuid.UUID) error
	DeleteFollowsByFollowee(ctx context.Context, followeeID uuid.UUID) error
	DeleteLikesByUser(ctx context.Context, userID uuid.UUID) error
	DeleteCommentsByUser(ctx context.Context, userID uuid.UUID) error
	DeleteNotificationsForUser(ctx context.Context, userID uuid.UUID) error
	DeleteNotificationsByActor(ctx context.Context, actorID uuid.UUID) error
	TombstoneUser(ctx context.Context, id uuid.UUID) (db.User, error)
}

// AccountHandler holds the dependencies for the
// self-service account deletion endpoint. Run opens
// the delete-cascade tx; Queue enqueues the R2
// cleanup after commit. Both are interfaces so unit
// tests stub them (account_test.go).
type AccountHandler struct {
	Run   txRunner[accountTxStore]
	Queue taskEnqueuer
}

// NewAccountHandler builds an AccountHandler from the
// production concretes. All three are required: the
// deletion flow always runs a tx (Queries.WithTx) and
// always enqueues a cleanup task, so a nil on any
// dependency is a wiring bug and refuses construction
// early.
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
	return &AccountHandler{
		Run: NewSQLTxRunner(dbHandle, func(tx *sql.Tx) accountTxStore {
			return queries.WithTx(tx)
		}),
		Queue: asynqEnqueuer{client: queue},
	}, nil
}

// newAccountHandlerForTest lets the test suite build
// the handler directly from interfaces.
func newAccountHandlerForTest(runner txRunner[accountTxStore], queue taskEnqueuer) *AccountHandler {
	return &AccountHandler{Run: runner, Queue: queue}
}

// DeleteMe handles DELETE /api/users/me. It is the
// self-service entry point: the authenticated user
// is the one being tombstoned, and the only auth
// check needed is the JWT (middleware guarantees
// currentUser.ID is populated). The endpoint
// returns 204 on success; 401 if the JWT is
// missing/invalid; 500 if the tx fails or the
// enqueue fails. The error envelope is funnelled
// through shared.RespondError so the wire shape stays
// consistent with the rest of the API.
func (h *AccountHandler) DeleteMe(c echo.Context) error {
	if h.Run == nil || h.Queue == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "account handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	ctx := c.Request().Context()

	if err := deleteUserData(ctx, h.Run, h.Queue, user.ID); err != nil {
		// deleteUserData already wraps with
		// ErrInternal + a context message; the
		// shared.RespondError path maps it to a
		// 500 envelope. We log first so the
		// full chain is on disk for post-mortem.
		slog.Error("deleteUserData failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, err)
	}
	return c.NoContent(204)
}

// DeleteUserData is the canonical phase-8 entry
// point for "remove a user and everything that
// points at them", kept as the exported concrete
// adapter so webhook.go's user.removed path stays
// unchanged. The business rules live in deleteUserData.
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
	runner := NewSQLTxRunner(dbHandle, func(tx *sql.Tx) accountTxStore {
		return q.WithTx(tx)
	})
	return deleteUserData(ctx, runner, asynqEnqueuer{client: queue}, userID)
}

// deleteUserData is the testable core of the delete
// cascade. Flow (per LLD section 11):
//  1. Tx body (via the runner): ListVideoKeysByUser
//     (collect r2_key + thumbnail keys first) ->
//     DecrementLikesForUser / DecrementCommentsForUser
//     (counter corrections on other users' videos) ->
//     DeleteVideosByUser (cascades likes + comments on
//     this user's own videos) -> delete follow edges
//     both directions -> delete the user's own like +
//     comment rows -> delete notifications where the
//     user is recipient or actor -> TombstoneUser.
//  2. Commit (runner).
//  3. AFTER commit: enqueue cleanup:objects with the
//     raw + thumbnail keys (hls_prefix is left to the
//     worker's DeletePrefix path; we do not list
//     segments from the api-gateway).
//
// TombstoneUser returning sql.ErrNoRows means the user
// never existed locally (webhook path can hit this);
// the tx rolls back the partial work and the function
// returns nil so the caller acks Zitadel.
//
// Counter race note (unchanged from the pre-refactor
// implementation): the decrement UPDATEs self-lock their
// target rows; drift is bounded by GREATEST(..., 0).
func deleteUserData(ctx context.Context, runner txRunner[accountTxStore], queue taskEnqueuer, userID uuid.UUID) error {
	r2Keys, err := collectUserR2Keys(ctx, runner, userID)
	if err != nil {
		return err
	}
	if r2Keys == nil {
		// TombstoneUser returned sql.ErrNoRows inside
		// collectUserR2Keys: the user never existed
		// locally. Nothing was committed; ack the caller.
		return nil
	}

	// Post-commit: enqueue R2 cleanup. Doing this AFTER
	// commit means a rollback never leaves the worker
	// with a phantom task. The hls_prefix is
	// intentionally NOT in the keys list - the worker
	// lists + deletes that prefix via DeletePrefix when
	// the video row itself is processed.
	//
	// Empty keys -> no-op per EnqueueCleanupObjects.
	payload := shared.CleanupObjectsPayload{Keys: r2Keys}
	if _, err := queue.EnqueueCleanupObjects(payload); err != nil {
		// We have already tombstoned; failing here would
		// leave the user dead but their R2 objects alive.
		// Per LLD the 24h grace lets the cleanup job pick
		// up the slack (HandleCleanupObjects is also
		// scheduled independently for raw key cleanup),
		// so we log warn instead of failing the request.
		slog.Warn("enqueue cleanup:objects after tombstone failed",
			"err", err, "user_id", userID, "keys_count", len(r2Keys))
	}
	return nil
}

// collectUserR2Keys runs the whole delete-cascade tx
// and returns the R2 keys collected before the rows
// disappear. A nil return (without error) means the
// user row never existed: the tx body rolled back and
// there is nothing to clean up.
func collectUserR2Keys(ctx context.Context, runner txRunner[accountTxStore], userID uuid.UUID) ([]string, error) {
	var r2Keys []string
	err := runner.Run(ctx, func(qtx accountTxStore) error {
		// Step 1: collect R2 keys BEFORE the rows
		// disappear. ListVideoKeysByUser reads the
		// videos table; once DeleteVideosByUser runs,
		// the keys are gone.
		rows, err := qtx.ListVideoKeysByUser(ctx, userID)
		if err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("list video keys: %v", err))
		}
		r2Keys = make([]string, 0, 3*len(rows))
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
			// an R2 list call inside the tx.
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
		// directions.
		if err := qtx.DeleteFollowsByFollower(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete follows (follower): %v", err))
		}
		if err := qtx.DeleteFollowsByFollowee(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete follows (followee): %v", err))
		}

		// Step 6: the user's own like + comment rows.
		if err := qtx.DeleteLikesByUser(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete likes: %v", err))
		}
		if err := qtx.DeleteCommentsByUser(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete comments: %v", err))
		}

		// Step 7: notifications where the user is
		// the recipient and where the user is the
		// actor. Both sides must go.
		if err := qtx.DeleteNotificationsForUser(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete notifications (user): %v", err))
		}
		if err := qtx.DeleteNotificationsByActor(ctx, userID); err != nil {
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("delete notifications (actor): %v", err))
		}

		// Step 8: tombstone. Username placeholder is
		// deterministic so the UNIQUE constraint on
		// username keeps holding.
		if _, err := qtx.TombstoneUser(ctx, userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// A user that does not exist is
				// not a tombstone failure; the
				// webhook path can hit this if
				// Zitadel notifies us about a
				// user we never created locally.
				// Signal "no user" via the nil
				// keys sentinel; the caller acks
				// Zitadel.
				r2Keys = nil
				return errNoLocalUser
			}
			return shared.Wrap(shared.ErrInternal, fmt.Sprintf("tombstone user: %v", err))
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNoLocalUser) {
			return nil, nil
		}
		return nil, err
	}
	return r2Keys, nil
}

// errNoLocalUser is the internal sentinel flowing from
// the tx body to collectUserR2Keys: TombstoneUser hit
// sql.ErrNoRows, the cascade is meaningless, rollback
// (the runner does it on any fn error) + ack.
var errNoLocalUser = errors.New("no local user row")
