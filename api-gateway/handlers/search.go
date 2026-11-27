// Package handlers - search.go implements the GET /api/search endpoint.
// See planning/ROADMAP_TIKTOK_PARITY.md §3 and Issue #57.
package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// searchStore is the consumer-side interface SearchHandler requires.
// *db.Queries satisfies it.
type searchStore interface {
	SearchUsers(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error)
	SearchVideos(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error)
}

// SearchHandler holds dependencies for the search endpoint.
type SearchHandler struct {
	Queries searchStore
	R2      r2ObjectStore
	Cfg     *shared.APIConfig
}

// NewSearchHandler creates a new SearchHandler. Each dependency is required.
func NewSearchHandler(queries *db.Queries, r2 *shared.R2Client, cfg *shared.APIConfig) (*SearchHandler, error) {
	if queries == nil {
		return nil, fmt.Errorf("NewSearchHandler: queries is nil")
	}
	if r2 == nil {
		return nil, fmt.Errorf("NewSearchHandler: r2 is nil")
	}
	if cfg == nil {
		return nil, fmt.Errorf("NewSearchHandler: cfg is nil")
	}
	return &SearchHandler{Queries: queries, R2: r2, Cfg: cfg}, nil
}

// NewSearchHandlerForTest lets tests and smoketests build the handler directly from interfaces.
func NewSearchHandlerForTest(store searchStore, r2 r2ObjectStore, cfg *shared.APIConfig) *SearchHandler {
	return &SearchHandler{Queries: store, R2: r2, Cfg: cfg}
}

type SearchData struct {
	Users  []UserSummary `json:"users"`
	Videos []VideoObject `json:"videos"`
}

type SearchPagination struct {
	UsersNextCursor  *string `json:"users_next_cursor,omitempty"`
	VideosNextCursor *string `json:"videos_next_cursor,omitempty"`
}

type searchEnvelope struct {
	Data       SearchData       `json:"data"`
	Pagination SearchPagination `json:"pagination"`
}

