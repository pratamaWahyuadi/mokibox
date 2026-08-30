// cleanup.go - HandleCleanupObjects + HandleCleanupVideo
// (NFR-13, FR-VIDEO-09, FR-VIDEO-11).
//
// Both handlers are the worker's "garbage collector".
// They never block the transcode pipeline, never touch
// Postgres transactions, and never receive an HTTP
// request. The signature matches asynq.HandlerFunc:
// ctx + *asynq.Task, returning error.
//
// HandleCleanupObjects deletes a known batch of R2
// keys. Used by:
//   - /confirm when an upload is the wrong size
//     (api-gateway enqueues the raw key)
//   - HandleTranscode after MarkVideoReady succeeds
//     (the raw upload is no longer needed)
//
// HandleCleanupVideo is the 24-hour tombstone job. A
// user-driven delete (or account-delete cascade) flips
// status -> DELETED and deleted_at -> NOW(). The
// api-gateway enqueues cleanup:video with
// ProcessIn(24h). When the worker eventually picks it
// up it must:
//   - delete r2_key, hls_prefix/*, thumbnail_key
//   - DeleteVideoRow to hard-delete the row
//
// The 24h grace lets a deleted video's HLS stay
// cached in case the user toggles the delete off
// (e.g. mobile unsend) before the row is removed.
// Once the row is gone there is no undo.
//
// Cleanup is idempotent. DeleteObjects and DeletePrefix
// already swallow NotFound, and DeleteVideoRow on a
// missing id is a no-op (the row was already cleaned).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// Cleanup grace period. PRD NFR-13 + LLD section 8
// and section 11: objects are kept up to 24 hours
// after DELETED so a quick undelete is possible.
//
// HandleCleanupVideo re-enqueues the same task with
// the remaining time when the row is too fresh. The
// constant lives in the worker package because it is
// only used here; the API contract still says "up to
// 24 hours" without naming a constant.
const cleanupGrace = 24 * time.Hour

// HandleCleanupObjects unmarshals
// CleanupObjectsPayload{Keys} and deletes each key.
// Missing keys are silently skipped by R2Client.DeleteObjects
// (idempotent). Any other R2 error is returned so asynq
// can retry.
func (w *Worker) HandleCleanupObjects(ctx context.Context, t *asynq.Task) error {
	if w.R2 == nil || w.Logger == nil {
		// Defence in depth per mokibox-go-shared -
		// a nil dep would panic mid-handler.
		return nil
	}
	var payload shared.CleanupObjectsPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		w.Logger.Error("HandleCleanupObjects: unmarshal", "err", err)
		return nil // malformed payload - permanent
	}
	if len(payload.Keys) == 0 {
		// No-op. Returning nil (not an error) keeps
		// asynq from retrying an empty payload.
		return nil
	}
	w.Logger.Info("HandleCleanupObjects: start", "keys_count", len(payload.Keys))
	if err := w.R2.DeleteObjects(ctx, payload.Keys); err != nil {
		w.Logger.Error("HandleCleanupObjects: delete", "err", err, "keys_count", len(payload.Keys))
		return err // transient - let asynq retry
	}
	w.Logger.Info("HandleCleanupObjects: success", "keys_count", len(payload.Keys))
	return nil
}

