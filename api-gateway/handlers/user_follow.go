// Package handlers - user_follow.go implements the
// follow endpoints defined in planning/04_api_contracts.md
// section 6 and LLD section 9 (Fase 6, issue C):
//
//	POST   /api/users/:id/follow     - idempotent follow
//	DELETE /api/users/:id/follow     - idempotent unfollow
//	GET    /api/users/:id/followers  - list followers (cursor paginated)
//	GET    /api/users/:id/following  - list following (cursor paginated)
//
// All endpoints sit behind the auth middleware so the
// current *db.User is always available via
// middleware.UserFromContext. Errors are funnelled
// through shared.RespondError so the wire format stays
// consistent.
//
// Visibility on the list endpoints matches the
// GetUserProfile rule: a private account is hidden
// from non-followers, returning 404 (not 403) so
// private users cannot be enumerated.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// followObject is the on-the-wire shape of a single
// follow relationship (planning/04_api_contracts.md
// section 6). It is shared by POST/DELETE follow + the
// followers / following list endpoints, which embed
// the same shape inside { user: {UserSummary} }.
type followObject struct {
	FollowerID  uuid.UUID `json:"follower_id"`
	FolloweeID  uuid.UUID `json:"followee_id"`
	IsFollowing bool      `json:"is_following"`
	CreatedAt   string    `json:"created_at"`
}

// followListItem is the on-the-wire shape of one entry
// in /api/users/:id/followers and /following
// (planning/04_api_contracts.md section 6). It
// mirrors the contract's { user: {UserSummary},
// created_at: ... } exactly.
type followListItem struct {
	User      *UserSummary `json:"user"`
	CreatedAt string       `json:"created_at"`
}

// FollowUser creates a follow row from currentUser
// to :id. Idempotent (FollowUser query uses ON
// CONFLICT DO NOTHING). Inserts a `type=follow`
// notification for the target as a best-effort side
// effect (failure is logged and the success response
// is still returned).
//
// Errors:
//   - self-follow          -> 400 SELF_FOLLOW_NOT_ALLOWED
//   - target missing/inactive -> 404 NOT_FOUND
//   - no auth              -> 401 UNAUTHORIZED
func (h *UserHandler) FollowUser(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "user handler not configured"))
	}
	currentUser, ok := middleware.UserFromContext(c)
	if !ok || currentUser == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}
	if currentUser.ID == targetID {
		// Self-follow is a 400 with the dedicated
		// code, NOT 404 - this is an explicit
		// denial, not an anti-enumeration case.
		return shared.RespondError(c, shared.Wrap(shared.ErrSelfFollow, "cannot follow yourself"))
	}

	ctx := c.Request().Context()
	target, err := h.Queries.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("GetUserByID during follow failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load target user"))
	}
	if !target.IsActive {
		// Anti-enumeration: inactive user -> 404
		// so a non-member cannot probe which
		// accounts have been deactivated.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}

	if err := h.Queries.FollowUser(ctx, db.FollowUserParams{
		FollowerID: currentUser.ID,
		FolloweeID: targetID,
	}); err != nil {
		slog.Error("FollowUser insert failed", "err", err,
			"follower", currentUser.ID, "followee", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "create follow"))
	}

	// Fetch the (now guaranteed) follow row for the
	// wire `created_at`. The INSERT was idempotent;
	// GetFollow returns the original row whether
	// this request inserted it or it already
	// existed.
	follow, ferr := h.Queries.GetFollow(ctx, db.GetFollowParams{
		FollowerID: currentUser.ID,
		FolloweeID: targetID,
	})
	if ferr != nil {
		// Race: row removed between INSERT and
		// SELECT (admin cleanup, etc.). Surface as
		// 500 with the cause logged so the original
		// DB error is preserved.
		slog.Error("GetFollow after FollowUser failed", "err", ferr,
			"follower", currentUser.ID, "followee", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load follow"))
	}

	// Insert the follow notification as a
	// best-effort side effect. Per phase-6 user
	// clarification: the follow itself is the
	// primary action, the notification is a
	// side effect whose failure must not undo
	// the follow.
	if err := h.insertFollowNotification(ctx, target, *currentUser); err != nil {
		slog.Warn("insert follow notification failed", "err", err,
			"target", target.ID, "actor", currentUser.ID)
	}

	return shared.RespondOK(c, followObject{
		FollowerID:  follow.FollowerID,
		FolloweeID:  follow.FolloweeID,
		IsFollowing: true,
		CreatedAt:   formatVideoTime(follow.CreatedAt),
	})
}

// insertFollowNotification writes a notifications row
// with type='follow' and payload={"username": ...}.
// The actor is the user who initiated the follow
// (currentUser); the user_id (target) is the one
// whose inbox receives the notification.
func (h *UserHandler) insertFollowNotification(ctx context.Context, target, actor db.User) error {
	if h.Queries == nil {
		return fmt.Errorf("insertFollowNotification: queries is nil")
	}
	payload, err := json.Marshal(map[string]string{"username": actor.Username})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	_, err = h.Queries.InsertNotification(ctx, db.InsertNotificationParams{
		UserID:  target.ID,
		ActorID: actor.ID,
		Type:    "follow",
		Payload: payload,
	})
	return err
}

