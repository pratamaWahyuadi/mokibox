// Command api-gateway is the public-facing HTTP service
// for the MokiBox backend. This file is the
// process entry point and wires the runtime
// dependencies (config, db pools, R2 client, Asynq
// client, Zitadel verifier) into the Echo router.
//
// Phase 3 left a minimal main that only ran the router
// with a stub TokenVerifier. Phase 4 extends it with
// the dependencies required by the upload-intent +
// confirm handlers:
//   - APIConfig via shared.LoadAPI (DATABASE_URL,
//     REDIS_*, R2_*, MEDIA_TOKEN_*, PRESIGN_UPLOAD_EXPIRY,
//     etc.)
//   - *pgxpool.Pool via shared.NewDB
//   - *sql.DB via database/sql + pgx/v5/stdlib
//     (used by Queries.WithTx in confirm)
//   - *db.Queries via db.New on the *sql.DB
//   - *shared.R2Client via shared.NewR2Client
//   - *asynq.Client via shared.NewAsynqClient
//   - The production Zitadel TokenVerifier via
//     middleware.NewZitadelVerifier when issuer is set;
//     otherwise a denyAllVerifier so a misconfigured
//     deployment cannot accidentally authenticate
//     anyone.
//
// The ZITADEL_* env vars are read directly so a
// developer can `go run ./api-gateway` against a local
// Postgres + Redis + R2 without first standing up the
// full phase-9 production wiring (custom HTTP error
// handler, request validator, graceful shutdown that
// closes every client). Phase 9 owns that work; phase 4
// only adds the deps required to run upload-intent +
// confirm end-to-end.
//
// Env vars:
//   - API_GATEWAY_ADDR             listen addr (default :8080)
//   - ZITADEL_ISSUER_URL           issuer for the JWT verifier
//   - ZITADEL_API_CLIENT_ID        expected audience
//   - ZITADEL_TARGET_SIGNING_KEY   Actions V2 HMAC secret
//   - DATABASE_URL                 Postgres connection string
//     (role tiktok_api)
//   - REDIS_ADDR / REDIS_PASSWORD  Asynq producer connection
//   - R2_*                         Cloudflare R2 credentials
//   - MEDIA_TOKEN_SECRET, MEDIA_TOKEN_TTL,
//     PRESIGN_UPLOAD_EXPIRY        media token + presign tuning
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for pgx
)

// addr is the listen address for the API gateway. It
// can be overridden via the API_GATEWAY_ADDR env var so
// docker-compose can map it without code changes. The
// default matches the Nginx upstream defined in
// deploy/nginx/default.conf.
const defaultAddr = ":8080"

func main() {
	addr := os.Getenv("API_GATEWAY_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	cfg, err := shared.LoadAPI()
	if err != nil {
		log.Fatalf("api-gateway: load config: %v", err)
	}

	// Phase 4 wiring: every dependency NewRouter needs
	// is built here. Phase 9 will wrap this with
	// signal-driven graceful shutdown that closes each
	// client; for phase 4 we Close() the pools on
	// SIGINT/SIGTERM and let the http.Server shut down
	// on its own deadline.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// *pgxpool.Pool for the user handler (queries that
	// do not need a tx).
	pgPool, err := shared.NewDB(ctx, cfg.DatabaseURL, shared.APIPoolMaxConns)
	if err != nil {
		log.Fatalf("api-gateway: NewDB: %v", err)
	}
	defer pgPool.Close()

	// *sql.DB for Queries.WithTx. Opening via
	// pgx/v5/stdlib keeps both pools pointing at the
	// same database / role. A separate pool is
	// intentional so the user handler's ErrNoRows
	// semantics (pgx.ErrNoRows) stay correct.
	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("api-gateway: sql.Open: %v", err)
	}
	sqlDB.SetMaxOpenConns(int(shared.APIPoolMaxConns))
	sqlDB.SetMaxIdleConns(2)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	defer sqlDB.Close()
	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := sqlDB.PingContext(pingCtx); err != nil {
		pingCancel()
		log.Fatalf("api-gateway: sqlDB ping: %v", err)
	}
	pingCancel()

	queries := db.New(sqlDB)

	r2Client, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID:       cfg.R2AccountID,
		AccessKeyID:     cfg.R2AccessKeyID,
		SecretAccessKey: cfg.R2SecretAccessKey,
		Bucket:          cfg.R2Bucket,
		Endpoint:        cfg.R2Endpoint,
	})
	if err != nil {
		log.Fatalf("api-gateway: NewR2Client: %v", err)
	}

	asynqClient, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err != nil {
		log.Fatalf("api-gateway: NewAsynqClient: %v", err)
	}
	defer asynqClient.Close()

	verifier, err := buildVerifier(
		os.Getenv("ZITADEL_ISSUER_URL"),
		os.Getenv("ZITADEL_API_CLIENT_ID"),
	)
	if err != nil {
		log.Printf("api-gateway: verifier disabled: %v", err)
		verifier = denyAllVerifier{}
	}

	deps := RouterDeps{
		DB:                pgPool,
		Queries:           queries,
		SQLDB:             sqlDB,
		R2:                r2Client,
		Queue:             asynqClient,
		Cfg:               cfg,
		AuthVerifier:      verifier,
		WebhookSigningKey: os.Getenv("ZITADEL_TARGET_SIGNING_KEY"),
	}

	e := NewRouter(deps)

	// Start the server in a goroutine so we can wait
	// for SIGINT / SIGTERM and shut down cleanly.
	// Phase 9 extends this with a context-based
	// graceful shutdown (http.Server.Shutdown).
	srv := &http.Server{
		Addr:              addr,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("api-gateway listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down api-gateway")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}

// buildVerifier returns the production Zitadel
// TokenVerifier when both required env vars are set;
// otherwise it returns an error so the caller can
// substitute a denyAllVerifier (a misconfigured
// deployment should never accidentally accept tokens).
func buildVerifier(issuer, apiClientID string) (middleware.TokenVerifier, error) {
	if issuer == "" || apiClientID == "" {
		return nil, errVerifierNotConfigured
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return middleware.NewZitadelVerifier(ctx, issuer, apiClientID)
}

// errVerifierNotConfigured is the sentinel returned
// when the Zitadel env vars are missing.
var errVerifierNotConfigured = errStub("ZITADEL_ISSUER_URL / ZITADEL_API_CLIENT_ID not set")

// denyAllVerifier is the stand-in TokenVerifier used
// when the Zitadel env is not configured. It rejects
// every token so a stray /api/users/* request cannot
// accidentally be served. Phase 9 removes it.
type denyAllVerifier struct{}

// CheckToken always returns an empty subject and an
// error so the auth middleware maps it to 401.
func (denyAllVerifier) CheckToken(_ context.Context, _ string) (string, error) {
	return "", errDenied
}

// errDenied is the sentinel returned by denyAllVerifier.
var errDenied = errStub("auth verifier not configured (phase 3 stub)")

// errStub is a tiny error type so the auth middleware's
// shared.Wrap produces a meaningful 401 message.
type errStub string

func (e errStub) Error() string { return string(e) }