// HandleCleanupVideo finalises a video marked DELETED
// more than 24 hours ago. The flow:
//
//  1. Load the row. Missing -> skip (already cleaned
//     by a previous run).
//  2. status != DELETED -> skip. The row was either
//     resurrected (admin action, future feature) or
//     never deleted; in both cases the api-gateway
//     is the source of truth and the worker should
//     not touch the row.
//  3. deleted_at < now - 24h? -> still in grace
//     period. Re-enqueue the same task with
//     ProcessIn(elapsed) so the job retries when
//     the grace elapses without flooding Redis
//     with polling.
//  4. Past grace: collect keys, delete them, hard
//     delete the row.
//
// "Keys to delete" is composed from three sources:
//
//   - video.R2Key: the raw upload (still in R2
//     unless a prior cleanup:objects already removed
//     it).
//   - video.HlsPrefix + "/": every HLS segment +
//     master + per-variant index. Implemented via
//     R2Client.DeletePrefix so the worker does not
//     have to know the exact filenames ffmpeg
//     produced (segment_0000.ts, segment_0001.ts,
//     ...). The shared.R2Client.DeletePrefix method
//     is the only R2 helper that needs ListObjectsV2.
//   - video.ThumbnailKey: the JPEG.
func (w *Worker) HandleCleanupVideo(ctx context.Context, t *asynq.Task) error {
	if w.Queries == nil || w.R2 == nil || w.Asynq == nil || w.Logger == nil {
		return nil
	}

	var payload shared.CleanupVideoPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		w.Logger.Error("HandleCleanupVideo: unmarshal", "err", err)
		return nil
	}
	videoID, err := uuid.Parse(payload.VideoID)
	if err != nil {
		w.Logger.Error("HandleCleanupVideo: parse video id", "err", err, "video_id", payload.VideoID)
		return nil
	}
	w.Logger.Info("HandleCleanupVideo: start", "video_id", videoID)

	video, err := w.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows) {
			w.Logger.Info("HandleCleanupVideo: row gone, skip", "video_id", videoID)
			return nil
		}
		w.Logger.Error("HandleCleanupVideo: load video", "err", err, "video_id", videoID)
		return nil
	}
	if video.Status != "DELETED" {
		w.Logger.Info("HandleCleanupVideo: not DELETED, skip",
			"video_id", videoID, "status", video.Status)
		return nil
	}
	if !video.DeletedAt.Valid {
		// chk_videos_deleted_at guarantees deleted_at
		// is set when status='DELETED', so this is a
		// defence-in-depth branch. Skip rather than
		// fail - the row is inconsistent but we
		// cannot self-heal it.
		w.Logger.Warn("HandleCleanupVideo: DELETED row missing deleted_at, skip",
			"video_id", videoID)
		return nil
	}

	elapsed := time.Since(video.DeletedAt.Time)
	if elapsed < cleanupGrace {
		remaining := cleanupGrace - elapsed
		w.Logger.Info("HandleCleanupVideo: grace period not yet elapsed, re-enqueue",
			"video_id", videoID, "remaining", remaining.String())
		task, terr := shared.NewCleanupVideoTask(shared.CleanupVideoPayload{VideoID: videoID.String()})
		if terr != nil {
			w.Logger.Error("HandleCleanupVideo: build re-enqueue task", "err", terr, "video_id", videoID)
			return nil
		}
		if _, eerr := shared.EnqueueWithDelay(w.Asynq, task, remaining); eerr != nil {
			w.Logger.Error("HandleCleanupVideo: enqueue re-enqueue", "err", eerr, "video_id", videoID)
			return nil
		}
		return nil
	}

	// Past grace: collect + delete.
	keys := []string{video.R2Key}
	if video.HlsPrefix.Valid && video.HlsPrefix.String != "" {
		// DeletePrefix requires a trailing slash so
		// we do not accidentally match sibling
		// prefixes. HlsPrefix is stored without the
		// trailing slash (e.g. "hls/<uid>/<vid>") so
		// we append "/" here.
		if err := w.R2.DeletePrefix(ctx, video.HlsPrefix.String+"/"); err != nil {
			w.Logger.Error("HandleCleanupVideo: delete hls prefix", "err", err,
				"hls_prefix", video.HlsPrefix.String)
			return err
		}
	}
	if video.ThumbnailKey.Valid && video.ThumbnailKey.String != "" {
		keys = append(keys, video.ThumbnailKey.String)
	}
	if err := w.R2.DeleteObjects(ctx, keys); err != nil {
		w.Logger.Error("HandleCleanupVideo: delete raw + thumb", "err", err,
			"keys_count", len(keys))
		return err
	}

	if err := w.Queries.DeleteVideoRow(ctx, videoID); err != nil {
		w.Logger.Error("HandleCleanupVideo: hard delete row", "err", err, "video_id", videoID)
		return err
	}

	w.Logger.Info("HandleCleanupVideo: success", "video_id", videoID,
		"elapsed_since_deleted", elapsed.String())
	return nil
}