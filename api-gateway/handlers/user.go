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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// userStore is the consumer-side interface the
// UserHandler methods (user.go + user_follow.go) use.
// *db.Queries satisfies it.
type userStore interface {
	visibilityStore
	GetUserProfileWithStats(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error)
	UpdateUserProfile(ctx context.Context, arg db.UpdateUserProfileParams) (db.UpdateUserProfileRow, error)
	ListVideosByUser(ctx context.Context, arg db.ListVideosByUserParams) ([]db.Video, error)
	FollowUser(ctx context.Context, arg db.FollowUserParams) error
	GetFollow(ctx context.Context, arg db.GetFollowParams) (db.Follow, error)
	DeleteFollow(ctx context.Context, arg db.DeleteFollowParams) error
	ListFollowers(ctx context.Context, arg db.ListFollowersParams) ([]db.ListFollowersRow, error)
	ListFollowing(ctx context.Context, arg db.ListFollowingParams) ([]db.ListFollowingRow, error)
	InsertNotification(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error)
	UpdateUsername(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error)
	GetUsernameChangedAt(ctx context.Context, userID uuid.UUID) (db.GetUsernameChangedAtRow, error)
}

// UserHandler holds the dependencies for the user
// profile + follow endpoints. Queries is the
// consumer-side interface (testable with stubs); the
// constructor keeps the production concretes so
// routes.go wiring is unchanged.
type UserHandler struct {
	Queries userStore
	R2      r2ObjectStore
	Queue   taskEnqueuer
	Cfg     *shared.APIConfig
}

// NewUserHandler builds a UserHandler from production dependencies.
func NewUserHandler(q *db.Queries, r2 *shared.R2Client, queue *asynq.Client, cfg *shared.APIConfig) (*UserHandler, error) {
	if q == nil {
		return nil, fmt.Errorf("NewUserHandler: queries is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewUserHandler: r2 is nil")
	}
	if queue == nil {
		return nil, fmt.Errorf("NewUserHandler: queue is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewUserHandler: config is nil")
	}
	return &UserHandler{
		Queries: q,
		R2:      r2,
		Queue:   asynqEnqueuer{client: queue},
		Cfg:     cfg,
	}, nil
}

// newUserHandlerForTest lets the test suite build the
// handler directly from the interface.
func newUserHandlerForTest(store userStore) *UserHandler {
	return &UserHandler{Queries: store}
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

// selfUserProfileResponse adds the username-change bookkeeping
// field that MUST only appear on self endpoints (K11):
// GET /api/users/me, PUT /api/users/me and
// PUT /api/users/me/username. The public profile
// (GET /api/users/:id) keeps returning plain
// userProfileResponse, so other viewers never see
// username_changed_at (the cooldown timestamp is private
// state; it also leaks the account's recent username-change
// activity if exposed publicly).
type selfUserProfileResponse struct {
	userProfileResponse
	UsernameChangedAt *string `json:"username_changed_at"`
}

// usernameCooldown is the fixed 7-day cooldown between
// username changes (product decision Q3: hard-coded, not
// configurable via env). The enforcement itself lives in
// the SQL of UpdateUsername using now() from the DATABASE;
// this constant is documentation-only on the Go side.
const usernameCooldown = 7 * 24 * time.Hour

// reservedUsernames are rejected as new usernames (K5,
// final list owned by the repo owner). "user_" is
// deliberately NOT reserved so the existing auto-generated
// usernames (newUsername() in middleware/auth.go) stay
// valid values.
var reservedUsernames = map[string]bool{
	"admin": true, "support": true, "mokibox": true, "api": true,
	"me": true, "system": true, "root": true, "official": true,
	"help": true, "security": true,
}

// changeUsernameRules validates a lowercase-normalized
// username (K5). Programmatic (not struct-tag) so every
// rule is unit-testable. Rules:
//   - 3-30 characters
//   - only a-z, 0-9, dot, underscore
//   - no leading/trailing dot, no consecutive dots
//   - not a reserved word
//   - no "deleted_" prefix (tombstone namespace,
//     TombstoneUser writes username = 'deleted_' || id)
func changeUsernameRules(s string) error {
	if len(s) < 3 || len(s) > 30 {
		return fmt.Errorf("username must be 3-30 characters")
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_') {
			return fmt.Errorf("username may only contain lowercase letters, digits, dot and underscore")
		}
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("username must not start or end with a dot")
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("username must not contain consecutive dots")
	}
	if strings.HasPrefix(s, "deleted_") {
		return fmt.Errorf("username must not start with deleted_")
	}
	if reservedUsernames[s] {
		return fmt.Errorf("username is reserved")
	}
	return nil
}

