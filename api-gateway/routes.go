// Package api-gateway wires Echo routes for the
// public API. routes.go is the single source of truth
// for which URL paths the gateway exposes and which
// middleware protects them.
//
// Phase 3 issue B (this commit) registers only the
// user profile endpoints:
//   - GET  /healthz                          (no auth)
//   - GET  /api/users/me                     (auth)
//   - PUT  /api/users/me                     (auth)
//   - GET  /api/users/:id                    (auth)
//   - GET  /api/users/:id/videos             (auth)
//
// Phase 3 issue C extends this file with the Zitadel
// webhook route, mounted OUTSIDE the auth group.
// Subsequent phases add their own endpoints here
// (upload-intent, confirm, feed, follow, like, comment,
// notification, delete, etc.). Phase 9 finalises the
// global HTTP error handler, the body validator, and
// the production main.go that calls NewRouter.
package main

import (
	"github.com/labstack/echo/v4"

	"github.com/hibiken/asynq"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// RouterDeps bundles the runtime dependencies that
// NewRouter needs to wire the HTTP layer. Each field
// corresponds to a long-lived client that has already
// been initialised and connected. Keeping the inputs
// flat (rather than taking a single fat struct) means
// main.go in phase 9 can assemble them inline without
// inventing a new config wrapper.
type RouterDeps struct {
	DB      *pgxpool.Pool
	Queries *db.Queries
	R2      *shared.R2Client
	Queue   *asynq.Client
	Cfg     *shared.APIConfig
	// AuthVerifier is the production TokenVerifier built
	// from the Zitadel issuer. Phase 9 will build it in
	// main.go via middleware.NewZitadelVerifier and pass
	// it in here. Tests can pass a stub that satisfies
	// middleware.TokenVerifier.
	AuthVerifier middleware.TokenVerifier
}

// NewRouter returns a fully wired *echo.Echo with the
// phase-3 issue-B routes registered. It does not start
// the server; main.go is responsible for e.Start.
//
// The function does not own the dependencies it
// receives - the caller (main.go) is expected to keep
// the DB pool, Asynq client, etc. alive for the
// lifetime of the process and to close them on
// shutdown.
func NewRouter(d RouterDeps) *echo.Echo {
	e := echo.New()

	// Health probe (no auth, no auth middleware). Kept
	// trivial so the orchestrator's docker healthcheck
	// can hit it cheaply.
	e.GET("/healthz", handlers.HealthHandler)

	// Authenticated API group. Everything under /api/*
	// (except the webhook) requires a valid Zitadel JWT
	// and resolves to a *db.User on the Echo context.
	api := e.Group("/api", middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: d.AuthVerifier,
		Queries:  d.Queries,
	}))

	uh := handlers.NewUserHandler(d.DB, d.Queries, d.R2, d.Queue, d.Cfg)
	api.GET("/users/me", uh.GetMe)
	api.PUT("/users/me", uh.UpdateMe)
	api.GET("/users/:id", uh.GetUserProfile)
	api.GET("/users/:id/videos", uh.GetUserVideos)

	return e
}
