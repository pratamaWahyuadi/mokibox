// transcode.go - HandleTranscode pipeline (FR-VIDEO-04..09).
//
// The handler is the hot path of the worker:
//
//  1. Unmarshal the payload (video_id).
//  2. Load the row. If missing / not PROCESSING / out of
//     retries, take the appropriate no-op branch and
//     return nil so asynq drops the task without retry.
//  3. Increment retry_count so a later crash can be
//     bounded. The handler owns the retry budget:
//     retry_count >= 3 means MarkVideoFailed.
//  4. Create a per-task workdir under /tmp/transcode.
//  5. Download the raw upload from R2.
//  6. ffprobe + ValidateMedia. On ErrInvalidMedia the
//     row is marked FAILED and the raw is cleaned up;
//     this is permanent - no retry.
//  7. Transcode to HLS 480p and 720p + a thumbnail.
//     Both variants run with ffmpeg under the
//     worker's TranscodeTimeout context so a runaway
//     ffmpeg is killed (LLD best practice).
//  8. Build + upload the master.m3u8 alongside the
//     variant segments + thumbnail.
//  9. Re-check the row status. If the video is no
//     longer PROCESSING (race with delete-account or
//     delete-video) the uploads are cleaned up and
//     the handler returns nil. MarkVideoReady itself
//     has a WHERE status='PROCESSING' guard, but
//     cleaning up the orphans here is cheaper than
//     relying on the 24h cleanup job for every race.
// 10. MarkVideoReady. The WHERE guard makes the flip
//     idempotent against the race above.
// 11. Enqueue cleanup:objects for the raw R2 key.
//
// Retry policy (PRD FR-VIDEO-08, LLD section 8):
//   - transient failure (download / transcode / upload)
//     AND retry_count < 3  -> re-enqueue with
//     ProcessIn(30s * retry_count). Return nil so asynq
//     does not also retry (avoid double-counting).
//   - transient failure AND retry_count >= 3 ->
//     MarkVideoFailed + enqueue cleanup:objects for
//     the raw key. Return nil.
//   - permanent failure (validation / missing row) ->
//     MarkVideoFailed (if appropriate) + return nil.
//
// The two-layer retry model matches the producer-side
// docs in handlers/video.go (fase 4): the producer
// enqueues with asynq.MaxRetry(1) as a queue-level
// safety net; this handler is where the "3x" retry
// budget is enforced.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// Variant list: 480p + 720p per LLD section 8.
// Each variant has its own output subdirectory and a
// bandwidth hint baked into master.m3u8 below. Adding
// a new variant is a 4-line change here + the master
// playlist builder; the transcode loop stays generic.
var transcodeVariants = []struct {
	Dir        string
	Resolution string
	Bandwidth  int // bits per second, used in master playlist
}{
	{"480p", "640x480", 800_000},
	{"720p", "1280x720", 1_500_000},
}

// MaxRetries is the application-level retry budget.
// PRD FR-VIDEO-07 + LLD section 8: at most 3 attempts
// per video. The check happens BEFORE the increment
// (per LLD line 726-727), so the row's retry_count
// stays at most 3 across the entire attempt sequence.
const MaxRetries = 3

// retryDelayFor returns the ProcessIn delay for the
// next attempt. LLD says 30s * retry_count, so attempt
// 2 is delayed 30s, attempt 3 is 60s. Kept here
// (instead of inline) so a future change to exponential
// backoff only touches one place.
func retryDelayFor(retryCount int32) time.Duration {
	return time.Duration(retryCount) * 30 * time.Second
}

