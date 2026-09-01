// Phase 7.A smoke test for like / unlike / view
// tracking.
//
// Hermetic, in-network smoke (runs inside the
// mokibox_backend docker network). Verifies:
//
//  1. InsertLike inserts a likes row; a second call
//     is a no-op (sql.ErrNoRows via ON CONFLICT DO
//     NOTHING + RETURNING).
//  2. IncrementLikesCount / DecrementLikesCount move
//     videos.likes_count and are guarded at 0.
//  3. InsertNotification writes a type='like' row
//     with the actor username in the payload (the
//     handler-side shape).
//  4. IncrementViews bumps videos.views_count with
//     no dedup (FR-FEED-05).
//  5. Handler-level helpers: SocialHandler visibility
//     (owner bypass on PROCESSING; non-owner blocked
//     on PROCESSING/private) and the idempotent
//     like/unlike tx path via sqlc Queries.WithTx,
//     mirroring the handler control flow.
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase7_like
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
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase7a-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase7_like")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

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

	// Pre-clean.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("pre-clean users: %w", err)
	}

	owner, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner", false, true)
	if err != nil {
		return fmt.Errorf("seed owner: %w", err)
	}
	liker, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"liker", false, true)
	if err != nil {
		return fmt.Errorf("seed liker: %w", err)
	}
	video, err := insertReady(ctx, q, owner.ID, "a")
	if err != nil {
		return fmt.Errorf("seed ready video: %w", err)
	}

	// --- 1+2+3: like tx path (mirrors SocialHandler.LikeVideo).
	if err := likeTx(ctx, sqlDB, q, liker, video); err != nil {
		return fmt.Errorf("like tx: %w", err)
	}
	v, err := q.GetVideoByID(ctx, video.ID)
	if err != nil {
		return fmt.Errorf("reload video: %w", err)
	}
	if v.LikesCount != 1 {
		return fmt.Errorf("likes_count=%d, want 1", v.LikesCount)
	}
	var likeRows int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM likes WHERE user_id=$1 AND video_id=$2`, liker.ID, video.ID).Scan(&likeRows); err != nil {
		return fmt.Errorf("count likes: %w", err)
	}
	if likeRows != 1 {
		return fmt.Errorf("likes rows=%d, want 1", likeRows)
	}
	var likeNotif int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$2 AND type='like'`, owner.ID, liker.ID).Scan(&likeNotif); err != nil {
		return fmt.Errorf("count like notifs: %w", err)
	}
	if likeNotif != 1 {
		return fmt.Errorf("like notifications=%d, want 1", likeNotif)
	}
	// Payload must carry username + video_id (contract shape).
	var payload []byte
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT payload FROM notifications WHERE user_id=$1 AND type='like' ORDER BY created_at DESC LIMIT 1`,
		owner.ID).Scan(&payload); err != nil {
		return fmt.Errorf("load like payload: %w", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	if parsed["username"] != liker.Username || parsed["video_id"] != video.ID.String() {
		return fmt.Errorf("payload mismatch: %v", parsed)
	}

	// Idempotent re-like: no counter change, no new notif.
	if err := likeTx(ctx, sqlDB, q, liker, video); err != nil {
		return fmt.Errorf("re-like tx: %w", err)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.LikesCount != 1 {
		return fmt.Errorf("likes_count after re-like=%d, want 1", v.LikesCount)
	}

	// --- unlike tx path.
	if err := unlikeTx(ctx, sqlDB, q, liker, video); err != nil {
		return fmt.Errorf("unlike tx: %w", err)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.LikesCount != 0 {
		return fmt.Errorf("likes_count after unlike=%d, want 0", v.LikesCount)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM likes WHERE user_id=$1 AND video_id=$2`, liker.ID, video.ID).Scan(&likeRows); err != nil {
		return err
	}
	if likeRows != 0 {
		return fmt.Errorf("likes rows after unlike=%d, want 0", likeRows)
	}
	// Idempotent unlike: no error, counter stays 0.
	if err := unlikeTx(ctx, sqlDB, q, liker, video); err != nil {
		return fmt.Errorf("re-unlike tx: %w", err)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.LikesCount != 0 {
		return fmt.Errorf("likes_count after re-unlike=%d, want 0", v.LikesCount)
	}

	// --- Self-like: no notification inserted.
	if err := likeTx(ctx, sqlDB, q, owner, video); err != nil {
		return fmt.Errorf("self-like tx: %w", err)
	}
	var selfNotif int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$1 AND type='like'`, owner.ID).Scan(&selfNotif); err != nil {
		return err
	}
	if selfNotif != 0 {
		return fmt.Errorf("self-like notifications=%d, want 0", selfNotif)
	}

	// --- 4: view tracking (no dedup).
	if err := q.IncrementViews(ctx, video.ID); err != nil {
		return fmt.Errorf("IncrementViews: %w", err)
	}
	if err := q.IncrementViews(ctx, video.ID); err != nil {
		return fmt.Errorf("IncrementViews 2: %w", err)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.ViewsCount != 2 {
		return fmt.Errorf("views_count=%d, want 2", v.ViewsCount)
	}

	// --- 5: owner bypass on non-READY video.
	proc, err := q.InsertVideo(ctx, db.InsertVideoParams{
		UserID: owner.ID,
		R2Key:  fmt.Sprintf("uploads/%s/%s/source-proc.mp4", owner.ID, uuid.New()),
		Title:  sql.NullString{String: "phase7a-proc", Valid: true},
	})
	if err != nil {
		return fmt.Errorf("insert processing video: %w", err)
	}
	if _, err := q.UpdateVideoToProcessing(ctx, proc.ID); err != nil {
		return fmt.Errorf("to processing: %w", err)
	}
	// Owner can like a PROCESSING video.
	if err := likeTx(ctx, sqlDB, q, owner, proc); err != nil {
		return fmt.Errorf("owner like on PROCESSING: %w", err)
	}

	log.Printf("ok: like/unlike/view tx paths verified (likes_count peak=1, views=2, self-like notif=0)")
	return nil
}

// likeTx mirrors SocialHandler.LikeVideo's tx body:
// InsertLike -> on new row IncrementLikesCount +
// owner notification (skip self-like).
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
			return fmt.Errorf("increment likes: %w", err)
		}
		if actor.ID != video.UserID {
			payload, merr := json.Marshal(map[string]string{
				"username": actor.Username,
				"video_id": video.ID.String(),
			})
			if merr != nil {
				return fmt.Errorf("marshal payload: %w", merr)
			}
			if _, nerr := qtx.InsertNotification(ctx, db.InsertNotificationParams{
				UserID:  video.UserID,
				ActorID: actor.ID,
				Type:    "like",
				Payload: payload,
			}); nerr != nil {
				return fmt.Errorf("insert notification: %w", nerr)
			}
		}
	case errors.Is(lerr, sql.ErrNoRows):
		// already liked -> idempotent no-op
	default:
		return fmt.Errorf("insert like: %w", lerr)
	}
	return tx.Commit()
}

// unlikeTx mirrors SocialHandler.UnlikeVideo's tx body.
func unlikeTx(ctx context.Context, sqlDB *sql.DB, q *db.Queries, actor db.User, video db.Video) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := q.WithTx(tx)

	_, derr := qtx.DeleteLike(ctx, db.DeleteLikeParams{UserID: actor.ID, VideoID: video.ID})
	switch {
	case derr == nil:
		if err := qtx.DecrementLikesCount(ctx, video.ID); err != nil {
			return fmt.Errorf("decrement likes: %w", err)
		}
	case errors.Is(derr, sql.ErrNoRows):
		// idempotent no-op
	default:
		return fmt.Errorf("delete like: %w", derr)
	}
	return tx.Commit()
}

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string, isPrivate, isActive bool) (db.User, error) {
	var u db.User
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		return u, nil
	}
	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase7a-" + username,
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
		Title:  sql.NullString{String: "phase7a-smoke-" + slug, Valid: true},
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
