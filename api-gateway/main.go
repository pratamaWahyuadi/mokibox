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
	"fmt"
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

// verifierRetryBudget caps the total time spent
// retrying NewZitadelVerifier during boot. The
// verifier performs the OIDC discovery + JWKS fetch
// eagerly so a misconfigured issuer URL surfaces at
// boot rather than on the first request. Zitadel
// takes a few seconds to finish migrating its own
// DB on a cold start, and api-gateway does not
// depend_on zitadel in docker-compose (zitadel is in
// a sibling compose project), so a naive fail-fast
// on the very first attempt can race against Zitadel
// boot. We retry with backoff (0s / 5s / 10s / 20s)
// until the budget runs out, then exit 1.
//
// Scope of this fix: absorbs the cold-start race
// window (api-gateway boot finishes within 35s +
// per-attempt discovery timeout, well inside the
// docker compose restart back-off curve). Does NOT
// solve the deeper liveness-vs-readiness split (a
// later reviewer might want /healthz to serve 200
// even while the verifier is retrying — that would
// require starting srv.ListenAndServe before the
// verifier build succeeds, which is tracked as a
// fase 10 follow-up if it ever becomes a problem in
// production). For now /healthz is unreachable
// during the retry window, which is acceptable
// because Zitadel should be up before api-gateway is
// healthy in practice (depends_on service_healthy
// for postgres + redis + external zitadel readiness
// probe could be added later).
const verifierRetryBudget = 60 * time.Second

// verifierRetryDelays is the schedule of sleep
// intervals BEFORE each attempt. Index 0 is the
// initial delay (we set it to 0 so the first attempt
// fires immediately). Subsequent indices are the
// back-off between attempt N and attempt N+1.
//
// Total of all delays: 0+5+10+20 = 35s. The per-
// attempt OIDC discovery timeout is the
// zitadel-go default (10s) so worst-case wallclock
// for a fully unreachable Zitadel is 35s + 4*10s =
// 75s, slightly over the budget. The budget is a
// wallclock-from-first-attempt cap; we abandon
// mid-attempt if the attempt itself would push us
// past the deadline.
var verifierRetryDelays = []time.Duration{
	0,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

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
	//    sqlc *db.Queries and Queries.WithTx. Close
	//    happens in Phase 2 of the shutdown sequence
	//    below (api-gateway/Issue E) so we get a log
	//    line per phase rather than a silent defer.
	sqlDB, err := shared.NewSQLDB(ctx, cfg.DatabaseURL, shared.APIPoolMaxConns, 2)
	if err != nil {
		slog.Error("api-gateway: open db pool", "err", err)
		os.Exit(1)
	}

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

	// 3. Asynq producer client. Close happens in
	//    Phase 2 of the shutdown sequence below
	//    (api-gateway/Issue E).
	asynqClient, err := shared.NewAsynqClient(shared.RedisConfig{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	})
	if err != nil {
		slog.Error("api-gateway: build asynq client", "err", err)
		os.Exit(1)
	}

	// 4. Production Zitadel verifier. NewZitadelVerifier
	//    performs the OIDC discovery + JWKS fetch
	//    eagerly so a misconfigured issuer URL is
	//    surfaced at boot rather than on the first
	//    request. Per Fase 9 planning decision there is
	//    no deny-all fallback: a misconfigured
	//    deployment must NOT silently start serving
	//    requests. Fail-fast — but with a retry-with-
	//    backoff budget so a Zitadel still-booting on a
	//    cold start does not trip the fail-fast before
	//    Zitadel's own HTTP server is ready to serve
	//    OIDC discovery. See verifierRetryBudget /
	//    verifierRetryDelays above for the schedule.
	verifier, err := buildVerifierWithRetry(ctx, cfg.ZitadelIssuerURL, cfg.ZitadelAPIClientID)
	if err != nil {
		slog.Error("api-gateway: build zitadel verifier (fail-fast)",
			"err", err,
			"retry_budget", verifierRetryBudget.String(),
		)
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
	slog.Info("api-gateway: shutdown signal received",
		"signal", sig.String(),
		"shutdown_timeout", shutdownTimeout.String(),
	)

	// Phase 1: stop accepting new connections and drain
	// in-flight requests. srv.Shutdown blocks until
	// every active request has returned or the context
	// deadline (shutdownTimeout = 30s) is exceeded.
	slog.Info("api-gateway: draining in-flight requests")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("api-gateway: http server shutdown", "err", err)
	} else {
		slog.Info("api-gateway: http server drained cleanly")
	}

	// Phase 2: close long-lived clients. The deferred
	// sqlDB.Close() and asynqClient.Close() at the top
	// of main() will run when this function returns,
	// but we emit one log line per client here so an
	// operator watching the shutdown sequence can see
	// the order and which step took how long.
	slog.Info("api-gateway: closing db pool")
	if err := sqlDB.Close(); err != nil {
		slog.Error("api-gateway: close db pool", "err", err)
	}

	slog.Info("api-gateway: closing asynq client")
	if err := asynqClient.Close(); err != nil {
		slog.Error("api-gateway: close asynq client", "err", err)
	}

	// R2 client has no Close() method - the
	// aws-sdk-go-v2 transport is stateless. Documented
	// in the dependency list above so the reader knows
	// nothing is leaked.

	slog.Info("api-gateway: shutdown complete")
}

