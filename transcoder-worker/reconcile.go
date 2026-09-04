// reconcile.go - R2 orphan reconciliation sweeper
// (issue #44, phase-10).
//
// DeleteUserData (phase 8) is a classic dual-write: the DB
// transaction commits first (all the user's videos rows
// hard-deleted via DeleteVideosByUser), and only afterwards
// does the handler enqueue cleanup:objects to Redis. If the
// process crashes or Redis blips between the two, the DB no
// longer has any row pointing at the R2 keys - the objects
// under uploads/<userID>/, thumbs/<userID>/, hls/<userID>/
// are permanent orphans that only storage bills will ever
// find.
//
// This sweeper closes that gap. It does NOT touch the
// existing row-driven sweeps:
//   - HandleCleanupVideo + ListVideosEligibleForCleanup
//     handle videos soft-deleted < 24h (rows still exist);
//   - reconciliation is for the DELETE-ACCOUNT case where
//     the rows are already GONE and R2 is the only
//     remaining witness.
//
// Tick flow (one reconcile:tick task):
//  1. ListUsersEligibleForReconcile: tombstoned users
//     (is_active=false AND deleted_at set) whose grace
//     period (>24h) has fully elapsed. Active users are
//     never returned by the query - the sweeper cannot
//     touch them even if it wanted to.
//  2. For each user (batch-capped, default 100), list the
//     three R2 prefixes. Any object found there is, by
//     definition, an orphan: the user is past grace, so
//     the post-commit enqueue had every chance to run.
//  3. Dry-run mode (RECONCILE_DRY_RUN=true): log the
//     candidates and stop. Otherwise enqueue ONE
//     cleanup:objects task per user with every listed key
//     (the existing, idempotent handler deletes them).
//  3. Per-tick hit/miss counts go to the log for
//     auditability (issue Safety criteria).
//
// Safety (issue Safety criteria):
//   - only the three whitelisted prefixes are ever listed
//     (uploads/, thumbs/, hls/ + "/<userID>/"); the helper
//     itself additionally refuses empty / slash-only /
//     unterminated prefixes (bucket-wipe impossible);
//   - corrupt rows (tombstone flag without deleted_at, or
//     vice versa) are excluded by the query and left to
//     admin review (issue Out of Scope);
//   - idempotent: a second tick against a user whose
//     cleanup already ran lists zero keys and enqueues
//     nothing.
//
// Cadence: the worker's main goroutine starts a ticker
// (RECONCILE_INTERVAL, default 1h) that enqueues a
// reconcile:tick task; `make reconcile-once` enqueues the
// same task manually.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// reconcilePrefixes are the ONLY R2 roots the sweeper will
// ever look at (issue Safety criteria: refuse to operate
// outside these). Each is composed with "/<userID>/" so a
// user's subtree cannot escape its own prefix.
var reconcilePrefixes = [3]string{"uploads", "thumbs", "hls"}