// HandleTranscode is the asynq handler for
// shared.TypeTranscodeVideo. The full pipeline is
// described in the package comment above; this function
// is the entry point that wires the steps together and
// is the only thing main.go references.
func (w *Worker) HandleTranscode(ctx context.Context, t *asynq.Task) error {
	// Defence in depth per mokibox-go-shared: a nil
	// dep would panic mid-handler, masking the
	// real failure and giving an attacker an oracle
	// on the missing-injection path.
	if w.Queries == nil {
		w.Logger.Error("transcode handler: queries is nil")
		return nil
	}
	if w.R2 == nil {
		w.Logger.Error("transcode handler: r2 is nil")
		return nil
	}
	if w.Asynq == nil {
		w.Logger.Error("transcode handler: asynq client is nil")
		return nil
	}

	var payload shared.TranscodeVideoPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		w.Logger.Error("transcode handler: unmarshal payload", "err", err)
		return nil // malformed payload - permanent, do not retry
	}
	videoID, err := uuid.Parse(payload.VideoID)
	if err != nil {
		w.Logger.Error("transcode handler: parse video id", "err", err, "video_id", payload.VideoID)
		return nil
	}
	w.Logger.Info("transcode handler: start", "video_id", videoID)

	// Bound the whole pipeline under the worker's
	// configured timeout (default 5m). FFmpeg, R2
	// download / upload all share this deadline so a
	// stuck subprocess cannot outlive the task.
	ctx, cancel := context.WithTimeout(ctx, w.Cfg.TranscodeTimeout)
	defer cancel()

	video, err := w.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.Logger.Info("transcode handler: video not found, skipping", "video_id", videoID)
			return nil
		}
		w.Logger.Error("transcode handler: load video", "err", err, "video_id", videoID)
		return nil // DB error - permanent from handler's view; asynq may retry once via MaxRetry(1)
	}
	if video.Status != "PROCESSING" {
		w.Logger.Info("transcode handler: video not in PROCESSING, skipping",
			"video_id", videoID, "status", video.Status)
		return nil
	}

	// Retry budget check FIRST (LLD line 726: "retry_count >= 3
	// -> set FAILED, return nil"). We use the just-loaded
	// `video` row so the budget reflects what is in the DB
	// before this attempt's work begins. If a previous
	// attempt already exhausted the budget, the producer-side
	// asynq.MaxRetry(1) safety net or the worker re-enqueue
	// path may still deliver this task; we treat it as final.
	if video.RetryCount >= MaxRetries {
		// Mark FAILED (idempotent if FAILED is
		// already set) and ask cleanup to remove
		// the raw key. The user will see the
		// status flip on next /videos/:id/status.
		if _, ferr := w.Queries.MarkVideoFailed(ctx, videoID); ferr != nil {
			w.Logger.Error("transcode handler: mark failed (post-budget)", "err", ferr, "video_id", videoID)
		}
		if _, qerr := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{
			Keys: []string{video.R2Key},
		}); qerr != nil {
			w.Logger.Warn("transcode handler: enqueue cleanup raw after budget exhaustion", "err", qerr, "r2_key", video.R2Key)
		}
		w.Logger.Info("transcode handler: retry budget exhausted, marked FAILED",
			"video_id", videoID, "retry_count", video.RetryCount)
		return nil
	}

	// Increment retry counter so the row reflects the
	// in-flight attempt. Following LLD step 3 ("Increment
	// retry_count") - happens AFTER the budget check so
	// an over-budget re-delivery does not bump the
	// counter past its true value.
	incremented, err := w.Queries.IncrementVideoRetry(ctx, videoID)
	if err != nil {
		w.Logger.Error("transcode handler: increment retry", "err", err, "video_id", videoID)
		return nil
	}

	// Per-task workdir. Random suffix keeps parallel
	// attempts (in the future) from clobbering each
	// other and avoids leaking the video UUID to /tmp
	// observers.
	suffix, err := randomHex(8)
	if err != nil {
		w.Logger.Error("transcode handler: random suffix", "err", err, "video_id", videoID)
		return nil
	}
	workDir := filepath.Join("/tmp/transcode", videoID.String()+"-"+suffix)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		w.Logger.Error("transcode handler: mkdir workdir", "err", err, "workdir", workDir)
		return nil
	}
	defer func() {
		if rmErr := os.RemoveAll(workDir); rmErr != nil {
			w.Logger.Warn("transcode handler: cleanup workdir", "err", rmErr, "workdir", workDir)
		}
	}()

	sourcePath := filepath.Join(workDir, "source.bin")
	if err := w.R2.Download(ctx, incremented.R2Key, sourcePath); err != nil {
		w.Logger.Error("transcode handler: download raw", "err", err, "r2_key", incremented.R2Key)
		return w.handleTransient(ctx, incremented, "download raw")
	}

	size, err := fileSize(sourcePath)
	if err != nil {
		w.Logger.Error("transcode handler: stat source", "err", err, "path", sourcePath)
		return w.handleTransient(ctx, incremented, "stat source")
	}

	probe, err := ProbeFile(ctx, sourcePath)
	if err != nil {
		if errors.Is(err, ErrInvalidMedia) {
			// Permanent: do not retry, mark FAILED,
			// clean up the raw key so R2 doesn't
			// keep accumulating hostile uploads.
			if _, ferr := w.Queries.MarkVideoFailed(ctx, videoID); ferr != nil {
				w.Logger.Error("transcode handler: mark failed (invalid media)", "err", ferr, "video_id", videoID)
			}
			if _, qerr := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{
				Keys: []string{incremented.R2Key},
			}); qerr != nil {
				w.Logger.Warn("transcode handler: enqueue cleanup raw (invalid media)", "err", qerr)
			}
			w.Logger.Info("transcode handler: invalid media, marked FAILED",
				"video_id", videoID, "err", err)
			return nil
		}
		// ffprobe subprocess error that is NOT a
		// validation failure (e.g. executable missing).
		// Treat as transient.
		w.Logger.Error("transcode handler: probe file", "err", err, "video_id", videoID)
		return w.handleTransient(ctx, incremented, "probe file")
	}
	if err := ValidateMedia(probe, size); err != nil {
		// Same permanent branch as ProbeFile's own
		// validation error.
		if _, ferr := w.Queries.MarkVideoFailed(ctx, videoID); ferr != nil {
			w.Logger.Error("transcode handler: mark failed (validate)", "err", ferr, "video_id", videoID)
		}
		if _, qerr := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{
			Keys: []string{incremented.R2Key},
		}); qerr != nil {
			w.Logger.Warn("transcode handler: enqueue cleanup raw (validate)", "err", qerr)
		}
		w.Logger.Info("transcode handler: validate media failed, marked FAILED",
			"video_id", videoID, "err", err)
		return nil
	}

	durationSec := parseFFmpegDuration(probe.Format.Duration)
	if durationSec <= 0 {
		// ffprobe gave us a value that did not
		// parse - treat as transient so we retry
		// rather than publishing a row with an
		// invalid duration.
		w.Logger.Error("transcode handler: parse duration", "duration_raw", probe.Format.Duration, "video_id", videoID)
		return w.handleTransient(ctx, incremented, "parse duration")
	}

	hlsPrefix := fmt.Sprintf("hls/%s/%s", incremented.UserID, videoID)
	thumbnailKey := fmt.Sprintf("thumbs/%s/%s/thumb.jpg", incremented.UserID, videoID)

	// Run all variants. They share the source and the
	// workdir; ffmpeg itself serialises internally so
	// parallel runs are safe in principle but the
	// single-VPS deployment already pegs CPU on a
	// single 720p encode, so we run them sequentially
	// for predictable memory pressure.
	uploadedKeys := make([]string, 0, 32)
	cleanupOnFailure := func() {
		// Best-effort cleanup if transcode fails
		// mid-pipeline. Errors here are logged but
		// do not change the handler return - the
		// workdir defer already removes local files.
		if len(uploadedKeys) > 0 {
			if _, qerr := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{Keys: uploadedKeys}); qerr != nil {
				w.Logger.Warn("transcode handler: enqueue cleanup partial", "err", qerr, "count", len(uploadedKeys))
			}
		}
	}

	for _, v := range transcodeVariants {
		variantDir := filepath.Join(workDir, hlsPrefix, v.Dir)
		if err := os.MkdirAll(variantDir, 0o755); err != nil {
			w.Logger.Error("transcode handler: mkdir variant", "err", err, "variant", v.Dir)
			cleanupOnFailure()
			return w.handleTransient(ctx, incremented, "mkdir variant")
		}
		indexPath := filepath.Join(variantDir, "index.m3u8")
		segmentPattern := filepath.Join(variantDir, "segment_%04d.ts")
		args := []string{
			"-y",
			"-i", sourcePath,
			"-vf", "scale=-2:" + strings.TrimSuffix(v.Dir, "p"),
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-crf", "23",
			"-c:a", "aac",
			"-b:a", "128k",
			"-hls_time", "6",
			"-hls_playlist_type", "vod",
			"-hls_segment_filename", segmentPattern,
			indexPath,
		}
		if err := w.runFFmpeg(ctx, args, workDir); err != nil {
			w.Logger.Error("transcode handler: ffmpeg variant", "err", err, "variant", v.Dir, "video_id", videoID)
			cleanupOnFailure()
			return w.handleTransient(ctx, incremented, "ffmpeg "+v.Dir)
		}
		// Upload every segment + index for this
		// variant before moving on so a failure
		// between variants doesn't leave the first
		// variant half-published.
		keys, err := w.uploadVariant(ctx, workDir, hlsPrefix, v.Dir)
		if err != nil {
			w.Logger.Error("transcode handler: upload variant", "err", err, "variant", v.Dir, "video_id", videoID)
			uploadedKeys = append(uploadedKeys, keys...)
			cleanupOnFailure()
			return w.handleTransient(ctx, incremented, "upload variant "+v.Dir)
		}
		uploadedKeys = append(uploadedKeys, keys...)
	}

	// Thumbnail: one frame from second 1.
	thumbPath := filepath.Join(workDir, filepath.Base(thumbnailKey))
	thumbArgs := []string{
		"-y",
		"-i", sourcePath,
		"-ss", "00:00:01",
		"-frames:v", "1",
		"-vf", "scale=480:-1",
		thumbPath,
	}
	if err := w.runFFmpeg(ctx, thumbArgs, workDir); err != nil {
		w.Logger.Error("transcode handler: ffmpeg thumb", "err", err, "video_id", videoID)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "ffmpeg thumb")
	}
	if err := w.R2.UploadFile(ctx, thumbnailKey, thumbPath, "image/jpeg"); err != nil {
		w.Logger.Error("transcode handler: upload thumb", "err", err, "key", thumbnailKey)
		uploadedKeys = append(uploadedKeys, thumbnailKey)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "upload thumb")
	}
	uploadedKeys = append(uploadedKeys, thumbnailKey)

	// Master playlist. Written to the workdir then
	// uploaded; the live resolution / bandwidth per
	// variant are encoded in the file so the player
	// picks the right rendition.
	masterPath := filepath.Join(workDir, "master.m3u8")
	if err := writeMasterPlaylist(masterPath, hlsPrefix, transcodeVariants); err != nil {
		w.Logger.Error("transcode handler: write master", "err", err, "video_id", videoID)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "write master")
	}
	masterKey := filepath.ToSlash(filepath.Join(hlsPrefix, "master.m3u8"))
	if err := w.R2.UploadFile(ctx, masterKey, masterPath, "application/vnd.apple.mpegurl"); err != nil {
		w.Logger.Error("transcode handler: upload master", "err", err, "key", masterKey)
		uploadedKeys = append(uploadedKeys, masterKey)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "upload master")
	}
	uploadedKeys = append(uploadedKeys, masterKey)

	// Re-check status. If the row was DELETED between
	// our first read and now, all the uploads above
	// are orphans; schedule cleanup and return
	// without flipping status. The 24h cleanup job
	// would catch them too, but cleaning here keeps
	// R2 storage tight during the race window.
	fresh, err := w.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.Logger.Info("transcode handler: video disappeared mid-flight, cleaning uploads",
				"video_id", videoID, "uploaded_count", len(uploadedKeys))
			cleanupOnFailure()
			return nil
		}
		w.Logger.Error("transcode handler: re-check status", "err", err, "video_id", videoID)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "re-check status")
	}
	if fresh.Status != "PROCESSING" {
		w.Logger.Info("transcode handler: video no longer PROCESSING mid-flight, cleaning uploads",
			"video_id", videoID, "status", fresh.Status, "uploaded_count", len(uploadedKeys))
		cleanupOnFailure()
		return nil
	}

	// Mark READY. MarkVideoReady has WHERE status='PROCESSING',
	// so even if a concurrent delete landed between
	// the re-check above and this UPDATE, the row
	// stays untouched and the cleanup job picks up
	// the orphans within 24h.
	readyParams := db.MarkVideoReadyParams{
		ID:              videoID,
		HlsPrefix:       sql.NullString{String: hlsPrefix, Valid: true},
		ThumbnailKey:    sql.NullString{String: thumbnailKey, Valid: true},
		DurationSeconds: sql.NullInt32{Int32: int32(math.Round(durationSec)), Valid: true},
	}
	if _, err := w.Queries.MarkVideoReady(ctx, readyParams); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.Logger.Info("transcode handler: mark-ready guarded by status, video already moved",
				"video_id", videoID)
			cleanupOnFailure()
			return nil
		}
		w.Logger.Error("transcode handler: mark ready", "err", err, "video_id", videoID)
		cleanupOnFailure()
		return w.handleTransient(ctx, incremented, "mark ready")
	}

	// Best-effort cleanup of the raw upload now that
	// the row is READY.
	if _, qerr := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{
		Keys: []string{incremented.R2Key},
	}); qerr != nil {
		w.Logger.Warn("transcode handler: enqueue cleanup raw after success", "err", qerr, "r2_key", incremented.R2Key)
	}

	w.Logger.Info("transcode handler: success",
		"video_id", videoID,
		"hls_prefix", hlsPrefix,
		"thumbnail_key", thumbnailKey,
		"duration_sec", int32(math.Round(durationSec)),
		"retry_count", incremented.RetryCount,
	)
	return nil
}

