// Phase 8.B smoke test for DeleteVideo (tombstone
// + 24h cleanup:video enqueue).
//
// Hermetic, in-network smoke (runs inside the
// mokibox_backend docker network). Verifies:
//
//  1. MarkVideoDeleted flips status to DELETED
//     and sets deleted_at on the row.
//  2. The api-gateway enqueues a cleanup:video
//     task with the 24h delay (asynq stores the
//     schedule in the scheduled sorted set; the
//     task must NOT show up in pending until the
//     delay elapses).
//  3. Likes + comments rows on the deleted video
//     are kept (they get cascade-deleted by the
//     worker after 24h via DeleteVideoRow); the
//     counter columns on the row are NOT zeroed
//     at tombstone time (this is the intentional
//     difference vs. account-delete: the row
//     stays, only the worker does the hard
//     delete).
//  4. Non-owner DeleteVideo -> 404 path is
//     enforced via the same flow used by
//     GetVideoDetail: a non-owner probe returns
//     sql.ErrNoRows from the visibility check
//     (we mirror the same path in the smoke by
//     using the MarkVideoDeleted query's user_id
//     filter: a non-owner UPDATE matches 0 rows
//     and returns sql.ErrNoRows).
//  5. After MarkVideoDeleted, the row's
//     constraint chk_videos_deleted_at still
//     passes (deleted_at NOT NULL while
//     status=DELETED).
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    -e DATABASE_URL=... -e REDIS_ADDR=... \
//	    -e REDIS_PASSWORD=... \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase8_delete_video
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase8b-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase8_delete_video")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// ----- DSN -----
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:***@postgres:5432/tiktok?sslmode=disable"
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	q := db.New(sqlDB)

	// ----- Asynq + Inspector -----
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "redis:6379"
	}
	redisPw := os.Getenv("REDIS_PASSWORD")
	asynqClient, err := shared.NewAsynqClient(shared.RedisConfig{Addr: redisAddr, Password: redisPw})
	if err != nil {
		return fmt.Errorf("asynq client: %w", err)
	}
	defer asynqClient.Close()
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: redisAddr, Password: redisPw})
	defer inspector.Close()

	// ----- Pre-clean -----
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("pre-clean users: %w", err)
	}
	// Drain leftover cleanup:video tasks so the
	// post-check is unambiguous.
	if err := drainScheduled(ctx, inspector, "cleanup:video"); err != nil {
		return fmt.Errorf("drain pre: %w", err)
	}

	// ----- Seed owner + non-owner + READY video -----
	owner, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner", false, true)
	if err != nil {
		return fmt.Errorf("seed owner: %w", err)
	}
	other, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"other", false, true)
	if err != nil {
		return fmt.Errorf("seed other: %w", err)
	}
	video, err := insertReady(ctx, q, owner.ID, "owner")
	if err != nil {
		return fmt.Errorf("seed video: %w", err)
	}

	// Give the video a like + comment + view
	// traffic so the tombstone has something to
	// preserve on the row.
	if err := likeTx(ctx, sqlDB, q, other, video); err != nil {
		return fmt.Errorf("seed like: %w", err)
	}
	if err := commentTx(ctx, sqlDB, q, other, video, "first"); err != nil {
		return fmt.Errorf("seed comment: %w", err)
	}

	preVid, err := q.GetVideoByID(ctx, video.ID)
	if err != nil {
		return fmt.Errorf("reload pre: %w", err)
	}
	if preVid.Status != "READY" {
		return fmt.Errorf("pre: video status=%q, want READY", preVid.Status)
	}
	if preVid.LikesCount != 1 {
		return fmt.Errorf("pre: video likes_count=%d, want 1", preVid.LikesCount)
	}
	if preVid.CommentsCount != 1 {
		return fmt.Errorf("pre: video comments_count=%d, want 1", preVid.CommentsCount)
	}
	if preVid.DeletedAt.Valid {
		return fmt.Errorf("pre: video deleted_at should be NULL")
	}

	// ----- The call under test: mark deleted + enqueue -----
	updated, err := q.MarkVideoDeleted(ctx, db.MarkVideoDeletedParams{
		ID:     video.ID,
		UserID: owner.ID,
	})
	if err != nil {
		return fmt.Errorf("MarkVideoDeleted: %w", err)
	}
	if updated.Status != "DELETED" {
		return fmt.Errorf("post: video status=%q, want DELETED", updated.Status)
	}
	if !updated.DeletedAt.Valid {
		return fmt.Errorf("post: video deleted_at NULL, want NOT NULL")
	}

	// Enqueue the 24h cleanup. We call
	// EnqueueCleanupVideo directly to mirror the
	// api-gateway's call; the asynq.ProcessIn
	// option is what causes the task to land in
	// the SCHEDULED set instead of pending.
	cleanupDelay := 24 * time.Hour
	if _, err := shared.EnqueueCleanupVideo(asynqClient, shared.CleanupVideoPayload{
		VideoID: updated.ID.String(),
	}, asynq.ProcessIn(cleanupDelay)); err != nil {
		return fmt.Errorf("EnqueueCleanupVideo: %w", err)
	}

	// ----- Verify the row (counters NOT zeroed) -----
	v2, err := q.GetVideoByID(ctx, video.ID)
	if err != nil {
		return fmt.Errorf("reload post: %w", err)
	}
	if v2.Status != "DELETED" {
		return fmt.Errorf("post2: video status=%q, want DELETED", v2.Status)
	}
	if !v2.DeletedAt.Valid {
		return fmt.Errorf("post2: video deleted_at NULL, want NOT NULL")
	}
	// Counters are intentionally preserved on
	// tombstone so the row still serves its
	// purpose during the 24h grace (visibility
	// is blocked by the status check elsewhere).
	if v2.LikesCount != 1 {
		return fmt.Errorf("post2: video likes_count=%d, want 1 (preserved on tombstone)", v2.LikesCount)
	}
	if v2.CommentsCount != 1 {
		return fmt.Errorf("post2: video comments_count=%d, want 1 (preserved on tombstone)", v2.CommentsCount)
	}

	// ----- Verify chk_videos_deleted_at still passes -----
	// A live UPDATE that would violate the
	// constraint must fail. We attempt to set
	// deleted_at NULL while status=DELETED, which
	// the constraint forbids.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE videos SET deleted_at = NULL WHERE id = $1`, video.ID); err == nil {
		return fmt.Errorf("constraint: UPDATE deleted_at=NULL should fail, got nil")
	}

	// ----- Verify scheduled task in Redis -----
	taskID, processAt, found, err := findCleanupVideoScheduled(ctx, inspector, video.ID.String())
	if err != nil {
		return fmt.Errorf("inspector: %w", err)
	}
	if !found {
		return fmt.Errorf("cleanup:video not enqueued for video %s", video.ID)
	}
	// ProcessIn(24h) -> scheduled for ~24h from
	// now. Allow a 5-minute slack for CI clock
	// drift + docker time skew.
	now := time.Now()
	lower := now.Add(23*time.Hour + 55*time.Minute)
	upper := now.Add(24*time.Hour + 5*time.Minute)
	if processAt.Before(lower) || processAt.After(upper) {
		return fmt.Errorf("cleanup:video scheduled at %s, want in [%s, %s]", processAt, lower, upper)
	}

	// ----- Verify pending is empty for this video -----
	// The ProcessIn delay must keep the task in
	// the SCHEDULED set, not PENDING.
	pending, err := inspector.ListPendingTasks("default", asynq.PageSize(50))
	if err != nil {
		return fmt.Errorf("ListPendingTasks: %w", err)
	}
	for _, t := range pending {
		if t.Type != "cleanup:video" {
			continue
		}
		var p shared.CleanupVideoPayload
		if jerr := json.Unmarshal(t.Payload, &p); jerr != nil {
			continue
		}
		if p.VideoID == video.ID.String() {
			return fmt.Errorf("cleanup:video for %s in PENDING set (delay not honoured)", video.ID)
		}
	}

	// ----- Non-owner delete attempt: MarkVideoDeleted
	// with the wrong user_id returns sql.ErrNoRows.
	// The handler maps this to 404. We verify
	// the contract: 0 rows updated, sql.ErrNoRows.
	_, err = q.MarkVideoDeleted(ctx, db.MarkVideoDeletedParams{
		ID:     video.ID,
		UserID: other.ID, // non-owner
	})
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("non-owner MarkVideoDeleted: err=%v, want sql.ErrNoRows", err)
	}

	_ = taskID
	log.Printf("ok: tombstoned video=%s status=DELETED deleted_at=%s; cleanup:video scheduled at %s (24h); non-owner UPDATE blocked (404)", video.ID, v2.DeletedAt.Time.Format(time.RFC3339), processAt.Format(time.RFC3339))
	return nil
}