// HandleReconcileTick is the asynq handler for
// shared.TypeReconcileTick. See the package comment for
// the full flow; this function is intentionally linear so
// the audit trail in the log reads top-to-bottom.
func (w *Worker) HandleReconcileTick(ctx context.Context, t *asynq.Task) error {
	if w.Queries == nil || w.R2 == nil || w.Asynq == nil || w.Logger == nil {
		// Defence in depth per mokibox-go-shared: a nil
		// dep would panic mid-sweep. Log is our only
		// output channel when Logger itself is nil, so
		// guard it separately.
		if w.Logger != nil {
			w.Logger.Error("HandleReconcileTick: nil dependency, skipping")
		}
		return nil
	}

	var payload shared.ReconcileTickPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		w.Logger.Error("HandleReconcileTick: unmarshal", "err", err)
		return nil // malformed payload - permanent
	}
	if payload.BatchSize <= 0 {
		// Producer contract says BatchSize > 0; treat a
		// zero as "use the configured default" rather
		// than sweeping unbounded.
		payload.BatchSize = w.Cfg.ReconcileBatch
	}

	// The tick itself is bounded: a huge backlog must not
	// pin the worker's single processing slot for hours.
	// 10 minutes covers 100 users x 3 list calls with
	// generous R2 latency headroom.
	tickCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	userIDs, err := w.Queries.ListUsersEligibleForReconcile(tickCtx, int32(payload.BatchSize))
	if err != nil {
		w.Logger.Error("HandleReconcileTick: list eligible users", "err", err)
		return err // transient (DB blip) - let asynq retry
	}

	orphansFound := 0
	usersWithOrphans := 0
	usersChecked := 0

	for _, uid := range userIDs {
		select {
		case <-tickCtx.Done():
			// Out of tick budget: log partial progress and
			// return nil - the next tick resumes from the
			// oldest deleted_at (the query sorts ASC).
			w.Logger.Info("HandleReconcileTick: tick budget elapsed, finishing early",
				"users_checked", usersChecked, "users_with_orphans", usersWithOrphans)
			return nil
		default:
		}

		keys, err := w.listUserOrphanKeys(tickCtx, uid)
		if err != nil {
			// One user's listing failure must not abort
			// the whole tick; the next tick retries them
			// (eventual consistency per issue notes).
			w.Logger.Warn("HandleReconcileTick: list user prefixes failed, will retry next tick",
				"err", err, "user_id", uid)
			continue
		}
		usersChecked++

		if len(keys) == 0 {
			continue
		}
		usersWithOrphans++
		orphansFound += len(keys)

		if payload.DryRun {
			w.Logger.Info("HandleReconcileTick: DRY RUN - orphan candidate",
				"user_id", uid, "keys_count", len(keys), "keys_sample", sampleKeys(keys, 5))
			continue
		}

		if _, err := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{Keys: keys}); err != nil {
			// Enqueue failure is logged and left to the
			// next tick (idempotency by design).
			w.Logger.Warn("HandleReconcileTick: enqueue cleanup failed, will retry next tick",
				"err", err, "user_id", uid, "keys_count", len(keys))
			continue
		}
		w.Logger.Info("HandleReconcileTick: orphans scheduled for cleanup",
			"user_id", uid, "keys_count", len(keys), "keys_sample", sampleKeys(keys, 5))
	}

	w.Logger.Info("HandleReconcileTick: tick complete",
		"users_eligible", len(userIDs),
		"users_checked", usersChecked,
		"users_with_orphans", usersWithOrphans,
		"orphan_keys_found", orphansFound,
		"dry_run", payload.DryRun,
	)
	return nil
}

// listUserOrphanKeys lists every object the given
// (already tombstoned, past-grace) user still holds under
// the three whitelisted prefixes. Sequential per user per
// issue Technical Notes (v1: avoid R2 rate limits).
func (w *Worker) listUserOrphanKeys(ctx context.Context, userID uuid.UUID) ([]string, error) {
	var keys []string
	for _, root := range reconcilePrefixes {
		prefix := fmt.Sprintf("%s/%s/", root, userID)
		found, err := w.R2.ListObjectsByPrefix(ctx, prefix, 0)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, err)
		}
		keys = append(keys, found...)
	}
	return keys, nil
}

// sampleKeys returns up to n keys for log lines so a huge
// orphan set does not flood structured logs.
func sampleKeys(keys []string, n int) []string {
	if len(keys) <= n {
		return keys
	}
	return keys[:n]
}

// runReconcileTicker is the periodic driver. It enqueues a
// reconcile:tick task on every interval; the asynq server
// (a separate processing slot) runs the sweep so the
// ticker never blocks the worker loop. Stops when ctx is
// cancelled (worker shutdown).
func (w *Worker) runReconcileTicker(ctx context.Context) {
	interval := w.Cfg.ReconcileInterval
	if interval <= 0 {
		interval = time.Hour
	}
	w.Logger.Info("reconcile ticker started",
		"interval", interval.String(),
		"batch", w.Cfg.ReconcileBatch,
		"dry_run", w.Cfg.ReconcileDryRun,
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Logger.Info("reconcile ticker stopped")
			return
		case <-ticker.C:
			payload := shared.ReconcileTickPayload{
				BatchSize: w.Cfg.ReconcileBatch,
				DryRun:    w.Cfg.ReconcileDryRun,
			}
			if _, err := shared.EnqueueReconcileTick(w.Asynq, payload); err != nil {
				w.Logger.Warn("reconcile ticker: enqueue failed", "err", err)
				continue
			}
		}
	}
}