// userProfileWithStats adds the follow-graph fields that
// only /api/users/:id returns (per API contract section 3).
type userProfileWithStats struct {
	userProfileResponse
	IsFollowing    bool `json:"is_following"`
	FollowerCount  int  `json:"follower_count"`
	FollowingCount int  `json:"following_count"`
}

func userProfileFromDB(ctx context.Context, r2 r2ObjectStore, cfg *shared.APIConfig, u db.User) (userProfileResponse, error) {
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
	avatarURL, err := resolveAvatarURL(ctx, r2, cfg, u.AvatarUrl, u.AvatarKey)
	if err != nil {
		return userProfileResponse{}, err
	}
	out.AvatarURL = avatarURL
	return out, nil
}

// rfc3339 formats t the way every timestamp field in this
// API is serialized.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// selfProfileFromDB builds the self-view profile (K11):
// the shared userProfileResponse plus username_changed_at
// when the user has changed their username before.
func selfProfileFromDB(ctx context.Context, r2 r2ObjectStore, cfg *shared.APIConfig, u db.User) (selfUserProfileResponse, error) {
	base, err := userProfileFromDB(ctx, r2, cfg, u)
	if err != nil {
		return selfUserProfileResponse{}, err
	}
	out := selfUserProfileResponse{userProfileResponse: base}
	if u.UsernameChangedAt.Valid {
		v := rfc3339(u.UsernameChangedAt.Time)
		out.UsernameChangedAt = &v
	}
	return out, nil
}

// GetMe returns the profile of the authenticated user.
// The auth middleware has already placed *db.User on
// the context; we just reserialize it. Self view: includes
// username_changed_at (K11).
func (h *UserHandler) GetMe(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	resp, err := selfProfileFromDB(c.Request().Context(), h.R2, h.Cfg, *user)
	if err != nil {
		slog.Error("presign current user avatar failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load avatar URL"))
	}
	return shared.RespondOK(c, resp)
}

// updateMeRequest is the body of PUT /api/users/me.
// All fields are optional. Per API contract, `username`
// is intentionally absent - it is immutable.
//
// Length bounds (display_name max 100, bio max 500,
// avatar_url max 500) are pragmatic ceilings for the
// single-VPS deployment. Phase 10 issue #31 (security
// hardening) may revisit these against the API
// contract if stricter values are needed; the
// validator/v10 tag-based approach means adjusting
// limits is a one-line struct field change.
type updateMeRequest struct {
	DisplayName *string `json:"display_name" validate:"omitempty,max=100"`
	Bio         *string `json:"bio"          validate:"omitempty,max=500"`
	AvatarURL   *string `json:"avatar_url"   validate:"omitempty,url,max=500"`
	IsPrivate   *bool   `json:"is_private"`
}