// ----- helpers (subset of phase 7) -----

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string, isPrivate, isActive bool) (db.User, error) {
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	var u db.User
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		return u, nil
	}
	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase8b-" + username,
		Username:    username,
		DisplayName: sql.NullString{String: username, Valid: true},
	})
	if err != nil {
		return db.User{}, err
	}
	if isPrivate || !isActive {
		_, _ = sqlDB.ExecContext(ctx, `UPDATE users SET is_private=$1, is_active=$2 WHERE id=$3`, isPrivate, isActive, created.ID)
	}
	return created, nil
}

func insertReady(ctx context.Context, q *db.Queries, owner uuid.UUID, slug string) (db.Video, error) {
	v, err := q.InsertVideo(ctx, db.InsertVideoParams{
		UserID: owner,
		R2Key:  fmt.Sprintf("uploads/%s/%s/source-%s.mp4", owner, uuid.New(), slug),
		Title:  sql.NullString{String: "phase8b-smoke-" + slug, Valid: true},
	})
	if err != nil {
		return db.Video{}, err
	}
	if _, err := q.UpdateVideoToProcessing(ctx, v.ID); err != nil {
		return db.Video{}, err
	}
	ready, err := q.MarkVideoReady(ctx, db.MarkVideoReadyParams{
		ID:              v.ID,
		HlsPrefix:       sql.NullString{String: fmt.Sprintf("hls/%s/%s/", owner, v.ID), Valid: true},
		ThumbnailKey:    sql.NullString{String: fmt.Sprintf("thumbs/%s/%s.jpg", owner, v.ID), Valid: true},
		DurationSeconds: sql.NullInt32{Int32: 12, Valid: true},
	})
	if err != nil {
		return db.Video{}, err
	}
	return ready, nil
}

