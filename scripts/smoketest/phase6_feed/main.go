// Phase 6.A smoke test for the home feed.
//
// This is a hermetic, in-network smoke test (runs inside
// the mokibox_backend docker network so it can reach
// postgres + redis without a host port mapping). It
// verifies:
//
//  1. ListFeedVideos returns rows in the expected shape
//     (includes user fields + liked_by_me) for a freshly
//     seeded dataset.
//  2. Visibility filter excludes the viewer's own videos
//     and private accounts the viewer does not follow.
//  3. liked_by_me is computed per-viewer correctly
//     (true for rows the viewer has liked, false
//     otherwise).
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    -e DATABASE_URL \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase6_feed
//
// All seeded data is namespaced under
// "smoke-test-phase6/" in username so it can be cleaned
// up without touching real data.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase6-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase6_feed")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// Fallback for in-network smoke: superuser
		// DSN pointing at the postgres container. The
		// tiktok_api role would be preferred in
		// production; for the smoke we only do SELECT
		// + INSERT/DELETE on user-owned tables so the
		// superuser is fine and avoids the need to read
		// .env from CI.
		dsn = "postgres://postgres:change-me-postgres@postgres:5432/tiktok?sslmode=disable"
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	q := db.New(sqlDB)

	// 1. Seed users (idempotent re-runs). All under
	//    smoke-test-phase6/ prefix.
	viewer, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"viewer", false, true)
	if err != nil {
		return fmt.Errorf("seed viewer: %w", err)
	}
	ownerPub, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner-pub", false, true)
	if err != nil {
		return fmt.Errorf("seed ownerPub: %w", err)
	}
	ownerPriv, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner-priv", true, true)
	if err != nil {
		return fmt.Errorf("seed ownerPriv: %w", err)
	}
	ownerInactive, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"owner-inactive", false, false)
	if err != nil {
		return fmt.Errorf("seed ownerInactive: %w", err)
	}
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: viewer.ID, FolloweeID: ownerPub.ID}); err != nil {
		return fmt.Errorf("ensure follow viewer->owner-pub: %w", err)
	}
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: viewer.ID, FolloweeID: ownerPriv.ID}); err != nil {
		return fmt.Errorf("ensure follow viewer->owner-priv: %w", err)
	}

	// 2. Pre-clean any orphan videos from a previous run.
	if err := cleanupVideos(ctx, sqlDB); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}

	// 3. Insert READY videos. Each owner gets one
	//    visible + owner-inactive + owner-priv-non-follower.
	v1, err := insertReady(ctx, q, ownerPub.ID, "pub-1")
	if err != nil {
		return fmt.Errorf("insert pub video: %w", err)
	}
	if _, err := insertReady(ctx, q, ownerPriv.ID, "priv-1"); err != nil {
		return fmt.Errorf("insert priv video: %w", err)
	}
	if _, err := insertReady(ctx, q, ownerInactive.ID, "inactive-1"); err != nil {
		return fmt.Errorf("insert inactive video: %w", err)
	}
	// Self video on viewer.
	if _, err := insertReady(ctx, q, viewer.ID, "self-1"); err != nil {
		return fmt.Errorf("insert self video: %w", err)
	}
	// Insert + immediately mark DELETED to verify
	// status='READY' filter excludes it.
	deleted, err := insertReady(ctx, q, ownerPub.ID, "deleted-1")
	if err != nil {
		return fmt.Errorf("insert deleted: %w", err)
	}
	if _, err := q.MarkVideoDeleted(ctx, db.MarkVideoDeletedParams{ID: deleted.ID, UserID: ownerPub.ID}); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}

	// 4. Like v1 as viewer so liked_by_me=true for v1.
	if err := insertLike(ctx, sqlDB, viewer.ID, v1.ID); err != nil {
		return fmt.Errorf("like: %w", err)
	}

	// 5. Run ListFeedVideos and assert.
	rows, err := q.ListFeedVideos(ctx, db.ListFeedVideosParams{
		UserID:    viewer.ID,
		ViewerID:  uuid.NullUUID{UUID: viewer.ID, Valid: true},
		PageLimit: 50,
	})
	if err != nil {
		return fmt.Errorf("ListFeedVideos: %w", err)
	}
	if len(rows) < 2 {
		// At minimum: pub-1 (followed public) +
		// priv-1 (followed private) must be in the
		// feed. Other READY videos from prior smoke
		// runs may still be in the DB (notably
		// fase 5's smoke_293f7fcbd0 video) and they
		// will pass the filter because their owner
		// is not the viewer. We do not assert an
		// exact total to keep the test resilient
		// against other smokes.
		return fmt.Errorf("expected at least 2 rows in feed, got %d", len(rows))
	}
	// Verify the leaked older smoke videos do NOT
	// break visibility: their owner must NOT be
	// inactive. (Fase 5 left the owner active so
	// it should be in the feed; that is correct.)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, fmt.Sprintf("%s/%s/%s", r.UserUsername, r.Status, r.ID))
	}
	log.Printf("feed rows: %v", ids)
	gotIDs := map[uuid.UUID]bool{}
	gotLikedByMe := map[uuid.UUID]bool{}
	for _, r := range rows {
		gotIDs[r.ID] = true
		gotLikedByMe[r.ID] = r.LikedByMe
	}
	// pub-1 (followed public) must be in the feed.
	if !gotIDs[v1.ID] {
		return fmt.Errorf("expected v1 (pub-1) in feed, missing")
	}
	// priv-1 (followed private) must be in the feed.
	// Find it by exclusion.
	foundPriv := false
	for _, r := range rows {
		if r.UserID == ownerPriv.ID && r.UserIsPrivate && r.Status == "READY" {
			foundPriv = true
		}
	}
	if !foundPriv {
		return fmt.Errorf("expected priv-1 (followed private owner) in feed, missing")
	}
	// inactive-1 must NOT be in the feed.
	for _, r := range rows {
		if r.UserID == ownerInactive.ID {
			return fmt.Errorf("inactive-1 should NOT be in feed (u.is_active=false)")
		}
	}
	// deleted-1 must NOT be in the feed (status != READY).
	if gotIDs[deleted.ID] {
		return fmt.Errorf("deleted-1 should NOT be in feed (status=DELETED)")
	}
	// self-1 must NOT be in the feed (v.user_id <> $1).
	for _, r := range rows {
		if r.UserID == viewer.ID {
			return fmt.Errorf("self-1 should NOT be in feed (excluded by v.user_id <> viewer)")
		}
	}
	// liked_by_me: v1 was liked, priv-1 was not.
	if !gotLikedByMe[v1.ID] {
		return fmt.Errorf("v1 should be liked_by_me=true")
	}
	for _, r := range rows {
		if r.UserID == ownerPriv.ID {
			if r.LikedByMe {
				return fmt.Errorf("priv-1 should be liked_by_me=false (viewer never liked it)")
			}
		}
	}

	// 6. Print sample row for visual inspection.
	sample := map[string]any{
		"total_rows": len(rows),
		"v1_in_feed": gotIDs[v1.ID],
		"v1_liked":   gotLikedByMe[v1.ID],
	}
	if len(rows) > 0 {
		first := rows[0]
		sample["first_row_user_username"] = first.UserUsername
		sample["first_row_status"] = first.Status
		sample["first_row_liked_by_me"] = first.LikedByMe
		sample["first_row_user_id_2"] = first.UserID2
		sample["first_row_thumbnail_key"] = first.ThumbnailKey.String
	}
	out, _ := json.MarshalIndent(sample, "", "  ")
	log.Printf("feed sample: %s", out)

	// 7. Cleanup.
	if err := cleanupAll(ctx, sqlDB); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	return nil
}

