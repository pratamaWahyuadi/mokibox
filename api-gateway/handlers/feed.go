// Package handlers - feed.go implements the home feed
// endpoint defined in planning/04_api_contracts.md
// section 5 and LLD section 9 (Fase 6):
//
//	GET /api/feed/home
//
// Behaviour (per FR-FEED-01):
//   - status=READY
//   - owner active (u.is_active=true)
//   - exclude the viewer's own videos
//   - include public accounts (u.is_private=false) +
//     accounts the viewer follows
//   - cursor pagination over (created_at DESC, id DESC)
//
// The handler is mounted inside the auth group so the
// current *db.User is always available via
// middleware.UserFromContext; a feed without a session
// is not a supported use-case yet (the LLD / contract
// both pin the endpoint behind JWT).
//
// Errors are funnelled through shared.RespondError so
// the wire format stays consistent with the rest of the
// API. No handler ever calls c.JSON directly.
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// FeedHandler holds the dependencies for the home feed
// endpoint. The shape mirrors VideoHandler: a small
// struct with constructor-injected deps, no package-level
// state, defence-in-depth nil checks at method entry.
type FeedHandler struct {
	Queries *db.Queries
	R2      *shared.R2Client
	Cfg     *shared.APIConfig
}

// NewFeedHandler builds a FeedHandler. Each dependency
// is required - a nil Queries, R2, or Cfg would surface
// as a 500 in production and the constructor returns an
// error so the misconfiguration is caught at startup.
func NewFeedHandler(queries *db.Queries, r2 *shared.R2Client, cfg *shared.APIConfig) (*FeedHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewFeedHandler: queries is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewFeedHandler: r2 is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewFeedHandler: cfg is nil")
	}
	return &FeedHandler{Queries: queries, R2: r2, Cfg: cfg}, nil
}

// HomeFeed returns a page of READY videos the viewer is
// allowed to see. See package doc for the full rule set.
//
// Response shape (planning/04_api_contracts.md section 5):
//
//	{
//	  "data": [<VideoObject>, ...],
//	  "pagination": {"next_cursor": "<cursor>|null"}
//	}
//
// Each VideoObject includes:
//   - thumbnail_url: presigned R2 GET (MediaTokenTTL)
//   - hls_playlist_url: API_BASE_URL/.../playlist.m3u8?token=<mediaToken>
//   - liked_by_me: computed in SQL (EXISTS (likes))
//   - is_owner: always false (the feed excludes the
//     viewer's own videos, so the field is fixed).
//   - user: UserSummary of the video owner
func (h *FeedHandler) HomeFeed(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		// Defence in depth: constructor already refuses
		// nil deps, but if a future caller constructs
		// FeedHandler by hand (e.g. a test) we still
		// refuse to silently break the response.
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "feed handler not configured"))
	}

	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}

	params := db.ListFeedVideosParams{
		UserID:    viewer.ID,
		PageLimit: int32(limit),
		ViewerID:  uuid.NullUUID{UUID: viewer.ID, Valid: true},
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}

	ctx := c.Request().Context()
	rows, err := h.Queries.ListFeedVideos(ctx, params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// :many never returns ErrNoRows, but a future
			// sqlc change might; map to empty page rather
			// than a 500 so the client sees a sane envelope.
			return shared.RespondList(c, []VideoObject{}, nil)
		}
		slog.Error("ListFeedVideos failed", "err", err, "viewer_id", viewer.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load feed"))
	}

	items := make([]VideoObject, 0, len(rows))
	for _, r := range rows {
		items = append(items, videoObjectFromFeedRow(ctx, h.R2, h.Cfg, r))
	}

	var nextCursor *string
	if len(rows) == limit {
		// Only emit a cursor on a full page; a short
		// page is by definition the last one.
		nc := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// videoObjectFromFeedRow maps a sqlc ListFeedVideosRow
// into the wire VideoObject. is_owner is always false
// because the feed query excludes the viewer's own
// videos upstream (v.user_id <> $1).
func videoObjectFromFeedRow(ctx context.Context, r2 *shared.R2Client, cfg *shared.APIConfig, r db.ListFeedVideosRow) VideoObject {
	out := VideoObject{
		ID:            r.ID,
		UserID:        r.UserID,
		Status:        r.Status,
		RetryCount:    int(r.RetryCount),
		LikesCount:    int(r.LikesCount),
		ViewsCount:    int(r.ViewsCount),
		CommentsCount: int(r.CommentsCount),
		CreatedAt:     formatVideoTime(r.CreatedAt),
		LikedByMe:     r.LikedByMe,
		IsOwner:       false, // feed excludes self
	}
	if r.Title.Valid {
		v := r.Title.String
		out.Title = &v
	}
	if r.Description.Valid {
		v := r.Description.String
		out.Description = &v
	}
	if r.DurationSeconds.Valid {
		v := int(r.DurationSeconds.Int32)
		out.DurationSeconds = &v
	}
	out.ThumbnailURL = buildThumbnailURL(ctx, r2, cfg, r.ThumbnailKey, r.Status)
	out.HLSPlaylistURL = buildPlaylistURL(cfg, r.ID, r.Status)
	out.User = userSummaryPtr(userSummaryFromRow(r.UserDisplayName, r.UserAvatarUrl, r.UserID2, r.UserUsername, r.UserIsPrivate))
	return out
}

// userSummaryPtr returns a *UserSummary so the JSON
// shape always emits "user": {...} (never null). The
// caller knows the row always carries a user join
// (JOIN users u ON u.id = v.user_id), so the nil case
// cannot happen, but we still return a pointer for
// consistency with future use-sites that may not have
// the join.
func userSummaryPtr(u UserSummary) *UserSummary {
	return &u
}