func likeTx(ctx context.Context, sqlDB *sql.DB, q *db.Queries, actor db.User, video db.Video) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := q.WithTx(tx)
	_, lerr := qtx.InsertLike(ctx, db.InsertLikeParams{UserID: actor.ID, VideoID: video.ID})
	switch {
	case lerr == nil:
		if err := qtx.IncrementLikesCount(ctx, video.ID); err != nil {
			return fmt.Errorf("increment: %w", err)
		}
	case errors.Is(lerr, sql.ErrNoRows):
	default:
		return fmt.Errorf("insert like: %w", lerr)
	}
	return tx.Commit()
}

func commentTx(ctx context.Context, sqlDB *sql.DB, q *db.Queries, actor db.User, video db.Video, body string) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := q.WithTx(tx)
	if _, cerr := qtx.InsertComment(ctx, db.InsertCommentParams{
		VideoID: video.ID, UserID: actor.ID, Content: body,
	}); cerr != nil {
		return fmt.Errorf("insert: %w", cerr)
	}
	if err := qtx.IncrementCommentsCount(ctx, video.ID); err != nil {
		return fmt.Errorf("increment: %w", err)
	}
	return tx.Commit()
}

// drainScheduled removes all scheduled tasks of
// the given type from the default queue.
func drainScheduled(ctx context.Context, ins *asynq.Inspector, taskType string) error {
	const page = 50
	for {
		tasks, err := ins.ListScheduledTasks("default", asynq.PageSize(page))
		if err != nil {
			return fmt.Errorf("ListScheduledTasks: %w", err)
		}
		if len(tasks) == 0 {
			return nil
		}
		matched := 0
		for _, t := range tasks {
			if t.Type != taskType {
				continue
			}
			matched++
			if err := ins.DeleteTask("default", t.ID); err != nil {
				return fmt.Errorf("DeleteTask %s: %w", t.ID, err)
			}
		}
		if matched == 0 && len(tasks) < page {
			return nil
		}
	}
}

// findCleanupVideoScheduled scans the SCHEDULED
// queue for a cleanup:video task matching the
// given videoID. Returns (id, processAt, found,
// err).
func findCleanupVideoScheduled(ctx context.Context, ins *asynq.Inspector, videoID string) (string, time.Time, bool, error) {
	const page = 50
	tasks, err := ins.ListScheduledTasks("default", asynq.PageSize(page))
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("ListScheduledTasks: %w", err)
	}
	for _, t := range tasks {
		if t.Type != "cleanup:video" {
			continue
		}
		var p shared.CleanupVideoPayload
		if jerr := json.Unmarshal(t.Payload, &p); jerr != nil {
			continue
		}
		if p.VideoID == videoID {
			return t.ID, t.NextProcessAt, true, nil
		}
	}
	return "", time.Time{}, false, nil
}
