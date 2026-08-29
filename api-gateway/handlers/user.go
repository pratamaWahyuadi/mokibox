// Package handlers contains the HTTP handlers mounted
// by api-gateway. Each handler is a method on a small
// struct that holds its dependencies; the struct is
// constructed at startup via New* and passed to
// routes.go. Handlers must not hold package-level state.
//
// user.go implements the user profile endpoints defined
// in planning/04_api_contracts.md section 3:
//   - GET  /api/users/me
//   - PUT  /api/users/me
//   - GET  /api/users/:id
//   - GET  /api/users/:id/videos
//
// All endpoints sit behind the auth middleware, so the
// current *db.User is always available via
// middleware.UserFromContext. Errors are funnelled
// through shared.RespondError so the wire format stays
// consistent with the rest of the API.
package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/hibiken/asynq"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// UserHandler holds the dependencies for the user
// profile endpoints. R2 and Queue are listed because
// the spec calls them out; phase 3 does not call them
// directly (thumbnails + notifications arrive in
// phase 6/7) but keeping the field on the struct now
// means phase 6/7 can wire them without touching
// routes.go.
type UserHandler struct {
	DB      *pgxpool.Pool
	Queries *db.Queries
	R2      *shared.R2Client // used from phase 6 for presigned thumbnail URLs
	Queue   *asynq.Client   // used from phase 7 to enqueue notifications
	Cfg     *shared.APIConfig
}

// NewUserHandler builds a UserHandler with all
// dependencies injected. Callers should pass nil only
// for fields they explicitly want to leave unset (phase 3
// leaves R2 and Queue as zero values - the methods
// implemented here do not touch them, so passing nil is
// safe and the strict wiring will be added in phase 9).
func NewUserHandler(pool *pgxpool.Pool, q *db.Queries, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) *UserHandler {
	return &UserHandler{
		DB:      pool,
		Queries: q,
		R2:      r2,
		Queue:   queue,
		Cfg:     cfg,
	}
}

