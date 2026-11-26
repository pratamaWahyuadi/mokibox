//go:build integration

package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/google/uuid"
)

// Integration tests for the avatar-related user queries. They only run
// when the test binary is built with `-tags=integration` and DATABASE_URL
// points at a real PostgreSQL (e.g. mokibox-postgres on the
// mokibox_backend network, or its container IP).
//
// The tests create and delete their own temporary users; they never
// touch real accounts such as user test1.

func newIntegrationDB(t *testing.T) (*sql.DB, *Queries) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := New(sqlDB)
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	return sqlDB, q
}

// newTempUser creates a throwaway user owned by the test and deletes the
// row in t.Cleanup.
func newTempUser(t *testing.T, sqlDB *sql.DB, q *Queries) User {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	user, err := q.CreateUser(ctx, CreateUserParams{
		ZitadelID:   "smoke-db-" + suffix,
		Username:    "smoke_db_" + suffix,
		DisplayName: sql.NullString{String: "smoke db user", Valid: true},
	})
	if err != nil {
		t.Fatalf("create temp user: %v", err)
	}
	if user.ID == uuid.Nil {
		t.Fatal("CreateUser returned no row")
	}
	t.Cleanup(func() {
		if _, err := sqlDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID); err != nil {
			t.Errorf("cleanup: delete temp user %s: %v", user.ID, err)
		}
	})
	return user
}

func TestSetPendingAvatarUploadIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()

	user := newTempUser(t, sqlDB, q)

	keyA := "avatars/" + user.ID.String() + "/aaaaaaaa-0000-0000-0000-00000000000a.png"
	keyB := "avatars/" + user.ID.String() + "/bbbbbbbb-0000-0000-0000-00000000000b.png"

	row1, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: keyA, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("first SetPendingAvatarUpload: %v", err)
	}
	if row1.PreviousPendingAvatarKey.Valid {
		t.Fatalf("first call: PreviousPendingAvatarKey = %q, want invalid (no prior pending)", row1.PreviousPendingAvatarKey.String)
	}
	if row1.PendingAvatarKey.String != keyA {
		t.Fatalf("first call: PendingAvatarKey = %q, want %q", row1.PendingAvatarKey.String, keyA)
	}

	row2, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: keyB, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("second SetPendingAvatarUpload: %v", err)
	}
	if !row2.PreviousPendingAvatarKey.Valid || row2.PreviousPendingAvatarKey.String != keyA {
		t.Fatalf("second call: PreviousPendingAvatarKey = %+v, want %q", row2.PreviousPendingAvatarKey, keyA)
	}
	if row2.PendingAvatarKey.String != keyB {
		t.Fatalf("second call: PendingAvatarKey = %q, want %q", row2.PendingAvatarKey.String, keyB)
	}

	var dbKey sql.NullString
	if err := sqlDB.QueryRowContext(ctx, "SELECT pending_avatar_key FROM users WHERE id = $1", user.ID).Scan(&dbKey); err != nil {
		t.Fatalf("read users.pending_avatar_key: %v", err)
	}
	if dbKey.String != keyB {
		t.Fatalf("DB pending_avatar_key = %q, want %q", dbKey.String, keyB)
	}
}

func TestSetPendingAvatarUploadNoRowsIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()

	user := newTempUser(t, sqlDB, q)
	if _, err := q.DeactivateUser(ctx, user.ID); err != nil {
		t.Fatalf("deactivate user: %v", err)
	}
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: "avatars/" + user.ID.String() + "/x.png", Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetPendingAvatarUpload on inactive user: err = %v, want sql.ErrNoRows", err)
	}

	ghost := uuid.New()
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     ghost,
		PendingAvatarKey:       sql.NullString{String: "avatars/" + ghost.String() + "/y.png", Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetPendingAvatarUpload on missing user: err = %v, want sql.ErrNoRows", err)
	}
}

func TestConfirmAvatarUploadIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()

	user := newTempUser(t, sqlDB, q)
	key1 := "avatars/" + user.ID.String() + "/11111111-0000-0000-0000-000000000001.png"
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: key1, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("SetPendingAvatarUpload: %v", err)
	}

	row, err := q.ConfirmAvatarUpload(ctx, ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: key1, Valid: true},
	})
	if err != nil {
		t.Fatalf("ConfirmAvatarUpload success: %v", err)
	}
	if row.AvatarKey.String != key1 {
		t.Fatalf("ConfirmAvatarUpload row.AvatarKey = %q, want %q", row.AvatarKey.String, key1)
	}
	if row.PreviousAvatarKey.Valid {
		t.Fatalf("ConfirmAvatarUpload row.PreviousAvatarKey = %q, want invalid (fresh user)", row.PreviousAvatarKey.String)
	}

	// Replay: the pending key is cleared, so the confirm must not match.
	if _, err := q.ConfirmAvatarUpload(ctx, ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: key1, Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replay ConfirmAvatarUpload: err = %v, want sql.ErrNoRows", err)
	}

	// Wrong key for the same user (no pending): ErrNoRows.
	if _, err := q.ConfirmAvatarUpload(ctx, ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: key1 + "x", Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ConfirmAvatarUpload wrong key: err = %v, want sql.ErrNoRows", err)
	}
}

func TestConfirmAvatarUploadExpiredIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()

	user := newTempUser(t, sqlDB, q)
	key := "avatars/" + user.ID.String() + "/22222222-0000-0000-0000-000000000002.png"
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: key, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("SetPendingAvatarUpload: %v", err)
	}
	// Backdate the pending expiry so the confirm sees an expired intent.
	if _, err := sqlDB.ExecContext(ctx, "UPDATE users SET pending_avatar_expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1", user.ID); err != nil {
		t.Fatalf("backdate pending expiry: %v", err)
	}
	if _, err := q.ConfirmAvatarUpload(ctx, ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: key, Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ConfirmAvatarUpload with expired pending: err = %v, want sql.ErrNoRows", err)
	}
}

func TestUpdateUserProfileIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()

	user := newTempUser(t, sqlDB, q)

	// Confirm an avatar first so we can check avatar_key handling.
	avatarKey := "avatars/" + user.ID.String() + "/33333333-0000-0000-0000-000000000003.png"
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: avatarKey, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("SetPendingAvatarUpload: %v", err)
	}
	if _, err := q.ConfirmAvatarUpload(ctx, ConfirmAvatarUploadParams{
		ID:               user.ID,
		PendingAvatarKey: sql.NullString{String: avatarKey, Valid: true},
	}); err != nil {
		t.Fatalf("ConfirmAvatarUpload: %v", err)
	}

	// 1. bio-only update: bio changes, avatar_key and pending stay unchanged.
	bio := "smoke-bio-" + uuid.NewString()[:13]
	row, err := q.UpdateUserProfile(ctx, UpdateUserProfileParams{
		ID:  user.ID,
		Bio: sql.NullString{String: bio, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile bio-only: %v", err)
	}
	if !row.Bio.Valid || row.Bio.String != bio {
		t.Fatalf("row.Bio = %+v, want %q", row.Bio, bio)
	}
	if !row.AvatarKey.Valid || row.AvatarKey.String != avatarKey {
		t.Fatalf("row.AvatarKey = %+v, want %q (unchanged)", row.AvatarKey, avatarKey)
	}
	if row.PendingAvatarKey.Valid {
		t.Fatalf("row.PendingAvatarKey = %q, want invalid (unchanged: no pending)", row.PendingAvatarKey.String)
	}

	// 2. Set a new pending, then update with avatar_url: avatar_key and
	// pending_avatar_key are cleared, previous_avatar_key keeps the old
	// avatar_key.
	pendingKey := "avatars/" + user.ID.String() + "/44444444-0000-0000-0000-000000000004.png"
	if _, err := q.SetPendingAvatarUpload(ctx, SetPendingAvatarUploadParams{
		ID:                     user.ID,
		PendingAvatarKey:       sql.NullString{String: pendingKey, Valid: true},
		PendingAvatarExpiresAt: sql.NullTime{Time: time.Now().Add(15 * time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("SetPendingAvatarUpload: %v", err)
	}
	newURL := "https://example.com/" + user.ID.String() + "/avatar.png"
	row2, err := q.UpdateUserProfile(ctx, UpdateUserProfileParams{
		ID:        user.ID,
		AvatarUrl: sql.NullString{String: newURL, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile avatar_url: %v", err)
	}
	if !row2.AvatarUrl.Valid || row2.AvatarUrl.String != newURL {
		t.Fatalf("row2.AvatarUrl = %+v, want %q", row2.AvatarUrl, newURL)
	}
	if row2.AvatarKey.Valid {
		t.Fatalf("row2.AvatarKey = %q, want invalid (cleared)", row2.AvatarKey.String)
	}
	if row2.PendingAvatarKey.Valid {
		t.Fatalf("row2.PendingAvatarKey = %q, want invalid (cleared)", row2.PendingAvatarKey.String)
	}
	if !row2.PreviousAvatarKey.Valid || row2.PreviousAvatarKey.String != avatarKey {
		t.Fatalf("row2.PreviousAvatarKey = %+v, want %q", row2.PreviousAvatarKey, avatarKey)
	}

	// 3. Non-existent user -> ErrNoRows.
	ghost := uuid.New()
	if _, err := q.UpdateUserProfile(ctx, UpdateUserProfileParams{
		ID:  ghost,
		Bio: sql.NullString{String: "nope", Valid: true},
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("UpdateUserProfile missing user: err = %v, want sql.ErrNoRows", err)
	}
}

func TestDMBlocksDenyMutualFollowIntegration(t *testing.T) {
	sqlDB, q := newIntegrationDB(t)
	ctx := context.Background()
	first := newTempUser(t, sqlDB, q)
	second := newTempUser(t, sqlDB, q)

	if err := q.FollowUser(ctx, FollowUserParams{FollowerID: first.ID, FolloweeID: second.ID}); err != nil {
		t.Fatalf("first follow: %v", err)
	}
	if err := q.FollowUser(ctx, FollowUserParams{FollowerID: second.ID, FolloweeID: first.ID}); err != nil {
		t.Fatalf("second follow: %v", err)
	}
	eligible, err := q.IsDMMutualFollow(ctx, IsDMMutualFollowParams{ID: first.ID, ID_2: second.ID})
	if err != nil || !eligible {
		t.Fatalf("mutual follow eligible = %t, err = %v", eligible, err)
	}

	blocked, err := q.InsertDMBlock(ctx, InsertDMBlockParams{BlockerID: second.ID, BlockedID: first.ID})
	if err != nil || blocked != 1 {
		t.Fatalf("insert block rows = %d, err = %v", blocked, err)
	}
	eligible, err = q.IsDMMutualFollow(ctx, IsDMMutualFollowParams{ID: first.ID, ID_2: second.ID})
	if err != nil || eligible {
		t.Fatalf("blocked pair eligible = %t, err = %v", eligible, err)
	}

	unblocked, err := q.DeleteDMBlock(ctx, DeleteDMBlockParams{BlockerID: second.ID, BlockedID: first.ID})
	if err != nil || unblocked != 1 {
		t.Fatalf("delete block rows = %d, err = %v", unblocked, err)
	}
	eligible, err = q.IsDMMutualFollow(ctx, IsDMMutualFollowParams{ID: first.ID, ID_2: second.ID})
	if err != nil || !eligible {
		t.Fatalf("unblocked pair eligible = %t, err = %v", eligible, err)
	}
}