// Search handles GET /api/search?q=<query>&type=<all|users|videos>&limit=<n>&cursor=<c>.
func (h *SearchHandler) Search(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "search handler not configured"))
	}

	viewerID, hasViewer := middleware.UserIDFromContext(c)

	// K8: the response depends on the Authorization header
	// (anonymous vs personal), so it must Vary on it. ADD
	// (not SET) so an existing Vary: Origin from nginx is
	// preserved.
	c.Response().Header().Add(echo.HeaderVary, "Authorization")

	rawQ := c.QueryParam("q")
	q := strings.TrimSpace(rawQ)
	if q == "" {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "query must not be empty").WithDetails(shared.FieldError{Field: "q", Message: "query must not be empty"}))
	}
	if utf8.RuneCountInString(q) > 100 {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "query must not exceed 100 characters").WithDetails(shared.FieldError{Field: "q", Message: "query must not exceed 100 characters"}))
	}

	searchType := strings.ToLower(strings.TrimSpace(c.QueryParam("type")))
	if searchType == "" {
		searchType = "all"
	}
	if searchType != "all" && searchType != "users" && searchType != "videos" {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "invalid search type").WithDetails(shared.FieldError{Field: "type", Message: "type must be one of: all, users, videos"}))
	}

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}

	offset, err := parseOffsetCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}
	// K7 (anti-scraping): anonymous callers are capped at
	// offset 1000; logged-in viewers keep maxOffset.
	if !hasViewer && offset > maxAnonymousOffset {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "cursor offset exceeds anonymous limit").WithDetails(shared.FieldError{Field: "cursor", Message: "offset must be <= 1000 for anonymous callers"}))
	}

	// K2: single viewer-or-anonymous decision point. When
	// no viewer is on the context (AuthenticateOptional
	// passed an anonymous request through) the SQL params
	// carry a NULL viewer: public results only,
	// liked_by_me=false, is_owner=false (K3/K4).
	viewerParams := uuid.NullUUID{UUID: viewerID, Valid: hasViewer}

	ctx := c.Request().Context()
	data := SearchData{
		Users:  make([]UserSummary, 0),
		Videos: make([]VideoObject, 0),
	}
	var pagination SearchPagination

	escapedQ := escapeSQLWildcards(q)

	// Search Users
	if searchType == "all" || searchType == "users" {
		userRows, err := h.Queries.SearchUsers(ctx, db.SearchUsersParams{
			ViewerID:     viewerParams,
			QueryPattern: escapedQ,
			RawQuery:     q,
			PageOffset:   int32(offset),
			PageLimit:    int32(limit + 1),
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Error("SearchUsers failed", "err", err, "viewer_id", viewerLogValue(viewerID, hasViewer), "query", q)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "search users failed"))
		}
		hasNext := len(userRows) > limit
		if hasNext {
			userRows = userRows[:limit]
		}
		for _, r := range userRows {
			user, err := userSummaryFromRow(ctx, h.R2, h.Cfg, r.DisplayName, r.AvatarUrl, r.AvatarKey, r.ID, r.Username, r.IsPrivate)
			if err != nil {
				slog.Error("presign search user avatar failed", "err", err, "user_id", r.ID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load search avatar URLs"))
			}
			data.Users = append(data.Users, user)
		}
		if hasNext {
			nc := encodeOffsetCursor(offset + limit)
			pagination.UsersNextCursor = &nc
		}
	}

	// Search Videos
	if searchType == "all" || searchType == "videos" {
		videoRows, err := h.Queries.SearchVideos(ctx, db.SearchVideosParams{
			ViewerID:     viewerParams,
			QueryPattern: escapedQ,
			PageOffset:   int32(offset),
			PageLimit:    int32(limit + 1),
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			slog.Error("SearchVideos failed", "err", err, "viewer_id", viewerLogValue(viewerID, hasViewer), "query", q)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "search videos failed"))
		}
		hasNext := len(videoRows) > limit
		if hasNext {
			videoRows = videoRows[:limit]
		}
		for _, r := range videoRows {
			vo, err := videoObjectFromSearchRow(ctx, h.R2, h.Cfg, r, viewerID, hasViewer)
			if err != nil {
				slog.Error("presign search video owner avatar failed", "err", err, "user_id", r.UserID2)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load search avatar URLs"))
			}
			data.Videos = append(data.Videos, vo)
		}
		if hasNext {
			nc := encodeOffsetCursor(offset + limit)
			pagination.VideosNextCursor = &nc
		}
	}

	return c.JSON(http.StatusOK, searchEnvelope{
		Data:       data,
		Pagination: pagination,
	})
}

func videoObjectFromSearchRow(ctx context.Context, r2 r2ObjectStore, cfg *shared.APIConfig, r db.SearchVideosRow, viewerID uuid.UUID, hasViewer bool) (VideoObject, error) {
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
		IsOwner:       hasViewer && r.UserID == viewerID,
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
	user, err := userSummaryFromRow(ctx, r2, cfg, r.UserDisplayName, r.UserAvatarUrl, r.UserAvatarKey, r.UserID2, r.UserUsername, r.UserIsPrivate)
	if err != nil {
		return VideoObject{}, err
	}
	out.User = userSummaryPtr(user)
	return out, nil
}

func escapeSQLWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

const maxOffset = 100000

// maxAnonymousOffset caps anonymous pagination depth
// (K7 anti-scraping). Logged-in callers keep maxOffset.
const maxAnonymousOffset = 1000

// viewerLogValue formats the viewer id for logs; anonymous
// requests log "anonymous" instead of the zero UUID.
func viewerLogValue(id uuid.UUID, hasViewer bool) any {
	if !hasViewer {
		return "anonymous"
	}
	return id
}

func parseOffsetCursor(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	// Check base64 encoded offset:
	rawStr := raw
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		rawStr = string(decoded)
	}
	if strings.HasPrefix(rawStr, "offset:") {
		rawStr = strings.TrimPrefix(rawStr, "offset:")
	}
	val, err := strconv.Atoi(rawStr)
	if err != nil || val < 0 || val > maxOffset {
		return 0, fmt.Errorf("invalid cursor format")
	}
	return val, nil
}

func encodeOffsetCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("offset:%d", offset)))
}