// userProfileResponse is the on-the-wire shape of a user
// profile. It mirrors planning/04_api_contracts.md
// section 2 ("UserProfile") and is shared by GetMe +
// UpdateMe + GetUserProfile.
type userProfileResponse struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
	Bio         *string   `json:"bio"`
	AvatarURL   *string   `json:"avatar_url"`
	IsPrivate   bool      `json:"is_private"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   string    `json:"created_at"`
}

// userProfileWithStats adds the follow-graph fields that
// only /api/users/:id returns (per API contract section 3).
type userProfileWithStats struct {
	userProfileResponse
	IsFollowing    bool `json:"is_following"`
	FollowerCount  int  `json:"follower_count"`
	FollowingCount int  `json:"following_count"`
}

// userFromDB maps a sqlc User row (with its
// sql.NullString fields) into a userProfileResponse.
// The *string JSON fields are nil when the underlying
// value is SQL NULL - which is the spec'd wire shape.
func userFromDB(u db.User) userProfileResponse {
	out := userProfileResponse{
		ID:        u.ID,
		Username:  u.Username,
		IsPrivate: u.IsPrivate,
		IsActive:  u.IsActive,
		CreatedAt: u.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"),
	}
	if u.DisplayName.Valid {
		v := u.DisplayName.String
		out.DisplayName = &v
	}
	if u.Bio.Valid {
		v := u.Bio.String
		out.Bio = &v
	}
	if u.AvatarUrl.Valid {
		v := u.AvatarUrl.String
		out.AvatarURL = &v
	}
	return out
}

// GetMe returns the profile of the authenticated user.
// The auth middleware has already placed *db.User on
// the context; we just reserialize it.
func (h *UserHandler) GetMe(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	return shared.RespondOK(c, userFromDB(*user))
}

// updateMeRequest is the body of PUT /api/users/me.
// All fields are optional. Per API contract, `username`
// is intentionally absent - it is immutable.
type updateMeRequest struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	AvatarURL   *string `json:"avatar_url"`
	IsPrivate   *bool   `json:"is_private"`
}

// UpdateMe applies the requested profile fields via
// sqlc UpdateUserProfile. Fields left nil in the body
// are skipped by the query (it uses sqlc.narg / COALESCE
// internally) so partial updates are supported.
//
// Field length validation is deliberately minimal in
// phase 3 - the spec calls for "all optional" with no
// upper bound. Phase 10 will revisit if a stricter
// validator is added.
func (h *UserHandler) UpdateMe(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req updateMeRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}

	updated, err := h.Queries.UpdateUserProfile(c.Request().Context(), db.UpdateUserProfileParams{
		ID:          user.ID,
		DisplayName: nullStringPtr(req.DisplayName),
		Bio:         nullStringPtr(req.Bio),
		AvatarUrl:   nullStringPtr(req.AvatarURL),
		IsPrivate:   nullBoolPtr(req.IsPrivate),
	})
	if err != nil {
		slog.Error("update user profile failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update profile"))
	}
	return shared.RespondOK(c, userFromDB(updated))
}

// nullStringPtr turns a *string body field into a
// sql.NullString that sqlc's COALESCE will use as
// "leave unchanged" when nil, and write when set.
func nullStringPtr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

// nullBoolPtr is the bool equivalent of nullStringPtr.
func nullBoolPtr(p *bool) sql.NullBool {
	if p == nil {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: *p, Valid: true}
}

// GetUserProfile returns the public profile of the user
// identified by :id, plus the follow-graph stats. If the
// target user does not exist OR is inactive, the handler
// returns 404 NOT_FOUND. Per the API contract and FR-AUTHZ
// ("resource enumeration"), inactive users are not
// distinguishable from non-existent ones for read-side
// callers.
func (h *UserHandler) GetUserProfile(c echo.Context) error {
	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}

	row, err := h.Queries.GetUserProfileWithStats(c.Request().Context(), db.GetUserProfileWithStatsParams{
		ID:         targetID,
		FollowerID: viewer.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("get user profile failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load user profile"))
	}
	if !row.IsActive {
		// Per API contract asumsi 6: inactive users are
		// invisible to other users. Return 404, not 403,
		// so an attacker cannot enumerate which user IDs
		// are deactivated vs. simply absent.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}

	resp := userProfileWithStats{
		userProfileResponse: userFromDB(db.User{
			ID:          row.ID,
			ZitadelID:   row.ZitadelID,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			Bio:         row.Bio,
			AvatarUrl:   row.AvatarUrl,
			IsPrivate:   row.IsPrivate,
			IsActive:    row.IsActive,
			DeletedAt:   row.DeletedAt,
			CreatedAt:   row.CreatedAt,
		}),
		FollowerCount:  int(row.FollowerCount),
		FollowingCount: int(row.FollowingCount),
		IsFollowing:    row.IsFollowing,
	}
	return shared.RespondOK(c, resp)
}

// userVideoItem is the on-the-wire shape of one entry
// in GET /api/users/:id/videos. It is a deliberate
// subset of the full VideoObject defined in the API
// contract:
//
//	- thumbnail_url and hls_playlist_url are nil because
//	  presigned R2 GET + media-token signing is wired in
//	  phase 6 (feed + video detail). The API contract
//	  already says these fields are null for non-READY
//	  videos, so the same wire value works for both
//	  phases.
//	- liked_by_me is also nil for the same reason (phase
//	  7 implements the like flow).
//	- user is nil for the same reason (the contract's
//	  nested UserSummary is filled in by phase 6, which
//	  has the R2 presigner and follow-graph query
//	  available).
//
// Listing this subset keeps phase 3 honest with the
// LLD "JANGAN lebih" rule; phase 6 will replace the
// mapper with the full VideoObject.
type userVideoItem struct {
	ID              uuid.UUID `json:"id"`
	UserID          uuid.UUID `json:"user_id"`
	Title           *string   `json:"title"`
	Description     *string   `json:"description"`
	DurationSeconds *int      `json:"duration_seconds"`
	Status          string    `json:"status"`
	RetryCount      int       `json:"retry_count"`
	LikesCount      int       `json:"likes_count"`
	ViewsCount      int       `json:"views_count"`
	CommentsCount   int       `json:"comments_count"`
	CreatedAt       string    `json:"created_at"`
	ThumbnailURL    *string   `json:"thumbnail_url"`
	HLSPlaylistURL  *string   `json:"hls_playlist_url"`
	LikedByMe       *bool     `json:"liked_by_me"`
	IsOwner         bool      `json:"is_owner"`
}

// videoToItem maps a sqlc Video to the wire subset. nil
// URLs are intentional (see userVideoItem doc).
func videoToItem(v db.Video, isOwner bool) userVideoItem {
	item := userVideoItem{
		ID:          v.ID,
		UserID:      v.UserID,
		Status:      v.Status,
		RetryCount:  int(v.RetryCount),
		LikesCount:  int(v.LikesCount),
		ViewsCount:  int(v.ViewsCount),
		CommentsCount: int(v.CommentsCount),
		CreatedAt:   v.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"),
		IsOwner:     isOwner,
	}
	if v.Title.Valid {
		t := v.Title.String
		item.Title = &t
	}
	if v.Description.Valid {
		d := v.Description.String
		item.Description = &d
	}
	if v.DurationSeconds.Valid {
		d := int(v.DurationSeconds.Int32)
		item.DurationSeconds = &d
	}
	// ThumbnailURL, HLSPlaylistURL, LikedByMe: left nil
	// for phase 3 - phase 6 fills them.
	return item
}

// videoListLimit is the default and max page size for
// /api/users/:id/videos, per the API contract (default
// 20, max 50). Centralised so tests can rely on the
// same constants.
const (
	videoListDefaultLimit = 20
	videoListMaxLimit     = 50
)

// GetUserVideos returns the videos owned by the target
// user. Visibility rules (per API contract section 3 +
// FR-AUTHZ):
//   - target inactive                  -> 404
//   - target private AND viewer is not
//     the owner AND viewer is not a
//     follower                        -> 404
//   - viewer is the owner              -> all statuses
//                                         except DELETED
//   - otherwise                        -> status = READY
//
// Pagination uses (created_at, id) descending via the
// shared.DecodeCursor helper, with limit clamped to
// 1..50.
func (h *UserHandler) GetUserVideos(c echo.Context) error {
	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}

	// Load the target user directly so we can apply the
	// active/private/owner checks before hitting the
	// videos table.
	target, err := h.Queries.GetUserByID(c.Request().Context(), targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("get target user failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load target user"))
	}
	if !target.IsActive {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}

	isOwner := target.ID == viewer.ID
	if !isOwner && target.IsPrivate {
		// Visibility gate for private accounts. A 404 here
		// (not 403) is required to prevent non-followers
		// from enumerating which user IDs are private.
		following, err := h.Queries.IsFollowing(c.Request().Context(), db.IsFollowingParams{
			FollowerID: viewer.ID,
			FolloweeID: target.ID,
		})
		if err != nil {
			slog.Error("IsFollowing check failed", "err", err, "viewer", viewer.ID, "target", target.ID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load follow state"))
		}
		if !following {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
	}

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}

	params := db.ListVideosByUserParams{
		UserID:    target.ID,
		IsOwner:   isOwner,
		PageLimit: int32(limit),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}

	videos, err := h.Queries.ListVideosByUser(c.Request().Context(), params)
	if err != nil {
		slog.Error("list videos by user failed", "err", err, "user_id", target.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load videos"))
	}

	items := make([]userVideoItem, 0, len(videos))
	for _, v := range videos {
		items = append(items, videoToItem(v, isOwner))
	}

	var nextCursor *string
	if len(videos) == limit {
		// Only emit a cursor when we returned a full
		// page. Empty / short pages are guaranteed to be
		// the last one.
		nc := shared.EncodeCursor(videos[len(videos)-1].CreatedAt, videos[len(videos)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// parseLimit clamps the ?limit query param to the
// 1..videoListMaxLimit range, with the spec's default
// of videoListDefaultLimit when absent.
func parseLimit(raw string) (int, error) {
	if raw == "" {
		return videoListDefaultLimit, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, fmt.Errorf("limit must be an integer")
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be > 0")
	}
	if n > videoListMaxLimit {
		n = videoListMaxLimit
	}
	return n, nil
}

// HealthHandler is the response for GET /healthz. It
// is mounted at the router root (outside the auth
// group) and is intentionally trivial - this is the
// only endpoint the orchestrator needs to confirm the
// process is alive. Database and Redis liveness are
// checked by the orchestrator via the dedicated
// `docker compose ps` health checks, not here.
func HealthHandler(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}