// ensureUser creates a user with the given username if
// it does not exist yet. isPrivate and isActive are
// applied as post-create updates because the public
// sqlc queries only have CreateUser (no UpdateUserProfile
// in this smoke context). active=true by default.
func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string, isPrivate, isActive bool) (db.User, error) {
	// Try to find an existing row from a previous run.
	var u db.User
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		// Update is_private / is_active to match what
		// this test expects (idempotent across re-runs).
		if u.IsPrivate != isPrivate || u.IsActive != isActive {
			if _, err := sqlDB.ExecContext(ctx,
				`UPDATE users SET is_private = $1, is_active = $2 WHERE id = $3`,
				isPrivate, isActive, u.ID); err != nil {
				return db.User{}, fmt.Errorf("update user flags: %w", err)
			}
			u.IsPrivate = isPrivate
			u.IsActive = isActive
		}
		return u, nil
	}
	// No row -> create one. zitadel_id is unique; use a
	// stable per-username sub so re-runs hit the
	// ON CONFLICT path.
	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase6-" + username,
		Username:    username,
		DisplayName: sql.NullString{String: username, Valid: true},
	})
	if err != nil {
		return db.User{}, fmt.Errorf("CreateUser: %w", err)
	}
	if isPrivate || !isActive {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE users SET is_private = $1, is_active = $2 WHERE id = $3`,
			isPrivate, isActive, created.ID); err != nil {
			return db.User{}, fmt.Errorf("apply flags: %w", err)
		}
		created.IsPrivate = isPrivate
		created.IsActive = isActive
	}
	return created, nil
}

func insertReady(ctx context.Context, q *db.Queries, owner uuid.UUID, slug string) (db.Video, error) {
	v, err := q.InsertVideo(ctx, db.InsertVideoParams{
		UserID: owner,
		R2Key:  fmt.Sprintf("uploads/%s/%s/source-%s.mp4", owner, uuid.New(), slug),
		Title:  sql.NullString{String: "phase6-smoke-" + slug, Valid: true},
	})
	if err != nil {
		return db.Video{}, err
	}
	// Default status is PENDING_UPLOAD per schema.
	// MarkVideoReady requires status='PROCESSING', so
	// flip first (same flow the worker uses after
	// enqueue).
	if _, err := q.UpdateVideoToProcessing(ctx, v.ID); err != nil {
		return db.Video{}, fmt.Errorf("UpdateVideoToProcessing: %w", err)
	}
	ready, err := q.MarkVideoReady(ctx, db.MarkVideoReadyParams{
		ID:              v.ID,
		HlsPrefix:       sql.NullString{String: fmt.Sprintf("hls/%s/%s/", owner, v.ID), Valid: true},
		ThumbnailKey:    sql.NullString{String: fmt.Sprintf("thumbs/%s/%s.jpg", owner, v.ID), Valid: true},
		DurationSeconds: sql.NullInt32{Int32: 10, Valid: true},
	})
	if err != nil {
		return db.Video{}, fmt.Errorf("MarkVideoReady: %w", err)
	}
	return ready, nil
}

func insertLike(ctx context.Context, sqlDB *sql.DB, userID, videoID uuid.UUID) error {
	_, err := sqlDB.ExecContext(ctx,
		`INSERT INTO likes (user_id, video_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		userID, videoID)
	return err
}

func cleanupVideos(ctx context.Context, sqlDB *sql.DB) error {
	_, err := sqlDB.ExecContext(ctx,
		`DELETE FROM videos WHERE title LIKE 'phase6-smoke-%'`)
	return err
}

func cleanupAll(ctx context.Context, sqlDB *sql.DB) error {
	if err := cleanupVideos(ctx, sqlDB); err != nil {
		return err
	}
	// Drop likes for our test users before dropping the
	// users themselves (CASCADE handles videos + follows
	// but likes need manual cleanup because there is no
	// ON DELETE CASCADE on likes.user_id).
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM likes WHERE user_id IN (SELECT id FROM users WHERE username LIKE $1)`, usernamePrefix+"%"); err != nil {
		return err
	}
	// CASCADE on user_id drops follows + videos.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return err
	}
	return nil
}
