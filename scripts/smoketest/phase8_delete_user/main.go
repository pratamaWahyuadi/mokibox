// Phase 8.A smoke test for DeleteUserData (tombstone
// + relational cleanup + R2 cleanup enqueue).
//
// Hermetic, in-network smoke (runs inside the
// mokibox_backend docker network). Verifies:
//
//  1. Counter corrections: a user A liking B's
//     video and commenting on B's video must
//     decrement B's likes_count and
//     comments_count by exactly 1 when A is
//     tombstoned (A's like/comment rows go away
//     too).
//  2. Relational cleanup: A's videos, A's
//     likes, A's comments, A's follow edges
//     (both directions) all go away.
//  3. Tombstone: A.is_active=false,
//     A.deleted_at set, A.username rewritten to
//     'deleted_<id>', A.display_name/bio/avatar
//     nulled.
//  4. R2 cleanup: after DeleteUserData, a
//     cleanup:objects task is enqueued on the
//     default queue carrying the raw + thumbnail
//     R2 keys of A's video (hls_prefix is left
//     to the worker's DeletePrefix path).
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    -e DATABASE_URL=... -e REDIS_ADDR=... \
//	    -e REDIS_PASSWORD=... \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase8_delete_user
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase8a-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase8_delete_user")
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

	// ----- Asynq client + inspector (same redis) -----
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

	// ----- Pre-clean (idempotent reruns) -----
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("pre-clean users: %w", err)
	}
	// Drain leftover cleanup:objects from a prior
	// run so the post-check is unambiguous.
	if err := drainQueue(ctx, inspector, "cleanup:objects"); err != nil {
		return fmt.Errorf("drain pre: %w", err)
	}

	// ----- Seed A (the user we will tombstone) -----
	userA, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"A", false, true)
	if err != nil {
		return fmt.Errorf("seed A: %w", err)
	}
	// A's own video (will be deleted by DeleteVideosByUser).
	videoA, err := insertReady(ctx, q, userA.ID, "A")
	if err != nil {
		return fmt.Errorf("seed videoA: %w", err)
	}

	// ----- Seed B (third party whose counters we must decrement) -----
	userB, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"B", false, true)
	if err != nil {
		return fmt.Errorf("seed B: %w", err)
	}
	videoB, err := insertReady(ctx, q, userB.ID, "B")
	if err != nil {
		return fmt.Errorf("seed videoB: %w", err)
	}

	// A third user C is needed so B's video has
	// likes_count > 0 BEFORE A likes it, so the
	// decrement is observable.
	userC, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"C", false, true)
	if err != nil {
		return fmt.Errorf("seed C: %w", err)
	}
	if err := likeTx(ctx, sqlDB, q, userC, videoB); err != nil {
		return fmt.Errorf("seed C like B: %w", err)
	}

	// Now A likes + comments on B's video. B's
	// counters go to 2 likes, 1 comment.
	if err := likeTx(ctx, sqlDB, q, userA, videoB); err != nil {
		return fmt.Errorf("A like B: %w", err)
	}
	if err := commentTx(ctx, sqlDB, q, userA, videoB, "nice"); err != nil {
		return fmt.Errorf("A comment B: %w", err)
	}

	// A <-> B follows so both edges need to disappear.
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: userA.ID, FolloweeID: userB.ID}); err != nil {
		return fmt.Errorf("A follow B: %w", err)
	}
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: userB.ID, FolloweeID: userA.ID}); err != nil {
		return fmt.Errorf("B follow A: %w", err)
	}

	// ----- Sanity check pre-state -----
	vBpre, err := q.GetVideoByID(ctx, videoB.ID)
	if err != nil {
		return fmt.Errorf("reload videoB pre: %w", err)
	}
	if vBpre.LikesCount != 2 {
		return fmt.Errorf("pre: videoB.likes_count=%d, want 2", vBpre.LikesCount)
	}
	if vBpre.CommentsCount != 1 {
		return fmt.Errorf("pre: videoB.comments_count=%d, want 1", vBpre.CommentsCount)
	}
	if err := assertTableCount(ctx, sqlDB, "videos", userA.ID, 1); err != nil {
		return fmt.Errorf("pre videos A: %w", err)
	}
	if err := assertTableCount(ctx, sqlDB, "likes", userA.ID, 1); err != nil {
		return fmt.Errorf("pre likes A: %w", err)
	}
	if err := assertTableCount(ctx, sqlDB, "comments", userA.ID, 1); err != nil {
		return fmt.Errorf("pre comments A: %w", err)
	}
	if err := assertTableCount2(ctx, sqlDB, "follows", "follower_id", userA.ID, 1); err != nil {
		return fmt.Errorf("pre follows A (follower): %w", err)
	}
	if err := assertTableCount2(ctx, sqlDB, "follows", "followee_id", userA.ID, 1); err != nil {
		return fmt.Errorf("pre follows A (followee): %w", err)
	}

	// ----- The call under test -----
	if err := handlers.DeleteUserData(ctx, q, sqlDB, asynqClient, userA.ID); err != nil {
		return fmt.Errorf("DeleteUserData: %w", err)
	}

	// ----- Post-state: B's counters decremented -----
	vBpost, err := q.GetVideoByID(ctx, videoB.ID)
	if err != nil {
		return fmt.Errorf("reload videoB post: %w", err)
	}
	// C's like stays (C not touched); A's like
	// row is removed, so DecrementLikesForUser
	// subtracts exactly 1.
	if vBpost.LikesCount != 1 {
		return fmt.Errorf("post: videoB.likes_count=%d, want 1 (A's 1 like removed)", vBpost.LikesCount)
	}
	if vBpost.CommentsCount != 0 {
		return fmt.Errorf("post: videoB.comments_count=%d, want 0 (A's 1 comment removed)", vBpost.CommentsCount)
	}

	// A's videos: gone (DeleteVideosByUser).
	if err := assertTableCount(ctx, sqlDB, "videos", userA.ID, 0); err != nil {
		return fmt.Errorf("post videos A: %w", err)
	}
	// A's likes: gone.
	if err := assertTableCount(ctx, sqlDB, "likes", userA.ID, 0); err != nil {
		return fmt.Errorf("post likes A: %w", err)
	}
	// A's comments: gone.
	if err := assertTableCount(ctx, sqlDB, "comments", userA.ID, 0); err != nil {
		return fmt.Errorf("post comments A: %w", err)
	}
	// A's follow edges (both directions): gone.
	if err := assertTableCount2(ctx, sqlDB, "follows", "follower_id", userA.ID, 0); err != nil {
		return fmt.Errorf("post follows A (follower): %w", err)
	}
	if err := assertTableCount2(ctx, sqlDB, "follows", "followee_id", userA.ID, 0); err != nil {
		return fmt.Errorf("post follows A (followee): %w", err)
	}

	// Notifications referencing A as actor are gone.
	if err := assertTableCount2(ctx, sqlDB, "notifications", "actor_id", userA.ID, 0); err != nil {
		return fmt.Errorf("post notif actor A: %w", err)
	}

	// Tombstone: row still exists with is_active=false,
	// deleted_at NOT NULL, username rewritten.
	tA, err := q.GetUserByID(ctx, userA.ID)
	if err != nil {
		return fmt.Errorf("reload userA: %w", err)
	}
	if tA.IsActive {
		return fmt.Errorf("tombstone: userA.is_active=true, want false")
	}
	if !tA.DeletedAt.Valid {
		return fmt.Errorf("tombstone: userA.deleted_at NULL, want NOT NULL")
	}
	if !strings.HasPrefix(tA.Username, "deleted_") {
		return fmt.Errorf("tombstone: userA.username=%q, want 'deleted_<id>' prefix", tA.Username)
	}
	if tA.Username != "deleted_"+userA.ID.String() {
		return fmt.Errorf("tombstone: userA.username=%q, want exact 'deleted_%s'", tA.Username, userA.ID)
	}
	if tA.DisplayName.Valid {
		return fmt.Errorf("tombstone: userA.display_name should be null, got %q", tA.DisplayName.String)
	}
	if tA.Bio.Valid {
		return fmt.Errorf("tombstone: userA.bio should be null")
	}
	if tA.AvatarUrl.Valid {
		return fmt.Errorf("tombstone: userA.avatar_url should be null")
	}

	// cleanup:objects enqueued. Payload should
	// carry videoA's raw + thumb. Hls prefix is
	// intentionally NOT in the keys (the worker
	// handles it via DeletePrefix).
	keys, found, err := findCleanupObjectsKeys(ctx, inspector)
	if err != nil {
		return fmt.Errorf("inspector: %w", err)
	}
	if !found {
		return fmt.Errorf("cleanup:objects task not enqueued")
	}
	if !containsString(keys, videoA.R2Key) {
		return fmt.Errorf("cleanup:objects missing r2_key %q; got %v", videoA.R2Key, keys)
	}
	wantThumb := ""
	if videoA.ThumbnailKey.Valid {
		wantThumb = videoA.ThumbnailKey.String
	}
	if wantThumb != "" && !containsString(keys, wantThumb) {
		return fmt.Errorf("cleanup:objects missing thumbnail %q; got %v", wantThumb, keys)
	}

	log.Printf("ok: tombstoned A=%s; B's video likes 2->1 comments 1->0; A's rows gone; cleanup:objects enqueued (%d keys)", userA.ID, len(keys))
	return nil
}

