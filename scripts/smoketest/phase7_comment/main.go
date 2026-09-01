// Phase 7.B smoke test for comment / list / delete /
// reply.
//
// Hermetic, in-network smoke (runs inside the
// mokibox_backend docker network). Verifies:
//
//  1. Comment tx: InsertComment + IncrementCommentsCount
//     + type='comment' notification to the video owner
//     (skipped on self-comment).
//  2. ListCommentsByVideo returns created_at DESC rows
//     with the joined user columns.
//  3. Reply: parent_id set; notification fan-out to
//     video owner + parent author with dedup (actor,
//     owner, parent-author distinct vs overlapping).
//  4. Delete: CountCommentSubtree counts comment +
//     replies; DeleteCommentByID cascades;
//     DecrementCommentsCountBy drops the counter by
//     the subtree size in one statement.
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo -e DATABASE_URL=... \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase7_comment
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

const usernamePrefix = "smoke-test-phase7b-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase7_comment")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL required (superuser DSN)")
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

	owner, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner")
	if err != nil {
		return fmt.Errorf("seed owner: %w", err)
	}
	commenter, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"commenter")
	if err != nil {
		return fmt.Errorf("seed commenter: %w", err)
	}
	video, err := insertReady(ctx, q, owner.ID, "b")
	if err != nil {
		return fmt.Errorf("seed ready video: %w", err)
	}

	// --- 1. Create top-level comment as commenter.
	top, err := commentTx(ctx, sqlDB, q, commenter, video, uuid.NullUUID{}, "nice one")
	if err != nil {
		return fmt.Errorf("create comment: %w", err)
	}
	v, _ := q.GetVideoByID(ctx, video.ID)
	if v.CommentsCount != 1 {
		return fmt.Errorf("comments_count=%d, want 1", v.CommentsCount)
	}
	var notif int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$2 AND type='comment'`, owner.ID, commenter.ID).Scan(&notif); err != nil {
		return err
	}
	if notif != 1 {
		return fmt.Errorf("comment notifications=%d, want 1", notif)
	}
	// Payload carries comment_id (contract shape).
	var payload []byte
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT payload FROM notifications WHERE type='comment' AND user_id=$1 ORDER BY created_at DESC LIMIT 1`,
		owner.ID).Scan(&payload); err != nil {
		return fmt.Errorf("load payload: %w", err)
	}
	var parsed map[string]string
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return fmt.Errorf("unmarshal payload: %w", err)
	}
	if parsed["comment_id"] == "" || parsed["video_id"] != video.ID.String() {
		return fmt.Errorf("payload mismatch: %v", parsed)
	}

	// --- Self-comment: owner comments on own video -> no notif.
	if _, err := commentTx(ctx, sqlDB, q, owner, video, uuid.NullUUID{}, "owner note"); err != nil {
		return fmt.Errorf("self comment: %w", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$1 AND type='comment'`, owner.ID).Scan(&notif); err != nil {
		return err
	}
	if notif != 0 {
		return fmt.Errorf("self-comment notifications=%d, want 0", notif)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.CommentsCount != 2 {
		return fmt.Errorf("comments_count after self-comment=%d, want 2", v.CommentsCount)
	}

	// --- 2. List: newest first, 2 rows.
	rows, err := q.ListCommentsByVideo(ctx, db.ListCommentsByVideoParams{
		VideoID:   video.ID,
		PageLimit: 10,
	})
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	if len(rows) != 2 {
		return fmt.Errorf("listed %d comments, want 2", len(rows))
	}
	if rows[0].CreatedAt.Before(rows[1].CreatedAt) {
		return fmt.Errorf("list not ordered created_at DESC")
	}
	if rows[0].Username == "" {
		return fmt.Errorf("joined username empty")
	}

	// --- 3. Reply as owner to commenter's top-level comment.
	//    Recipients: owner==actor (skip), parent author=commenter -> 1 notif.
	reply, err := commentTx(ctx, sqlDB, q, owner, video, uuid.NullUUID{UUID: top.ID, Valid: true}, "thanks!")
	if err != nil {
		return fmt.Errorf("reply: %w", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$2 AND type='comment'`, commenter.ID, owner.ID).Scan(&notif); err != nil {
		return err
	}
	if notif != 1 {
		return fmt.Errorf("reply notif to parent author=%d, want 1", notif)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.CommentsCount != 3 {
		return fmt.Errorf("comments_count after reply=%d, want 3", v.CommentsCount)
	}
	// Dedup case: commenter replies to their own comment on
	// owner's video -> only owner gets notified (parent author
	// == actor is skipped).
	if _, err := commentTx(ctx, sqlDB, q, commenter, video, uuid.NullUUID{UUID: top.ID, Valid: true}, "self reply"); err != nil {
		return fmt.Errorf("self-reply: %w", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE user_id=$1 AND actor_id=$2 AND type='comment'`, commenter.ID, commenter.ID).Scan(&notif); err != nil {
		return err
	}
	if notif != 0 {
		return fmt.Errorf("self-reply notif=%d, want 0", notif)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.CommentsCount != 4 {
		return fmt.Errorf("comments_count after self-reply=%d, want 4", v.CommentsCount)
	}

	// --- 4. Delete top comment: subtree = top + owner's
	//    reply + commenter's self-reply = 3 rows; counter
	//    drops by 3 (4 -> 1, owner's top-level note stays).
	subtree, err := q.CountCommentSubtree(ctx, top.ID)
	if err != nil {
		return fmt.Errorf("count subtree: %w", err)
	}
	if subtree != 3 {
		return fmt.Errorf("subtree=%d, want 3", subtree)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	qtx := q.WithTx(tx)
	if err := qtx.DeleteCommentByID(ctx, top.ID); err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	if err := qtx.DecrementCommentsCountBy(ctx, db.DecrementCommentsCountByParams{
		ID:            video.ID,
		CommentsCount: int32(subtree),
	}); err != nil {
		return fmt.Errorf("decrement by: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete: %w", err)
	}
	v, _ = q.GetVideoByID(ctx, video.ID)
	if v.CommentsCount != 1 {
		return fmt.Errorf("comments_count after subtree delete=%d, want 1", v.CommentsCount)
	}
	// Cascade: reply row gone.
	if _, err := q.GetCommentByID(ctx, reply.ID); !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reply should be cascade-deleted, err=%v", err)
	}

	log.Printf("ok: comment tx, list order, reply fan-out (dedup), subtree delete verified (final comments_count=1)")
	return nil
}

// commentTx mirrors SocialHandler.CreateComment /
// ReplyComment: InsertComment -> IncrementCommentsCount
// -> notifications (video owner + parent author,
// dedup, skip actor).
func commentTx(ctx context.Context, sqlDB *sql.DB, q *db.Queries, actor db.User, video db.Video, parentID uuid.NullUUID, content string) (db.Comment, error) {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return db.Comment{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	qtx := q.WithTx(tx)

	comment, err := qtx.InsertComment(ctx, db.InsertCommentParams{
		VideoID:  video.ID,
		UserID:   actor.ID,
		ParentID: parentID,
		Content:  content,
	})
	if err != nil {
		return db.Comment{}, fmt.Errorf("insert: %w", err)
	}
	if err := qtx.IncrementCommentsCount(ctx, video.ID); err != nil {
		return db.Comment{}, fmt.Errorf("increment: %w", err)
	}

	recipients := map[uuid.UUID]struct{}{}
	if video.UserID != actor.ID {
		recipients[video.UserID] = struct{}{}
	}
	if parentID.Valid {
		parent, perr := qtx.GetCommentByID(ctx, parentID.UUID)
		if perr != nil {
			return db.Comment{}, fmt.Errorf("load parent: %w", perr)
		}
		if parent.UserID != actor.ID {
			recipients[parent.UserID] = struct{}{}
		}
	}
	payload, merr := json.Marshal(map[string]string{
		"username":   actor.Username,
		"video_id":   video.ID.String(),
		"comment_id": comment.ID.String(),
	})
	if merr != nil {
		return db.Comment{}, fmt.Errorf("marshal: %w", merr)
	}
	for recipient := range recipients {
		if _, err := qtx.InsertNotification(ctx, db.InsertNotificationParams{
			UserID:  recipient,
			ActorID: actor.ID,
			Type:    "comment",
			Payload: payload,
		}); err != nil {
			return db.Comment{}, fmt.Errorf("notify: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return db.Comment{}, fmt.Errorf("commit: %w", err)
	}
	return comment, nil
}

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string) (db.User, error) {
	var u db.User
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		return u, nil
	}
	return q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase7b-" + username,
		Username:    username,
		DisplayName: sql.NullString{String: username, Valid: true},
	})
}

func insertReady(ctx context.Context, q *db.Queries, owner uuid.UUID, slug string) (db.Video, error) {
	v, err := q.InsertVideo(ctx, db.InsertVideoParams{
		UserID: owner,
		R2Key:  fmt.Sprintf("uploads/%s/%s/source-%s.mp4", owner, uuid.New(), slug),
		Title:  sql.NullString{String: "phase7b-smoke-" + slug, Valid: true},
	})
	if err != nil {
		return db.Video{}, err
	}
	if _, err := q.UpdateVideoToProcessing(ctx, v.ID); err != nil {
		return db.Video{}, err
	}
	return q.MarkVideoReady(ctx, db.MarkVideoReadyParams{
		ID:              v.ID,
		HlsPrefix:       sql.NullString{String: fmt.Sprintf("hls/%s/%s/", owner, v.ID), Valid: true},
		ThumbnailKey:    sql.NullString{String: fmt.Sprintf("thumbs/%s/%s.jpg", owner, v.ID), Valid: true},
		DurationSeconds: sql.NullInt32{Int32: 12, Valid: true},
	})
}
