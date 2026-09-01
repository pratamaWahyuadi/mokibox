// Package api-gateway wires Echo routes for the
// public API. routes.go is the single source of truth
// for which URL paths the gateway exposes and which
// middleware protects them.
//
// Phase 3 issue B (commit phase-3.2) registered the
// user profile endpoints; phase 3 issue C extended
// routes.go with the Zitadel Actions V2 webhook
// (mounted OUTSIDE the auth group). Phase 4 added the
// upload-intent and confirm endpoints inside the auth
// group. Phase 6 issue A added the home feed behind
// the auth group; issue B added the video detail /
// status / playlist endpoints; issue C added the
// follow endpoints. The current full route set is:
//   - GET  /healthz                          (no auth)
//   - POST /api/webhooks/zitadel             (no JWT auth;
//       signature verified inside the handler)
//   - GET  /api/users/me                     (auth)
//   - PUT  /api/users/me                     (auth)
//   - GET  /api/users/:id                    (auth)
//   - GET  /api/users/:id/videos             (auth)
//   - POST /api/users/:id/follow             (auth)
//   - DELETE /api/users/:id/follow           (auth)
//   - GET  /api/users/:id/followers          (auth)
//   - GET  /api/users/:id/following          (auth)
//   - GET  /api/feed/home                    (auth)
//   - POST /api/videos/upload-intent         (auth)
//   - POST /api/videos/confirm               (auth)
//   - GET  /api/videos/:id                   (auth)
//   - GET  /api/videos/:id/status            (auth)
//   - GET  /api/videos/:id/playlist.m3u8     (auth+token)
//
// Phase 9 finalises the global HTTP error handler, the
// body validator, and the production main.go that
// calls NewRouter.
package main