// UnfollowUser removes the follow row. Idempotent:
// DeleteFollow returns no error when the row does
// not exist. The wire response always reports
// is_following: false (the contract documents the
// unfollow response as 200 with is_following: false).
func (h *UserHandler) UnfollowUser(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "user handler not configured"))
	}
	currentUser, ok := middleware.UserFromContext(c)
	if !ok || currentUser == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}

	// Unfollow is idempotent so we deliberately do
	// not check self-unfollow; deleting a non
	// existent row is a no-op.
	if err := h.Queries.DeleteFollow(c.Request().Context(), db.DeleteFollowParams{
		FollowerID: currentUser.ID,
		FolloweeID: targetID,
	}); err != nil {
		slog.Error("DeleteFollow failed", "err", err,
			"follower", currentUser.ID, "followee", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "delete follow"))
	}
	return shared.RespondOK(c, followObject{
		FollowerID:  currentUser.ID,
		FolloweeID:  targetID,
		IsFollowing: false,
		CreatedAt:   "", // no row to take the timestamp from
	})
}

// ListFollowers returns the users who follow the
// given :id. Visibility: a private account is
// hidden from non-followers (404, not 403) so
// private users cannot be enumerated. Owner
// always sees their own follower list.
func (h *UserHandler) ListFollowers(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "user handler not configured"))
	}
	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}
	if err := h.assertCanSeeFollowList(c, viewer.ID, targetID); err != nil {
		return err
	}

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}

	params := db.ListFollowersParams{
		FollowerID: viewer.ID, // used for is_following_back
		FolloweeID: targetID,
		PageLimit:  int32(limit),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}

	rows, err := h.Queries.ListFollowers(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListFollowers failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load followers"))
	}

	items := make([]followListItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, followListItem{
			User:      followerListUserSummary(r),
			CreatedAt: formatVideoTime(r.CreatedAt),
		})
	}
	var nextCursor *string
	if len(rows) == limit {
		nc := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// ListFollowing returns the users that :id follows.
// Same visibility rule as ListFollowers.
func (h *UserHandler) ListFollowing(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "user handler not configured"))
	}
	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid user id"))
	}
	if err := h.assertCanSeeFollowList(c, viewer.ID, targetID); err != nil {
		return err
	}

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}

	params := db.ListFollowingParams{
		FollowerID: targetID,
		PageLimit:  int32(limit),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}

	rows, err := h.Queries.ListFollowing(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListFollowing failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load following"))
	}

	items := make([]followListItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, followListItem{
			User:      followerListUserSummaryFromFollowing(r),
			CreatedAt: formatVideoTime(r.CreatedAt),
		})
	}
	var nextCursor *string
	if len(rows) == limit {
		nc := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// assertCanSeeFollowList centralises the visibility
// rule for the follow-list endpoints:
//   - target does not exist              -> 404
//   - target inactive                    -> 404
//   - target private AND viewer != target
//     AND viewer not following           -> 404
//   - otherwise                          -> ok
//
// Returns the first error to be returned via
// shared.RespondError, or nil if the request is
// allowed to proceed.
func (h *UserHandler) assertCanSeeFollowList(c echo.Context, viewerID, targetID uuid.UUID) error {
	target, err := h.Queries.GetUserByID(c.Request().Context(), targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
		}
		slog.Error("GetUserByID during follow-list visibility failed", "err", err, "user_id", targetID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load user"))
	}
	if !target.IsActive {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}
	if targetID == viewerID {
		return nil // owner always sees their own lists
	}
	if !target.IsPrivate {
		return nil // public: anyone can see
	}
	// Private: must be a follower.
	following, ferr := h.Queries.IsFollowing(c.Request().Context(), db.IsFollowingParams{
		FollowerID: viewerID,
		FolloweeID: targetID,
	})
	if ferr != nil {
		slog.Error("IsFollowing during follow-list visibility failed", "err", ferr)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load follow state"))
	}
	if !following {
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "user not found"))
	}
	return nil
}

// followerListUserSummary builds a UserSummary from a
// ListFollowersRow (which is the user that follows the
// target).
func followerListUserSummary(r db.ListFollowersRow) *UserSummary {
	out := UserSummary{
		ID:        r.ID,
		Username:  r.Username,
		IsPrivate: r.IsPrivate,
	}
	if r.DisplayName.Valid {
		v := r.DisplayName.String
		out.DisplayName = &v
	}
	if r.AvatarUrl.Valid {
		v := r.AvatarUrl.String
		out.AvatarURL = &v
	}
	return &out
}

// followerListUserSummaryFromFollowing is the
// ListFollowing counterpart: the row already carries
// the same field set, so the same mapper works.
func followerListUserSummaryFromFollowing(r db.ListFollowingRow) *UserSummary {
	out := UserSummary{
		ID:        r.ID,
		Username:  r.Username,
		IsPrivate: r.IsPrivate,
	}
	if r.DisplayName.Valid {
		v := r.DisplayName.String
		out.DisplayName = &v
	}
	if r.AvatarUrl.Valid {
		v := r.AvatarUrl.String
		out.AvatarURL = &v
	}
	return &out
}