// ----- helpers -----

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string, isPrivate, isActive bool) (db.User, error) {
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	var u db.User
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		return u, nil
	}
	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase8a-" + username,
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
		Title:  sql.NullString{String: "phase8a-smoke-" + slug, Valid: true},
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

// likeTx mirrors SocialHandler.LikeVideo's tx body
// (insert like + increment counter + owner notif).
// We don't need the full handler wiring here - the
// goal is to put a real like + notif on disk so
// DeleteUserData has something to delete.
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
		if actor.ID != video.UserID {
			payload, _ := json.Marshal(map[string]string{
				"username": actor.Username,
				"video_id": video.ID.String(),
			})
			if _, nerr := qtx.InsertNotification(ctx, db.InsertNotificationParams{
				UserID: video.UserID, ActorID: actor.ID, Type: "like", Payload: payload,
			}); nerr != nil {
				return fmt.Errorf("notif: %w", nerr)
			}
		}
	case errors.Is(lerr, sql.ErrNoRows):
		// already liked
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
	c, cerr := qtx.InsertComment(ctx, db.InsertCommentParams{
		VideoID: video.ID, UserID: actor.ID, Content: body,
	})
	if cerr != nil {
		return fmt.Errorf("insert: %w", cerr)
	}
	if err := qtx.IncrementCommentsCount(ctx, video.ID); err != nil {
		return fmt.Errorf("increment: %w", err)
	}
	if actor.ID != video.UserID {
		payload, _ := json.Marshal(map[string]string{
			"username": actor.Username, "video_id": video.ID.String(), "comment_id": c.ID.String(),
		})
		if _, nerr := qtx.InsertNotification(ctx, db.InsertNotificationParams{
			UserID: video.UserID, ActorID: actor.ID, Type: "comment", Payload: payload,
		}); nerr != nil {
			return fmt.Errorf("notif: %w", nerr)
		}
	}
	return tx.Commit()
}