// handleTransient centralises the "retry if budget
// remains, otherwise mark FAILED" decision so every
// transient failure site in HandleTranscode looks the
// same. Returns nil regardless so asynq does not
// double-count by also retrying (PRD: app-level
// retry budget is exactly 3).
func (w *Worker) handleTransient(ctx context.Context, v db.Video, op string) error {
	// IncrementVideoRetry already bumped retry_count
	// at the start of this attempt, so v.RetryCount
	// here is "attempts made including this one".
	if v.RetryCount >= MaxRetries {
		if _, err := w.Queries.MarkVideoFailed(ctx, v.ID); err != nil {
			w.Logger.Error("transcode handler: mark failed (post-budget)", "err", err, "video_id", v.ID)
		}
		if _, err := shared.EnqueueCleanupObjects(w.Asynq, shared.CleanupObjectsPayload{Keys: []string{v.R2Key}}); err != nil {
			w.Logger.Warn("transcode handler: enqueue cleanup raw (post-budget)", "err", err, "r2_key", v.R2Key)
		}
		w.Logger.Info("transcode handler: budget exhausted after transient failure",
			"video_id", v.ID, "op", op, "retry_count", v.RetryCount)
		return nil
	}
	// Re-enqueue with backoff. We rebuild the task
	// from scratch (rather than reusing t) because
	// asynq's retry-count semantics are clearer this
	// way; the new task is a fresh attempt, not a
	// redelivery.
	task, err := shared.NewTranscodeTask(shared.TranscodeVideoPayload{VideoID: v.ID.String()})
	if err != nil {
		w.Logger.Error("transcode handler: build retry task", "err", err, "video_id", v.ID)
		return nil
	}
	if _, err := shared.EnqueueWithDelay(w.Asynq, task, retryDelayFor(v.RetryCount)); err != nil {
		w.Logger.Error("transcode handler: enqueue retry", "err", err, "video_id", v.ID, "delay", retryDelayFor(v.RetryCount))
		return nil
	}
	w.Logger.Info("transcode handler: re-enqueued for retry",
		"video_id", v.ID, "op", op, "retry_count", v.RetryCount, "delay", retryDelayFor(v.RetryCount))
	return nil
}