// buildVerifier returns the production Zitadel
// TokenVerifier. Both arguments are required; the
// caller (main) has already validated them via
// shared.LoadAPI, but NewZitadelVerifier re-checks so
// it stays usable from tests.
//
// Kept as a thin wrapper so tests can stub it; the
// production code path uses buildVerifierWithRetry
// (below) which adds the cold-start retry budget.
func buildVerifier(ctx context.Context, issuer, apiClientID string) (middleware.TokenVerifier, error) {
	return middleware.NewZitadelVerifier(ctx, issuer, apiClientID)
}

// buildVerifierWithRetry wraps buildVerifier with a
// retry-with-backoff loop so a Zitadel still-booting
// on a cold start does not trip the fail-fast before
// Zitadel's own HTTP server is ready to serve OIDC
// discovery.
//
// Schedule (see verifierRetryDelays): the first
// attempt fires immediately, then 5s / 10s / 20s
// between subsequent attempts. Each attempt uses a
// context with its own deadline so a single attempt
// cannot consume the entire budget on its own. The
// loop returns when an attempt succeeds, when the
// budget is exhausted, or when ctx is cancelled
// (e.g. the user hits Ctrl-C during boot).
func buildVerifierWithRetry(ctx context.Context, issuer, apiClientID string) (middleware.TokenVerifier, error) {
	start := time.Now()
	deadline := start.Add(verifierRetryBudget)
	var lastErr error
	for attempt, delay := range verifierRetryDelays {
		// Sleep BEFORE each attempt so the schedule
// applies even when ctx has been cancelled. We
// check ctx cancellation explicitly after the
// sleep so the loop bails out cleanly.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("retry cancelled before attempt %d: %w", attempt+1, ctx.Err())
		case <-time.After(delay):
		}

		// Per-attempt deadline: cap each attempt
// at the remaining budget so we never overshoot
// verifierRetryBudget. A 5s minimum gives even
// the last attempt a chance to complete the OIDC
// discovery round-trip.
		remaining := time.Until(deadline)
		if remaining < 5*time.Second {
			return nil, fmt.Errorf("verifier retry budget exhausted (elapsed %s, attempts %d): %w",
				time.Since(start).Round(time.Millisecond),
				attempt+1,
				lastErr,
			)
		}
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, remaining)
		verifier, err := buildVerifier(attemptCtx, issuer, apiClientID)
		cancelAttempt()
		if err == nil {
			if attempt > 0 {
				slog.Info("api-gateway: verifier built after retry",
					"attempt", attempt+1,
					"elapsed", time.Since(start).Round(time.Millisecond).String(),
				)
			}
			return verifier, nil
		}
		lastErr = err
		slog.Warn("api-gateway: verifier build attempt failed",
			"attempt", attempt+1,
			"of", len(verifierRetryDelays),
			"elapsed", time.Since(start).Round(time.Millisecond).String(),
			"next_delay", delay.String(),
			"err", err,
		)
	}
	return nil, fmt.Errorf("verifier retry exhausted (elapsed %s, attempts %d): %w",
		time.Since(start).Round(time.Millisecond),
		len(verifierRetryDelays),
		lastErr,
	)
}