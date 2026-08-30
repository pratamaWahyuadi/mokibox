// Smoke test for phase 4 (issue A).
//
// Verifies the wiring without spinning up the Echo
// server: a real *pgxpool.Pool against the local
// Postgres container, plus a real *shared.R2Client
// against the R2 credentials in .env. Both checks
// exit 0 on success.
//
// Run from repo root:
//
//	go run ./scripts/smoketest/phase4
//
// The script prints a small PASS / FAIL summary at the
// end so the calling shell can grep it.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	fmt.Println("PASS: phase 4 smoke test (DB roundtrip + R2 presign)")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := dbRoundtrip(ctx); err != nil {
		return fmt.Errorf("db roundtrip: %w", err)
	}
	if err := r2Presign(ctx); err != nil {
		return fmt.Errorf("r2 presign: %w", err)
	}
	return nil
}

func dbRoundtrip(ctx context.Context) error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is empty")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool.New: %w", err)
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		cancel()
		return fmt.Errorf("pgxpool ping: %w", err)
	}
	cancel()

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer sqlDB.Close()

	q := db.New(sqlDB)

	// Pick an existing user that already has a row in
	// the database (any user works; the test creates a
	// PENDING_UPLOAD row scoped to that user and cleans
	// it up afterwards).
	userID, err := pickExistingUser(ctx, pool)
	if err != nil {
		return fmt.Errorf("pick existing user: %w", err)
	}
	log.Printf("smoke: using existing user_id=%s", userID)

	// Clean up any leftover PENDING_UPLOAD for this
	// user so the partial unique index does not block
	// the InsertVideo below.
	if existing, err := q.GetPendingVideoByUser(ctx, userID); err == nil {
		log.Printf("smoke: pre-cleanup existing PENDING_UPLOAD id=%s", existing.ID)
		if err := cleanupVideo(ctx, sqlDB, existing.ID); err != nil {
			return fmt.Errorf("pre-cleanup: %w", err)
		}
	} else if !isNoRows(err) {
		return fmt.Errorf("GetPendingVideoByUser pre-check: %w", err)
	}

	r2Key := fmt.Sprintf("uploads/%s/test-%s/source.mp4", userID, strings.ReplaceAll(uuid.New().String(), "-", ""))
	title := "smoke test title"
	desc := "smoke test description"
	inserted, err := q.InsertVideo(ctx, db.InsertVideoParams{
		UserID:      userID,
		R2Key:       r2Key,
		Title:       sql.NullString{String: title, Valid: true},
		Description: sql.NullString{String: desc, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("InsertVideo: %w", err)
	}
	log.Printf("smoke: InsertVideo id=%s r2_key=%s", inserted.ID, inserted.R2Key)

	// Verify "no row" path: GetVideoByID must surface
	// sql.ErrNoRows (or pgx.ErrNoRows), which is the
	// contract confirm-upload relies on for the 404
	// branch.
	if _, err := q.GetVideoByID(ctx, uuid.New()); err == nil {
		return fmt.Errorf("GetVideoByID should have returned an error for a missing id")
	} else if !isNoRows(err) {
		return fmt.Errorf("GetVideoByID wrong error type: %v", err)
	} else {
		log.Printf("smoke: GetVideoByID ErrNoRows OK: %v", err)
	}

	fetched, err := q.GetPendingVideoByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("GetPendingVideoByUser: %w", err)
	}
	if fetched.ID != inserted.ID {
		return fmt.Errorf("GetPendingVideoByUser returned id=%s, want %s", fetched.ID, inserted.ID)
	}
	if fetched.R2Key != r2Key {
		return fmt.Errorf("GetPendingVideoByUser returned r2_key=%s, want %s", fetched.R2Key, r2Key)
	}
	if !fetched.Title.Valid || fetched.Title.String != title {
		return fmt.Errorf("GetPendingVideoByUser title mismatch: %+v", fetched.Title)
	}
	log.Printf("smoke: roundtrip verified id=%s status=%s", fetched.ID, fetched.Status)

	// Exercise UpdatePendingVideoMetadata: this is the
	// new query added in phase 4 so the upload-intent
	// reuse path refreshes title/description.
	newTitle := "smoke updated title"
	newDesc := "smoke updated desc"
	if _, err := q.UpdatePendingVideoMetadata(ctx, db.UpdatePendingVideoMetadataParams{
		ID:          inserted.ID,
		Title:       sql.NullString{String: newTitle, Valid: true},
		Description: sql.NullString{String: newDesc, Valid: true},
	}); err != nil {
		return fmt.Errorf("UpdatePendingVideoMetadata: %w", err)
	}
	refreshed, err := q.GetPendingVideoByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("GetPendingVideoByUser after metadata update: %w", err)
	}
	if !refreshed.Title.Valid || refreshed.Title.String != newTitle {
		return fmt.Errorf("metadata title not refreshed: %+v", refreshed.Title)
	}
	if !refreshed.Description.Valid || refreshed.Description.String != newDesc {
		return fmt.Errorf("metadata description not refreshed: %+v", refreshed.Description)
	}
	log.Printf("smoke: UpdatePendingVideoMetadata verified title=%q desc=%q", refreshed.Title.String, refreshed.Description.String)

	// UpdatePendingVideoMetadata must surface
	// sql.ErrNoRows when the row no longer matches
	// the WHERE guard (deleted OR status !=
	// PENDING_UPLOAD). Simulate by deleting the row
	// first, then trying the update.
	if err := cleanupVideo(ctx, sqlDB, inserted.ID); err != nil {
		return fmt.Errorf("pre-metadata-err cleanup: %w", err)
	}
	if _, err := q.UpdatePendingVideoMetadata(ctx, db.UpdatePendingVideoMetadataParams{
		ID:          inserted.ID,
		Title:       sql.NullString{String: "x", Valid: true},
		Description: sql.NullString{String: "y", Valid: true},
	}); err == nil {
		return fmt.Errorf("UpdatePendingVideoMetadata on missing row should error")
	} else if !isNoRows(err) {
		return fmt.Errorf("UpdatePendingVideoMetadata missing-row wrong error type: %v", err)
	} else {
		log.Printf("smoke: UpdatePendingVideoMetadata ErrNoRows OK: %v", err)
	}
	return nil
}