// UpdateMe applies the requested profile fields via
// sqlc UpdateUserProfile. Fields left nil in the body
// are skipped by the query (it uses sqlc.narg / COALESCE
// internally) so partial updates are supported.
//
// Field length validation is delegated to
// validator/v10 via the struct tags on
// updateMeRequest. The handler stays focused on
// mapping validated fields to the sqlc query.
func (h *UserHandler) UpdateMe(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req updateMeRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}

	if req.AvatarURL != nil && isPresignedAvatarURLForUser(*req.AvatarURL, user.ID, h.Cfg) {
		req.AvatarURL = nil
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
	if req.AvatarURL != nil {
		if updated.PreviousAvatarKey.Valid && updated.PreviousAvatarKey.String != "" {
			h.enqueueProfileAvatarCleanup(user.ID, updated.PreviousAvatarKey.String)
		}
		if updated.PreviousPendingAvatarKey.Valid && updated.PreviousPendingAvatarKey.String != "" {
			h.enqueueProfileAvatarCleanup(user.ID, updated.PreviousPendingAvatarKey.String)
		}
	}
	updatedUser := db.User{
		ID:                     updated.ID,
		ZitadelID:              updated.ZitadelID,
		Username:               updated.Username,
		DisplayName:            updated.DisplayName,
		Bio:                    updated.Bio,
		AvatarUrl:              updated.AvatarUrl,
		IsPrivate:              updated.IsPrivate,
		IsActive:               updated.IsActive,
		DeletedAt:              updated.DeletedAt,
		CreatedAt:              updated.CreatedAt,
		AvatarKey:              updated.AvatarKey,
		PendingAvatarKey:       updated.PendingAvatarKey,
		PendingAvatarExpiresAt: updated.PendingAvatarExpiresAt,
		UsernameChangedAt:      updated.UsernameChangedAt,
	}
	resp, err := selfProfileFromDB(c.Request().Context(), h.R2, h.Cfg, updatedUser)
	if err != nil {
		slog.Error("presign updated user avatar failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load avatar URL"))
	}
	return shared.RespondOK(c, resp)
}

// changeUsernameRequest is the body of
// PUT /api/users/me/username. Basic bounds run through
// validator/v10; the charset/reserved/tombstone rules run
// programmatically via changeUsernameRules (K5).
type changeUsernameRequest struct {
	Username *string `json:"username" validate:"required,min=3,max=30"`
}

// UpdateUsername implements PUT /api/users/me/username
// (Fitur B): the authenticated user changes their own
// username, at most once per 7 days.
//
// Ordering guarantees:
//   - Same-as-current is rejected BEFORE the store is
//     touched, so the cooldown timestamp is NOT consumed
//     for a no-op change.
//   - The cooldown itself is enforced by the single
//     conditional UPDATE (UpdateUsername) using now() from
//     the DATABASE (K3) - atomic, race-free; RowsAffected
//     decides the outcome, never a read-then-write.
//   - A unique-index violation (SQLSTATE 23505 on
//     uq_users_username_lower) becomes 409 USERNAME_TAKEN
//     (K6b); a zero-row UPDATE becomes either 404 (user
//     missing/inactive) or 429 USERNAME_CHANGE_COOLDOWN
//     with next_change_at + Retry-After computed from DB
//     time (K6). Logs carry user_id only - never the
//     username.
func (h *UserHandler) UpdateUsername(c echo.Context) error {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	var req changeUsernameRequest
	if err := c.Bind(&req); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if err := c.Validate(&req); err != nil {
		return shared.RespondError(c, err)
	}

	newName := strings.ToLower(*req.Username)
	if err := changeUsernameRules(newName); err != nil {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, err.Error()).
			WithDetails(shared.FieldError{Field: "username", Message: err.Error()}))
	}
	// Same-as-current: reject BEFORE the store so the
	// cooldown is not consumed (K item 4b).
	if strings.EqualFold(newName, user.Username) {
		const msg = "username is already yours"
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, msg).
			WithDetails(shared.FieldError{Field: "username", Message: msg}))
	}

	row, err := h.Queries.UpdateUsername(c.Request().Context(), db.UpdateUsernameParams{
		UserID:      user.ID,
		NewUsername: newName,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return shared.RespondError(c, shared.Wrap(shared.ErrUsernameTaken, "username already taken"))
		}
		if errors.Is(err, sql.ErrNoRows) {
			// Zero-row UPDATE: distinguish "missing/inactive"
			// (404) from "still cooling down" (429). The
			// follow-up read is read-only; the write decision
			// stays in the atomic UPDATE (K3).
			probe, gerr := h.Queries.GetUsernameChangedAt(c.Request().Context(), user.ID)
			if gerr != nil {
				if errors.Is(gerr, sql.ErrNoRows) {
					return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
				}
				slog.Error("get username_changed_at failed", "err", gerr, "user_id", user.ID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load username cooldown"))
			}
			if !probe.UsernameChangedAt.Valid || probe.RetryAfterSeconds <= 0 {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
			}
			// 429 USERNAME_CHANGE_COOLDOWN (K6): details carry
			// next_change_at (DB time), header carries
			// Retry-After seconds (DB time).
			c.Response().Header().Set("Retry-After", strconv.FormatInt(probe.RetryAfterSeconds, 10))
			return shared.RespondError(c, shared.NewAPIError(shared.CodeUsernameChangeCooldown,
				"username can only be changed once every 7 days").
				WithDetails(shared.FieldError{
					Field:   "next_change_at",
					Message: rfc3339(probe.NextChangeAt),
				}))
		}
		slog.Error("update username failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update username"))
	}

	// Success: refetch-free response from RETURNING. Only the
	// username/username_changed_at fields changed - rebuild
	// the self profile from the middleware user + the row.
	updated := *user
	updated.Username = row.Username
	updated.UsernameChangedAt = row.UsernameChangedAt
	resp, err := selfProfileFromDB(c.Request().Context(), h.R2, h.Cfg, updated)
	if err != nil {
		slog.Error("presign updated user avatar failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load avatar URL"))
	}
	return shared.RespondOK(c, resp)
}

