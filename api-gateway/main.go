// Command api-gateway is the public-facing HTTP service
// for the MokiBox backend. This file is the
// process entry point and is deliberately minimal in
// phase 3: it just constructs the dependencies that
// NewRouter needs and starts the Echo server.
//
// Phase 9 will replace the body of main() with the
// production wiring: env loading via shared.LoadAPI,
// real DB pool, R2 client, Asynq client, Zitadel
// verifier, custom HTTP error handler, request
// validator, graceful shutdown, and signal handling.
// Keeping phase 3's main this short means a missing
// dependency in the dev environment does not block the
// issue-B / issue-C commits.
//
// For the phase-3 smoke test we DO read a few env vars
// directly so a developer can `go run ./api-gateway`
// against a local Postgres + Redis without first
// standing up the full phase-9 main. The set of
// recognised vars is the minimum needed to exercise
// the auth and webhook paths:
//   - API_GATEWAY_ADDR            listen addr (default :8080)
//   - ZITADEL_ISSUER_URL          issuer for the JWT verifier
//   - ZITADEL_API_CLIENT_ID       expected audience
//   - ZITADEL_TARGET_SIGNING_KEY  Actions V2 HMAC secret
//
// If the issuer is empty, we wire a denyAllVerifier so
// a stray /api/users/* request cannot accidentally be
// served as an authenticated user. If the signing key
// is empty, the webhook handler refuses every request
// (defence in depth - see handlers/webhook.go).
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

	// Phase-3 wiring: read the absolute minimum env
	// set needed to exercise /api/users/* and the
	// webhook. Phase 9 replaces this with
	// shared.LoadAPI() and real client construction.
	verifier, err := buildVerifier(
		os.Getenv("ZITADEL_ISSUER_URL"),
		os.Getenv("ZITADEL_API_CLIENT_ID"),
	)
	if err != nil {
		log.Printf("api-gateway: verifier disabled: %v", err)
		verifier = denyAllVerifier{}
	}

	deps := RouterDeps{
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

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
