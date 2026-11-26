//go:build integration

package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type localMarketplaceVerifier struct{}

func (localMarketplaceVerifier) CheckToken(_ context.Context, rawToken string) (string, error) {
	if rawToken == "" {
		return "", fmt.Errorf("empty token")
	}
	return rawToken, nil
}

func TestMarketplaceLocalHTTPSmoke(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is required for the local Marketplace HTTP smoke")
	}
	ctx := context.Background()
	sqlDB, err := shared.NewSQLDB(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	q := db.New(sqlDB)

	var sellerIDText, sellerToken, buyerIDText, buyerToken string
	err = sqlDB.QueryRowContext(ctx, `SELECT id::text, zitadel_id FROM users WHERE is_active = TRUE ORDER BY created_at, id OFFSET 0 LIMIT 1`).Scan(&sellerIDText, &sellerToken)
	if err == sql.ErrNoRows {
		sellerID := uuid.New()
		sellerToken = "smoke-seller-" + sellerID.String()
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO users (id, zitadel_id, username, is_active) VALUES ($1, $2, $3, TRUE) RETURNING id::text, zitadel_id`, sellerID, sellerToken, "smoke_seller_"+sellerID.String()[:8]).Scan(&sellerIDText, &sellerToken); err != nil {
			t.Fatalf("seed seller: %v", err)
		}
	} else if err != nil {
		t.Fatalf("seller: %v", err)
	}
	err = sqlDB.QueryRowContext(ctx, `SELECT id::text, zitadel_id FROM users WHERE is_active = TRUE ORDER BY created_at, id OFFSET 1 LIMIT 1`).Scan(&buyerIDText, &buyerToken)
	if err == sql.ErrNoRows {
		buyerID := uuid.New()
		buyerToken = "smoke-buyer-" + buyerID.String()
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO users (id, zitadel_id, username, is_active) VALUES ($1, $2, $3, TRUE) RETURNING id::text, zitadel_id`, buyerID, buyerToken, "smoke_buyer_"+buyerID.String()[:8]).Scan(&buyerIDText, &buyerToken); err != nil {
			t.Fatalf("seed buyer: %v", err)
		}
	} else if err != nil {
		t.Fatalf("buyer: %v", err)
	}
	sellerID, err := uuid.Parse(sellerIDText)
	if err != nil {
		t.Fatal(err)
	}
	buyerID, err := uuid.Parse(buyerIDText)
	if err != nil {
		t.Fatal(err)
	}

	r2, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID: "smoke", AccessKeyID: "smoke", SecretAccessKey: "smoke",
		Bucket: "smoke", Endpoint: "https://example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:56379"
	}
	queue := asynq.NewClient(asynq.RedisClientOpt{Addr: redisAddr})
	defer queue.Close()
	cfg := &shared.APIConfig{
		APIBaseURL:               "http://localhost",
		MediaTokenSecret:         "local-marketplace-smoke-secret",
		MediaTokenTTL:            time.Hour,
		PresignUploadExpiry:      time.Minute,
		DMInboxTTL:               168 * time.Hour,
		MarketplaceEnabled:       true,
		MarketplaceSweepInterval: time.Hour,
		MarketplaceSweepBatch:    100,
	}
	e := NewRouter(RouterDeps{
		Queries:      q,
		DB:           sqlDB,
		R2:           r2,
		Queue:        queue,
		Cfg:          cfg,
		AuthVerifier: localMarketplaceVerifier{},
	})
	ts := httptest.NewServer(e)
	defer ts.Close()

	cmd := exec.Command("go", "run", "./scripts/smoketest/marketplace")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(),
		"DATABASE_URL="+dsn,
		"MARKETPLACE_API_URL="+ts.URL,
		"MARKETPLACE_SMOKE_SELLER_USER="+sellerID.String(),
		"MARKETPLACE_SMOKE_SELLER_TOKEN="+sellerToken,
		"MARKETPLACE_SMOKE_BUYER_USER="+buyerID.String(),
		"MARKETPLACE_SMOKE_BUYER_TOKEN="+buyerToken,
	)
	out, err := cmd.CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Fatalf("marketplace HTTP smoke: %v", err)
	}

	mh, err := handlers.NewMarketplaceHandler(q, sqlDB, r2, cfg)
	if err != nil {
		t.Fatal(err)
	}
	listing, err := q.CreateMarketplaceListing(ctx, db.CreateMarketplaceListingParams{
		SellerUserID: sellerID, CategorySlug: "other", Title: "Smoke janitor listing",
		Description: "Temporary janitor smoke listing",
		City:        "Smoke City", ItemCondition: "good", PriceIdr: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := sqlDB.ExecContext(ctx, `DELETE FROM marketplace_listings WHERE id = $1`, listing.ID); err != nil {
			t.Errorf("cleanup janitor listing: %v", err)
		}
	}()
	key := "marketplace/smoke/janitor-" + listing.ID.String() + ".png"
	if _, err := q.CreateMarketplaceListingImage(ctx, db.CreateMarketplaceListingImageParams{
		ListingID: listing.ID, R2Key: key, ContentType: "image/png",
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	swept, err := mh.SweepExpiredImages(ctx, 10)
	if err != nil || swept != 1 {
		t.Fatalf("swept expired images = %d, err=%v", swept, err)
	}
	var queued int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM marketplace_cleanup_outbox WHERE r2_keys @> ARRAY[$1]::text[] AND status = 'pending'`, key).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("queued outbox rows for %s = %d, want 1", key, queued)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM marketplace_cleanup_outbox WHERE r2_keys @> ARRAY[$1]::text[]`, key); err != nil {
		t.Fatal(err)
	}
}

func TestStubMarketplaceServer(t *testing.T) {
	if os.Getenv("MARKETPLACE_STUB_SERVER_ENABLED") != "1" {
		t.Skip("MARKETPLACE_STUB_SERVER_ENABLED=1 is required for the stub server")
	}
	addr := os.Getenv("MARKETPLACE_STUB_SERVER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18081"
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required for the stub server")
	}
	ctx := context.Background()
	sqlDB, err := shared.NewSQLDB(ctx, dsn, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	r2, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID: "smoke", AccessKeyID: "smoke", SecretAccessKey: "smoke",
		Bucket: "smoke", Endpoint: "https://example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	queue := asynq.NewClient(asynq.RedisClientOpt{Addr: "127.0.0.1:56379"})
	defer queue.Close()
	cfg := &shared.APIConfig{
		APIBaseURL:               "http://localhost",
		MediaTokenSecret:         "local-marketplace-smoke-secret",
		MediaTokenTTL:            time.Hour,
		PresignUploadExpiry:      time.Minute,
		DMInboxTTL:               168 * time.Hour,
		MarketplaceEnabled:       true,
		MarketplaceSweepInterval: time.Hour,
		MarketplaceSweepBatch:    100,
	}
	e := NewRouter(RouterDeps{
		Queries:      db.New(sqlDB),
		DB:           sqlDB,
		R2:           r2,
		Queue:        queue,
		Cfg:          cfg,
		AuthVerifier: localMarketplaceVerifier{},
	})
	srv := &http.Server{Addr: addr, Handler: e}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	t.Logf("stub marketplace server listening on %s", addr)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
		return
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Error(err)
	}
}
