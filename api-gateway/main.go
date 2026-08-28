// Command api-gateway is the public-facing HTTP service
// for the MokiBox backend. This file is the
// process entry point and is deliberately minimal in
// phase 3: it just constructs the dependencies that
// NewRouter needs and starts the Echo server.
//
// Phase 9 will replace the body of main() with the
// production wiring: env loading, real DB pool, R2
// client, Asynq client, Zitadel verifier, custom HTTP
// error handler, request validator, graceful shutdown,
// and signal handling. Keeping phase 3's main this
// short means a missing dependency in the dev
// environment does not block the issue-B commit.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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

	// RouterDeps{} is the zero-value config; phase 9
	// will populate it from a real config. Building
	// the router with empty deps succeeds because the
	// user endpoints only use DB / Queries inside the
	// handler (which is not exercised by this minimal
	// main). The auth middleware requires a non-nil
	// Verifier, so we wire a stub here that always
	// rejects - that way an accidentally exposed
	// /api/users/* cannot impersonate a real user.
	//
	// In phase 9 this becomes
	//   middleware.NewZitadelVerifier(ctx, ...)
	// and the rest of RouterDeps is filled from env.
	deps := RouterDeps{
		AuthVerifier: denyAllVerifier{},
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

// denyAllVerifier is the stand-in TokenVerifier used by
// the phase-3 minimal main. It rejects every token so a
// stray /api/users/* request cannot accidentally be
// served. Phase 9 replaces it with
// middleware.NewZitadelVerifier(ctx, cfg.ZitadelIssuerURL, cfg.ZitadelAPIClientID).
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
