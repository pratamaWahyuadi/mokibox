// Command api-gateway is the public-facing HTTP service
// for the MokiBox backend. This file is the process
// entry point and wires the runtime dependencies
// (config, single *sql.DB pool, R2 client, Asynq
// client, Zitadel verifier) into the Echo router.
//
// A single *sql.DB pool (backed by pgx stdlib) is used
// for both sqlc *db.Queries (read path) and
// Queries.WithTx (transaction path). pgxpool is no
// longer part of this binary.
//
// Phase 9 will wrap this with signal-driven graceful
// shutdown that closes each client; for now we Close()
// the pool on SIGINT/SIGTERM and let the http.Server
// shut down on its own deadline.
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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
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
	// client; for now we Close() the pool on
	// SIGINT/SIGTERM and let the http.Server shut down
	// on its own deadline.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Single *sql.DB pool (via pgx stdlib). Used for
	// both sqlc *db.Queries and Queries.WithTx.
	sqlDB, err := shared.NewSQLDB(ctx, cfg.DatabaseURL, shared.APIPoolMaxConns, 2)
	if err != nil {
		log.Fatalf("api-gateway: NewSQLDB: %v", err)
	}
	defer sqlDB.Close()

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
		Queries:           queries,
		DB:                sqlDB,
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