import (
	"database/sql"
	"fmt"

	"github.com/labstack/echo/v4"

	"github.com/hibiken/asynq"

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
//
// DB is the single *sql.DB pool (via pgx stdlib);
// sufficient for both sqlc Queries and Queries.WithTx.
type RouterDeps struct {
	Queries *db.Queries
	DB      *sql.DB
	R2      *shared.R2Client
	Queue   *asynq.Client
	Cfg     *shared.APIConfig
	// AuthVerifier is the production TokenVerifier built
	// from the Zitadel issuer. Phase 9 will build it in
	// main.go via middleware.NewZitadelVerifier and pass
	// it in here. Tests can pass a stub that satisfies
	// middleware.TokenVerifier.
	AuthVerifier middleware.TokenVerifier
	// WebhookSigningKey is the ZITADEL_TARGET_SIGNING_KEY
	// used to verify the HMAC on every Actions V2 call.
	// Kept as a separate field (not buried in Cfg) so
	// tests can inject a fixed key without depending on
	// the env loader.
	WebhookSigningKey string
}

// NewRouter returns a fully wired *echo.Echo with the
// phase-3 routes registered. It does not start the
// server; main.go is responsible for e.Start.
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

	// Webhook: mounted on the root Echo (NOT under the
	// JWT auth group) because Actions V2 authenticates
	// the call via the ZITADEL-Signature HMAC header
	// rather than a Bearer token. Rate limiting lives
	// in phase 9 (middleware.RateLimitWebhook) - the
	// route is registered without it now so a future
	// commit can insert the middleware without touching
	// this file.
	wh := handlers.NewWebhookHandler(d.Queries, d.WebhookSigningKey)
	e.POST("/api/webhooks/zitadel", wh.Handle)

	// Authenticated API group. Everything under /api/*
	// (except the webhook) requires a valid Zitadel JWT
	// and resolves to a *db.User on the Echo context.
	api := e.Group("/api", middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: d.AuthVerifier,
		Queries:  d.Queries,
	}))

	uh := handlers.NewUserHandler(d.Queries, d.R2, d.Queue, d.Cfg)
	api.GET("/users/me", uh.GetMe)
	api.PUT("/users/me", uh.UpdateMe)
	api.GET("/users/:id", uh.GetUserProfile)
	api.GET("/users/:id/videos", uh.GetUserVideos)

	// Phase 6.C: follow endpoints. POST/DELETE are
	// idempotent (FollowUser is ON CONFLICT DO
	// NOTHING; DeleteFollow silently no-ops). The
	// list endpoints honour the private-account
	// visibility rule (404, not 403).
	api.POST("/users/:id/follow", uh.FollowUser)
	api.DELETE("/users/:id/follow", uh.UnfollowUser)
	api.GET("/users/:id/followers", uh.ListFollowers)
	api.GET("/users/:id/following", uh.ListFollowing)

	// Phase 4: upload-intent + confirm. Both sit
	// inside the auth group so the *db.User on the
	// context is always populated. Constructor returns
	// an error if any dependency is missing; the
	// startup of main.go is responsible for surfacing
	// that error before the server begins accepting
	// traffic.
	vh, err := handlers.NewVideoHandler(d.Queries, d.DB, d.R2, d.Queue, d.Cfg)
	if err != nil {
		// We cannot return an error from NewRouter
		// (its signature is fixed by phase 3) so the
		// misconfiguration is surfaced via echo's
		// panic-on-startup path: log and let main.go
		// handle the missing-route by panicking before
		// serving. In practice main.go checks these
		// deps before calling NewRouter so the panic
		// here is unreachable but kept defensive.
		panic(fmt.Sprintf("api-gateway: NewVideoHandler: %v", err))
	}
	api.POST("/videos/upload-intent", vh.UploadIntent)
	api.POST("/videos/confirm", vh.ConfirmUpload)

	// Phase 6.A: home feed. Same auth-group placement
	// as the rest of /api/*. The handler builds the
	// full VideoObject (presigned thumbnail, signed
	// playlist URL, liked_by_me, user summary) so the
	// client gets a ready-to-render page in a single
	// request - no N+1 follow-up round trips.
	fh, err := handlers.NewFeedHandler(d.Queries, d.R2, d.Cfg)
	if err != nil {
		panic(fmt.Sprintf("api-gateway: NewFeedHandler: %v", err))
	}
	api.GET("/feed/home", fh.HomeFeed)

	// Phase 6.B: video read endpoints. All three
	// share the same VideoHandler from video.go.
	// - /videos/:id               - full VideoObject
	//                              (auth, visibility
	//                              rules per LLD)
	// - /videos/:id/status        - processing status
	//                              (owner only)
	// - /videos/:id/playlist.m3u8 - signed HLS playlist
	//                              (auth OR media
	//                              token; returns raw
	//                              application/vnd.apple.mpegurl,
	//                              NOT the JSON envelope)
	api.GET("/videos/:id", vh.GetVideoDetail)
	api.GET("/videos/:id/status", vh.GetVideoStatus)
	api.GET("/videos/:id/playlist.m3u8", vh.GetPlaylist)

	// Phase 7.A: like / unlike / view tracking.
	// Like and unlike mutate the denormalised
	// likes_count inside a tx (insert/delete +
	// counter + notification commit together).
	// Both are idempotent and return the current
	// counter. View tracking is a single atomic
	// increment with no per-user dedup
	// (FR-FEED-05). All three enforce the video
	// visibility rule (404 on unauthorised).
	sh, err := handlers.NewSocialHandler(d.Queries, d.DB)
	if err != nil {
		panic(fmt.Sprintf("api-gateway: NewSocialHandler: %v", err))
	}
	api.POST("/videos/:id/like", sh.LikeVideo)
	api.DELETE("/videos/:id/like", sh.UnlikeVideo)
	api.POST("/videos/:id/view", sh.TrackView)

	// Phase 7.B: comments. Create + reply mutate
	// comments_count inside a tx; delete removes the
	// whole subtree and decrements by the recursive
	// count. The list endpoint applies the same video
	// visibility rule so a private video's comments
	// never leak (404 on unauthorised).
	api.POST("/videos/:id/comments", sh.CreateComment)
	api.GET("/videos/:id/comments", sh.ListComments)
	api.DELETE("/comments/:id", sh.DeleteComment)
	api.POST("/comments/:id/reply", sh.ReplyComment)

	// Phase 7.C: notification inbox. Read-only + a
	// single UPDATE, so the handler only needs
	// Queries (no *sql.DB). Payload is forwarded as
	// opaque JSON produced by the follow/like/comment
	// paths; this endpoint never inspects it.
	nh := handlers.NewNotificationHandler(d.Queries)
	api.GET("/notifications", nh.List)
	api.PUT("/notifications/read-all", nh.MarkAllRead)

	return e
}