func isPresignedAvatarURLForUser(rawURL string, userID uuid.UUID, cfg *shared.APIConfig) bool {
	if cfg == nil || cfg.R2Endpoint == "" || cfg.R2Bucket == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	endpoint, err := url.Parse(cfg.R2Endpoint)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") ||
		!strings.EqualFold(parsed.Host, endpoint.Host) {
		return false
	}
	prefix := strings.TrimRight(endpoint.Path, "/") + "/" + cfg.R2Bucket + "/avatars/" + userID.String() + "/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return false
	}
	query := parsed.Query()
	return query.Get("X-Amz-Algorithm") == "AWS4-HMAC-SHA256" &&
		query.Get("X-Amz-Credential") != "" &&
		query.Get("X-Amz-Signature") != ""
}

func (h *UserHandler) enqueueProfileAvatarCleanup(userID uuid.UUID, key string) {
	if h.Queue == nil {
		slog.Error("cannot enqueue replaced avatar cleanup: queue is nil", "user_id", userID, "avatar_key", key)
		return
	}
	if _, err := h.Queue.EnqueueCleanupObjects(shared.CleanupObjectsPayload{Keys: []string{key}}); err != nil {
		slog.Warn("enqueue replaced avatar cleanup failed", "err", err, "user_id", userID, "avatar_key", key)
	}
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
		if errors.Is(err, sql.ErrNoRows) {
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

	profile, err := userProfileFromDB(c.Request().Context(), h.R2, h.Cfg, db.User{
		ID:          row.ID,
		ZitadelID:   row.ZitadelID,
		Username:    row.Username,
		DisplayName: row.DisplayName,
		Bio:         row.Bio,
		AvatarUrl:   row.AvatarUrl,
		AvatarKey:   row.AvatarKey,
		IsPrivate:   row.IsPrivate,
		IsActive:    row.IsActive,
		DeletedAt:   row.DeletedAt,
		CreatedAt:   row.CreatedAt,
	})
	if err != nil {
		slog.Error("presign profile avatar failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load avatar URL"))
	}
	resp := userProfileWithStats{
		userProfileResponse: profile,
		FollowerCount:       int(row.FollowerCount),
		FollowingCount:      int(row.FollowingCount),
		IsFollowing:         row.IsFollowing,
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
		if errors.Is(err, sql.ErrNoRows) {
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