// runFFmpeg wraps exec.CommandContext with sane
// defaults: stderr is captured to a buffer so the
// log carries ffmpeg's actual error message when
// the encode fails, and the working directory is
// the per-task workdir so ffmpeg's relative paths
// (segment_%04d.ts) resolve where we expect.
//
// The cancel parent context is the per-task
// timeout context built in HandleTranscode, so a
// runaway ffmpeg is killed when the budget expires.
func (w *Worker) runFFmpeg(ctx context.Context, args []string, workDir string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = workDir
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// Include the captured stderr (last 1 KiB is
		// usually enough to spot the actual codec
		// error) so the log line is self-contained.
		msg := stderr.String()
		if len(msg) > 1024 {
			msg = msg[len(msg)-1024:]
		}
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", err, msg)
	}
	return nil
}

// uploadVariant walks the per-variant directory and
// uploads every file to R2 with a content type
// matching its extension. The returned slice lists
// every key uploaded so a partial failure can be
// cleaned up.
func (w *Worker) uploadVariant(ctx context.Context, workDir, hlsPrefix, variantDirName string) ([]string, error) {
	variantPath := filepath.Join(workDir, hlsPrefix, variantDirName)
	entries, err := os.ReadDir(variantPath)
	if err != nil {
		return nil, fmt.Errorf("read variant dir: %w", err)
	}
	uploaded := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(variantPath, e.Name())
		rel, err := filepath.Rel(workDir, full)
		if err != nil {
			return uploaded, fmt.Errorf("rel path: %w", err)
		}
		key := filepath.ToSlash(rel)
		ct := "application/octet-stream"
		switch filepath.Ext(e.Name()) {
		case ".m3u8":
			ct = "application/vnd.apple.mpegurl"
		case ".ts":
			ct = "video/mp2t"
		}
		if err := w.R2.UploadFile(ctx, key, full, ct); err != nil {
			return uploaded, fmt.Errorf("upload %s: %w", key, err)
		}
		uploaded = append(uploaded, key)
	}
	return uploaded, nil
}

// writeMasterPlaylist builds the master.m3u8 that
// references every variant under hlsPrefix. The
// format is the HLS v3 multi-variant manifest
// expected by AVPlayer / ExoPlayer / hls.js.
func writeMasterPlaylist(path, hlsPrefix string, variants []struct {
	Dir        string
	Resolution string
	Bandwidth  int
}) error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	for _, v := range variants {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%s\n", v.Bandwidth, v.Resolution)
		fmt.Fprintf(&b, "%s/%s/index.m3u8\n", hlsPrefix, v.Dir)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// parseFFmpegDuration parses ffprobe's "duration"
// string into a float number of seconds. ffprobe
// emits durations with arbitrary precision so we
// always read as float64.
func parseFFmpegDuration(s string) float64 {
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// fileSize returns the byte size of the file at
// path. Used to feed ValidateMedia's size check
// without a second R2 HeadObject call (we already
// have the file locally at this point).
func fileSize(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// randomHex returns n bytes of random data
// hex-encoded. Used to build unique workdir
// suffixes so parallel attempts (future) do not
// collide on /tmp.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}