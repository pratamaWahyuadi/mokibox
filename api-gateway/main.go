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
// Startup order (all steps must succeed before the
// server starts accepting traffic — defence in depth
// per Fase 9 issue A):
//
//  1. Load config from env via shared.LoadAPI. Fails
//     fast on any missing required variable.
//  2. Open the single *sql.DB pool via shared.NewSQLDB.
//     Fails fast on driver registration / dsn parse /
//     ping.
//  3. Build the R2 client via shared.NewR2Client.
//     Fails fast on missing credentials.
//  4. Build the Asynq client via shared.NewAsynqClient.
//     Fails fast on bad Redis config.
//  5. Build the production Zitadel TokenVerifier via
//     middleware.NewZitadelVerifier. The OIDC discovery
//     + JWKS fetch is performed eagerly so a
//     misconfigured issuer is surfaced at boot rather
//     than on the first request. There is intentionally
//     no fallback: a misconfigured deployment MUST NOT
//     silently start serving requests with a
//     deny-all verifier (security risk — see Fase 9
//     planning decision).
//  6. Wire the Echo router via NewRouter and start
//     http.Server.
//  7. On SIGINT/SIGTERM: stop accepting new requests,
//     drain in-flight requests up to a 30s deadline,
//     close the sqlDB pool and the Asynq client.
//
// Required env vars (full list enforced by
// shared.LoadAPI; non-exhaustive summary):
//   - API_GATEWAY_ADDR             listen addr (default :8080)
//   - DATABASE_URL                 Postgres connection string
//   - REDIS_ADDR / REDIS_PASSWORD  Asynq producer connection
//   - R2_*                         Cloudflare R2 credentials
//   - ZITADEL_ISSUER_URL           issuer for the JWT verifier
//   - ZITADEL_API_CLIENT_ID        expected audience
//   - ZITADEL_TARGET_SIGNING_KEY   Actions V2 HMAC secret
//   - API_BASE_URL, MEDIA_TOKEN_*  public URL + media token tuning
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// defaultAddr is the listen address for the API
// gateway. It can be overridden via the API_GATEWAY_ADDR
// env var so docker-compose can map it without code
// changes. The default matches the Nginx upstream
// defined in deploy/nginx/default.conf.
const defaultAddr = ":8080"

// shutdownTimeout caps the time the server spends
// draining in-flight requests after receiving
// SIGINT/SIGTERM. 30s is the standard industry value
// for HTTP services backed by Postgres + Redis — long
// enough to let an upload-intent request (presign +
// DB insert + Redis enqueue) finish, short enough
// that rolling restarts stay snappy.
const shutdownTimeout = 30 * time.Second

func main() {
	// Structured logs (text handler) to stderr so
	// docker compose logs picks them up cleanly. The
	// default slog handler is fine for now; a future
	// phase can switch to JSON for log aggregation.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	addr := os.Getenv("API_GATEWAY_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	cfg, err := shared.LoadAPI()
	if err != nil {
		slog.Error("api-gateway: load config", "err", err)
		os.Exit(1)
	}

	// Background context for long-lived clients. We
	// derive a separate cancellation context for the
	// shutdown sequence below so the DB / Asynq close
	// calls do not race with in-flight requests.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. *sql.DB pool (via pgx stdlib). Used for both
	//    sqlc *db.Queries and Queries.WithTx.
	sqlDB, err := shared.NewSQLDB(ctx, cfg.DatabaseURL, shared.APIPoolMaxConns, 2)
	if err != nil {
		slog.Error("api-gateway: open db pool", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := sqlDB.Close(); err != nil {
			slog.Error("api-gateway: close db pool", "err", err)
		}
	}()

	queries := db.New(sqlDB)

	// 2. R2 client. NewR2Client returns a concrete
	//    *R2Client; the aws-sdk-go-v2 transport is
	//    stateless so there is no Close method to call
	//    on shutdown.
	r2Client, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID:       cfg.R2AccountID,
		AccessKeyID:     cfg.R2AccessKeyID,
		SecretAccessKey: cfg.R2SecretAccessKey,
		Bucket:          cfg.R2Bucket,
		Endpoint:        cfg.R2Endpoint,
	})
	if err != nil {
		slog.Error("api-gateway: build r2 client", "err", err)
		os.Exit(1)
	}

	// 3. Asynq producer client. Closed on shutdown so
	//    any in-flight Enqueue calls return cleanly
	//    rather than hanging on a closed Redis
	//    connection.
	asynqClient, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err != nil {
		slog.Error("api-gateway: build asynq client", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := asynqClient.Close(); err != nil {
			slog.Error("api-gateway: close asynq client", "err", err)
		}
	}()

	// 4. Production Zitadel verifier. NewZitadelVerifier
	//    performs the OIDC discovery + JWKS fetch
	//    eagerly so a misconfigured issuer URL is
	//    surfaced at boot rather than on the first
	//    request. Per Fase 9 planning decision there is
	//    no deny-all fallback: a misconfigured
	//    deployment must NOT silently start serving
	//    requests. Fail-fast.
	verifier, err := buildVerifier(ctx, cfg.ZitadelIssuerURL, cfg.ZitadelAPIClientID)
	if err != nil {
		slog.Error("api-gateway: build zitadel verifier (fail-fast)", "err", err)
		os.Exit(1)
	}

	deps := RouterDeps{
		Queries:           queries,
		DB:                sqlDB,
		R2:                r2Client,
		Queue:             asynqClient,
		Cfg:               cfg,
		AuthVerifier:      verifier,
		WebhookSigningKey: cfg.ZitadelTargetSigningKey,
	}

	e := NewRouter(deps)

	srv := &http.Server{
		Addr:              addr,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run the server in a goroutine so we can wait
	// for SIGINT/SIGTERM and shut down cleanly via
	// http.Server.Shutdown (drains in-flight requests).
	go func() {
		slog.Info("api-gateway listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("api-gateway: listen", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	sig := <-stop
	slog.Info("api-gateway: shutdown signal received", "signal", sig.String())

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("api-gateway: http server shutdown", "err", err)
	}
	slog.Info("api-gateway: shutdown complete")
}

// buildVerifier returns the production Zitadel
// TokenVerifier. Both arguments are required; the
// caller (main) has already validated them via
// shared.LoadAPI, but NewZitadelVerifier re-checks so
// it stays usable from tests.
func buildVerifier(ctx context.Context, issuer, apiClientID string) (middleware.TokenVerifier, error) {
	return middleware.NewZitadelVerifier(ctx, issuer, apiClientID)
}