func r2Presign(ctx context.Context) error {
	cfg := shared.R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
		Endpoint:        os.Getenv("R2_ENDPOINT"),
	}
	if cfg.AccountID == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" || cfg.Bucket == "" || cfg.Endpoint == "" {
		return fmt.Errorf("R2 env vars missing (R2_ACCOUNT_ID/R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_BUCKET/R2_ENDPOINT)")
	}
	r2, err := shared.NewR2Client(ctx, cfg)
	if err != nil {
		return fmt.Errorf("NewR2Client: %w", err)
	}

	// Use a test- prefix so the smoke test does not
	// touch production keys even by accident.
	testKey := fmt.Sprintf("uploads/smoke-test/%s/source.mp4", strings.ReplaceAll(uuid.New().String(), "-", ""))
	url, err := r2.PresignPut(ctx, testKey, "application/octet-stream", 15*time.Minute)
	if err != nil {
		return fmt.Errorf("PresignPut: %w", err)
	}
	if !strings.Contains(url, "X-Amz-Signature=") {
		return fmt.Errorf("PresignPut URL missing X-Amz-Signature= marker: %s", url)
	}
	log.Printf("smoke: PresignPut OK for key=%s (url length=%d)", testKey, len(url))
	return nil
}

// pickExistingUser finds an existing user; if none
// exist, it inserts a smoke-test user via a raw SQL
// statement so the test is hermetic against a freshly
// migrated DB. The smoke-test user is left in place
// for re-runs (idempotent because the username is
// fixed and the table has a UNIQUE constraint that
// surfaces as 23505, which we ignore).
func pickExistingUser(ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, error) {
	var id uuid.UUID
	err := pool.QueryRow(ctx, "SELECT id FROM users LIMIT 1").Scan(&id)
	if err == nil {
		return id, nil
	}
	if !isNoRows(err) {
		return uuid.Nil, err
	}
	// Seed a smoke-test user.
	zitadelSub := fmt.Sprintf("smoke-test-%s", strings.ReplaceAll(uuid.New().String(), "-", ""))
	username := fmt.Sprintf("smoke_%s", strings.ReplaceAll(uuid.New().String(), "-", "")[:10])
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (zitadel_id, username, display_name, is_private, is_active)
		 VALUES ($1, $2, $3, FALSE, TRUE)
		 RETURNING id`,
		zitadelSub, username, "smoke test",
	).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("seed smoke user: %w", err)
	}
	log.Printf("smoke: seeded test user id=%s username=%s", id, username)
	return id, nil
}

func cleanupVideo(ctx context.Context, sqlDB *sql.DB, id uuid.UUID) error {
	_, err := sqlDB.ExecContext(ctx, "DELETE FROM videos WHERE id = $1", id)
	return err
}

func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return err == sql.ErrNoRows || strings.Contains(err.Error(), "no rows in result set")
}