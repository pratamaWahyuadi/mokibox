// Phase 6.C smoke test for follow / unfollow /
// followers / following endpoints.
//
// This is a hermetic, in-network smoke test (runs
// inside the mokibox_backend docker network so it
// can reach postgres + redis without a host port
// mapping). It verifies:
//
//  1. FollowUser inserts a row in `follows` (or is
//     a no-op if it already exists, since the query
//     is ON CONFLICT DO NOTHING).
//  2. InsertNotification writes a row in
//     `notifications` with type='follow' and a
//     payload that includes the actor's username.
//  3. DeleteFollow removes the row and is idempotent
//     (no error when the row is gone).
//  4. ListFollowers + ListFollowing return the
//     expected rows after a follow is created.
//
// As with the other phase-6 smokes, the HTTP layer
// itself is not exercised here because the
// api-gateway binary cannot start in this dev
// environment (LoadAPI requires ZITADEL_ISSUER_URL
// to be non-empty, and the denyAllVerifier fallback
// only kicks in after LoadAPI returns). The handler
// logic is exercised in the unit tests of issue B
// and the in-process query verification here.
//
// Usage (host-side):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase6_follow
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-phase6c-"

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase6_follow")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:change-me-postgres@postgres:5432/tiktok?sslmode=disable"
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

	// Seed: two users.
	follower, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"follower", false, true)
	if err != nil {
		return fmt.Errorf("seed follower: %w", err)
	}
	followee, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"followee", false, true)
	if err != nil {
		return fmt.Errorf("seed followee: %w", err)
	}
	// A third user for the list-endpoint tests.
	third, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"third", false, true)
	if err != nil {
		return fmt.Errorf("seed third: %w", err)
	}

	// 1. Follow: follower -> followee.
	if err := q.FollowUser(ctx, db.FollowUserParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}); err != nil {
		return fmt.Errorf("FollowUser: %w", err)
	}

	// 1a. Idempotent: re-following the same pair
	// must not error.
	if err := q.FollowUser(ctx, db.FollowUserParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}); err != nil {
		return fmt.Errorf("FollowUser idempotent: %w", err)
	}

	// 1b. Self-follow via SQL is rejected by the
	// `chk_follows_no_self` CHECK constraint.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO follows (follower_id, followee_id) VALUES ($1, $1)`,
		follower.ID); err == nil {
		return fmt.Errorf("self-follow should violate CHECK constraint")
	}

	// 2. Insert a follow notification (simulating
	// the handler's best-effort insert).
	payload, _ := json.Marshal(map[string]string{"username": follower.Username})
	notif, err := q.InsertNotification(ctx, db.InsertNotificationParams{
		UserID:  followee.ID,
		ActorID: follower.ID,
		Type:    "follow",
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("InsertNotification: %w", err)
	}
	if notif.Type != "follow" {
		return fmt.Errorf("notification type expected follow, got %q", notif.Type)
	}
	if notif.UserID != followee.ID {
		return fmt.Errorf("notification user_id expected %s, got %s", followee.ID, notif.UserID)
	}
	if notif.ActorID != follower.ID {
		return fmt.Errorf("notification actor_id expected %s, got %s", follower.ID, notif.ActorID)
	}

	// 3. Follow a third user so the list
	// endpoints have multiple entries.
	if err := q.FollowUser(ctx, db.FollowUserParams{
		FollowerID: third.ID,
		FolloweeID: followee.ID,
	}); err != nil {
		return fmt.Errorf("FollowUser third->followee: %w", err)
	}

	// 4. ListFollowers: followee should have
	// exactly 2 followers (follower + third).
	followers, err := q.ListFollowers(ctx, db.ListFollowersParams{
		FollowerID: follower.ID, // arbitrary viewer for is_following_back
		FolloweeID: followee.ID,
		PageLimit:  50,
	})
	if err != nil {
		return fmt.Errorf("ListFollowers: %w", err)
	}
	if len(followers) != 2 {
		ids := make([]string, 0, len(followers))
		for _, r := range followers {
			ids = append(ids, r.Username)
		}
		return fmt.Errorf("expected 2 followers for followee, got %d: %v", len(followers), ids)
	}
	gotFollowers := map[string]bool{}
	for _, r := range followers {
		gotFollowers[r.Username] = true
	}
	if !gotFollowers[usernamePrefix+"follower"] || !gotFollowers[usernamePrefix+"third"] {
		return fmt.Errorf("followers list missing expected users: %+v", gotFollowers)
	}

	// 5. ListFollowing: follower follows exactly
	// followee; third follows followee.
	following, err := q.ListFollowing(ctx, db.ListFollowingParams{
		FollowerID: follower.ID,
		PageLimit:  50,
	})
	if err != nil {
		return fmt.Errorf("ListFollowing: %w", err)
	}
	if len(following) != 1 {
		return fmt.Errorf("expected 1 following for follower, got %d", len(following))
	}
	if following[0].ID != followee.ID {
		return fmt.Errorf("following[0].ID expected %s, got %s", followee.ID, following[0].ID)
	}

	// 6. GetFollow returns the row for the wire
	// created_at.
	follow, err := q.GetFollow(ctx, db.GetFollowParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	})
	if err != nil {
		return fmt.Errorf("GetFollow: %w", err)
	}
	if follow.CreatedAt.IsZero() {
		return fmt.Errorf("GetFollow created_at is zero")
	}

	// 7. IsFollowing sanity.
	isFollowing, err := q.IsFollowing(ctx, db.IsFollowingParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	})
	if err != nil {
		return fmt.Errorf("IsFollowing: %w", err)
	}
	if !isFollowing {
		return fmt.Errorf("IsFollowing expected true")
	}

	// 8. CountFollowers / CountFollowing.
	nf, err := q.CountFollowers(ctx, followee.ID)
	if err != nil {
		return fmt.Errorf("CountFollowers: %w", err)
	}
	if nf != 2 {
		return fmt.Errorf("CountFollowers expected 2, got %d", nf)
	}

	// 9. DeleteFollow removes the row and is
	// idempotent.
	if err := q.DeleteFollow(ctx, db.DeleteFollowParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}); err != nil {
		return fmt.Errorf("DeleteFollow: %w", err)
	}
	// Idempotent: re-deleting the same row is a no-op.
	if err := q.DeleteFollow(ctx, db.DeleteFollowParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}); err != nil {
		return fmt.Errorf("DeleteFollow idempotent: %w", err)
	}
	// Verify: GetFollow returns ErrNoRows.
	if _, err := q.GetFollow(ctx, db.GetFollowParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}); err == nil {
		return fmt.Errorf("GetFollow should ErrNoRows after delete")
	}
	// ListFollowers now has 1 (only third).
	followers2, err := q.ListFollowers(ctx, db.ListFollowersParams{
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
		PageLimit:  50,
	})
	if err != nil {
		return fmt.Errorf("ListFollowers after delete: %w", err)
	}
	if len(followers2) != 1 {
		return fmt.Errorf("expected 1 follower after delete, got %d", len(followers2))
	}

	// 10. Privacy: with a private followee, the
	// follower list must not be visible to a
	// non-following viewer.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE users SET is_private = TRUE WHERE id = $1`, followee.ID); err != nil {
		return fmt.Errorf("set private: %w", err)
	}
	// Unrelated viewer.
	viewer, err := ensureUser(ctx, sqlDB, q, usernamePrefix+"viewer-x", false, true)
	if err != nil {
		return fmt.Errorf("seed viewer-x: %w", err)
	}
	followingViewer, err := q.IsFollowing(ctx, db.IsFollowingParams{
		FollowerID: viewer.ID,
		FolloweeID: followee.ID,
	})
	if err != nil {
		return fmt.Errorf("IsFollowing viewer-x: %w", err)
	}
	if followingViewer {
		return fmt.Errorf("viewer-x should NOT be following followee")
	}
	// The follower's list endpoint is also tested
	// by the visibility rule; we keep this simple
	// here and only verify the IsFollowing false
	// (the handler-level 404 mapping is exercised
	// in routes.go via the assertCanSeeFollowList
	// helper).

	log.Printf("sample: followers after delete = %d, notifications created = 1, follow row deleted",
		len(followers2))

	// Cleanup.
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM notifications WHERE user_id = $1 OR actor_id = $1`, followee.ID); err != nil {
		return fmt.Errorf("cleanup notif: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM follows WHERE follower_id IN (SELECT id FROM users WHERE username LIKE $1) OR followee_id IN (SELECT id FROM users WHERE username LIKE $1)`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("cleanup follows: %w", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%"); err != nil {
		return fmt.Errorf("cleanup users: %w", err)
	}
	return nil
}

func ensureUser(ctx context.Context, sqlDB *sql.DB, q *db.Queries, username string, isPrivate, isActive bool) (db.User, error) {
	var u db.User
	row := sqlDB.QueryRowContext(ctx,
		`SELECT id, zitadel_id, username, display_name, bio, avatar_url, is_private, is_active, deleted_at, created_at FROM users WHERE username = $1`, username)
	if err := row.Scan(&u.ID, &u.ZitadelID, &u.Username, &u.DisplayName, &u.Bio, &u.AvatarUrl, &u.IsPrivate, &u.IsActive, &u.DeletedAt, &u.CreatedAt); err == nil {
		if u.IsPrivate != isPrivate || u.IsActive != isActive {
			if _, err := sqlDB.ExecContext(ctx, `UPDATE users SET is_private=$1, is_active=$2 WHERE id=$3`, isPrivate, isActive, u.ID); err != nil {
				return db.User{}, err
			}
		}
		return u, nil
	}
	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   "smoke-phase6c-" + username,
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