// assertTableCount asserts that a table keyed by
// user_id has exactly `want` rows for the given user.
func assertTableCount(ctx context.Context, sqlDB *sql.DB, table string, userID uuid.UUID, want int) error {
	var n int
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE user_id = $1", table)
	if err := sqlDB.QueryRowContext(ctx, q, userID).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", table, err)
	}
	if n != want {
		return fmt.Errorf("%s by user_id=%s = %d, want %d", table, userID, n, want)
	}
	return nil
}

// assertTableCount2 is the variant for tables whose
// key column is not user_id.
func assertTableCount2(ctx context.Context, sqlDB *sql.DB, table, col string, userID uuid.UUID, want int) error {
	var n int
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = $1", table, col)
	if err := sqlDB.QueryRowContext(ctx, q, userID).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", table, err)
	}
	if n != want {
		return fmt.Errorf("%s by %s=%s = %d, want %d", table, col, userID, n, want)
	}
	return nil
}

// drainQueue deletes all pending tasks of a given
// type from the default queue, so a prior run
// cannot contaminate the post-check.
func drainQueue(ctx context.Context, ins *asynq.Inspector, taskType string) error {
	const page = 50
	for {
		tasks, err := ins.ListPendingTasks("default", asynq.PageSize(page))
		if err != nil {
			return fmt.Errorf("ListPendingTasks: %w", err)
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
		// If we paged through and matched none,
		// there is no more matching work. If we
		// matched but the page is not full, the
		// next iteration will see an empty list.
		if matched == 0 && len(tasks) < page {
			return nil
		}
	}
}

// findCleanupObjectsKeys scans pending tasks on
// the default queue, returns the first
// cleanup:objects task's keys. Returns (nil,
// false, nil) if none enqueued.
func findCleanupObjectsKeys(ctx context.Context, ins *asynq.Inspector) ([]string, bool, error) {
	const page = 50
	tasks, err := ins.ListPendingTasks("default", asynq.PageSize(page))
	if err != nil {
		return nil, false, fmt.Errorf("ListPendingTasks: %w", err)
	}
	for _, t := range tasks {
		if t.Type != "cleanup:objects" {
			continue
		}
		var p shared.CleanupObjectsPayload
		if jerr := json.Unmarshal(t.Payload, &p); jerr != nil {
			return nil, false, fmt.Errorf("unmarshal payload: %w", jerr)
		}
		return p.Keys, true, nil
	}
	return nil, false, nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
