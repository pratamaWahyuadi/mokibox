// Package handlers - video_detail.go implements the
// read-side video endpoints introduced in phase 6
// (planning/04_api_contracts.md section 3 + LLD
// section 9, issue B):
//
//	GET /api/videos/:id                  - full detail
//	GET /api/videos/:id/status           - processing status (owner only)
//	GET /api/videos/:id/playlist.m3u8    - signed HLS playlist (variant or master)
//
// The 3 endpoints share one struct (VideoHandler in
// video.go) but live in this separate file so the
// upload-intent + confirm pipeline (video.go) stays
// focused on the producer-side path.
//
// All errors are funnelled through shared.RespondError
// so the wire format is consistent. Per the API
// contract asumsi 6, every unauthorized read returns
// 404 (not 403) to prevent resource enumeration -
// even the status endpoint, even the playlist endpoint
// for non-READY videos. The only legitimate 400 is
// a malformed :id UUID.
//
// The playlist endpoint accepts either an
// Authorization: Bearer header (full JWT auth) or a
// ?token=<media-token> query param (anonymous-valid
// for the duration of the token). Per the phase-6
// user clarification, the token path skips the
// visibility check: a valid token means the bearer
// has been pre-authorised (via the feed or detail
// endpoint) for this video.
package handlers

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// hlsVariantCap is the byte size limit applied to a
// single GetObject call when reading an HLS playlist.
// A master.m3u8 is typically <2 KB; a variant
// index.m3u8 is similar. 256 KiB is generous headroom
// for a long-form variant with many #EXT-X-MEDIA
// entries. Anything larger is treated as a corrupt
// object and refused.
const hlsVariantCap int64 = 256 * 1024

// hlsMaxVariant is the highest bitrate suffix the
// player is allowed to request. Anything outside
// this allow-list is rejected as 400 VALIDATION_ERROR
// to bound the number of possible presign URLs.
const hlsMaxVariant = "720p"

// allowedVariants is the set of valid ?variant= values.
var allowedVariants = map[string]struct{}{
	"480p": {},
	"720p": {},
}

// GetVideoDetail returns the full VideoObject for the
// given video id. Visibility rules (per LLD section 9
// + API contract asumsi 6):
//   - owner: any status, no follow check.
//   - non-owner: status must be READY, owner must be
//     active, and if the owner is private the viewer
//     must be a follower.
//   - any unauthorised read collapses to 404 - we do
//     not distinguish "not found" from "forbidden"
//     so an attacker cannot enumerate which video IDs
//     exist (LLD best-practice "UUID != security").
func (h *VideoHandler) GetVideoDetail(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "video handler not configured"))
	}

	videoID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid video id"))
	}

	// Viewer may be nil when the auth middleware is
	// bypassed (only happens on routes mounted
	// outside the auth group, which is not the case
	// for /api/videos/:id). We still treat nil as
	// anonymous for forward-compatibility.
	viewer, _ := middleware.UserFromContext(c)
	viewerID := uuid.Nil
	if viewer != nil {
		viewerID = viewer.ID
	}

	ctx := c.Request().Context()
	row, err := h.Queries.GetVideoDetail(ctx, db.GetVideoDetailParams{
		ID:       videoID,
		ViewerID: uuid.NullUUID{UUID: viewerID, Valid: viewerID != uuid.Nil},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		slog.Error("GetVideoDetail failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video detail"))
	}

	isOwner := viewerID == row.UserID
	if !isOwner {
		// Non-owner: status must be READY. The
		// query joins users but does not project
		// is_active, so re-check via GetUserByID
		// to make sure an inactive owner does
		// not leak the video.
		owner, uerr := h.Queries.GetUserByID(ctx, row.UserID)
		if uerr != nil {
			if errors.Is(uerr, sql.ErrNoRows) {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
			}
			slog.Error("GetUserByID during detail visibility failed", "err", uerr, "user_id", row.UserID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load owner"))
		}
		if !owner.IsActive {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		if row.Status != "READY" {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		if row.UserIsPrivate && viewerID != uuid.Nil {
			// Private + not follower -> 404.
			following, ferr := h.Queries.IsFollowing(ctx, db.IsFollowingParams{
				FollowerID: viewerID,
				FolloweeID: row.UserID,
			})
			if ferr != nil {
				slog.Error("IsFollowing during detail visibility failed", "err", ferr, "viewer", viewerID, "owner", row.UserID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load follow state"))
			}
			if !following {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
			}
		}
	}

	return shared.RespondOK(c, videoObjectFromDetail(ctx, h.R2, h.Cfg, row, isOwner))
}

// videoObjectFromDetail maps a sqlc GetVideoDetailRow
// into the wire VideoObject. The row carries the full
// user join + liked_by_me so no extra queries are
// needed.
func videoObjectFromDetail(ctx context.Context, r2 *shared.R2Client, cfg *shared.APIConfig, r db.GetVideoDetailRow, isOwner bool) VideoObject {
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
		IsOwner:       isOwner,
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
	out.User = &UserSummary{
		ID:        r.UserID2,
		Username:  r.UserUsername,
		IsPrivate: r.UserIsPrivate,
	}
	if r.UserDisplayName.Valid {
		v := r.UserDisplayName.String
		out.User.DisplayName = &v
	}
	if r.UserAvatarUrl.Valid {
		v := r.UserAvatarUrl.String
		out.User.AvatarURL = &v
	}
	return out
}

// videoStatusResponse is the on-the-wire shape of
// /api/videos/:id/status (planning/04_api_contracts.md
// section 3). duration_seconds is null until transcode
// completes and MarkVideoReady stamps the actual
// value.
type videoStatusResponse struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	RetryCount      int    `json:"retry_count"`
	DurationSeconds *int   `json:"duration_seconds"`
}

// GetVideoStatus returns the processing state of the
// video. The endpoint is owner-only; any non-owner
// (and any missing video) returns 404 so the
// existence of a video id cannot be probed by an
// unauthenticated caller.
func (h *VideoHandler) GetVideoStatus(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "video handler not configured"))
	}

	viewer, ok := middleware.UserFromContext(c)
	if !ok || viewer == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	videoID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid video id"))
	}

	row, err := h.Queries.GetVideoByID(c.Request().Context(), videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		slog.Error("GetVideoByID during status failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video status"))
	}
	if row.UserID != viewer.ID {
		// Anti-enumeration: non-owner -> 404, not 403.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
	}

	resp := videoStatusResponse{
		ID:         row.ID.String(),
		Status:     row.Status,
		RetryCount: int(row.RetryCount),
	}
	if row.DurationSeconds.Valid {
		d := int(row.DurationSeconds.Int32)
		resp.DurationSeconds = &d
	}
	return shared.RespondOK(c, resp)
}

// GetPlaylist serves the HLS playlist for a video.
// Two modes:
//
//	GET /api/videos/:id/playlist.m3u8
//	    -> master.m3u8 with each variant URL rewritten
//	       to the API endpoint with a fresh media token.
//	GET /api/videos/:id/playlist.m3u8?variant=480p
//	    -> the 480p variant playlist with each .ts
//	       segment URL replaced by a presigned R2 GET.
//
// Auth: either an Authorization: Bearer JWT (full
// user) or a ?token= media token (anonymous-valid for
// the token TTL). The token path skips the visibility
// check (per phase-6 user clarification): a valid
// token issued for this video is sufficient.
//
// Returns 404 if status != READY, regardless of
// auth - the playlist must not leak before the
// transcode completes.
func (h *VideoHandler) GetPlaylist(c echo.Context) error {
	if h.Queries == nil || h.R2 == nil || h.Cfg == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "video handler not configured"))
	}

	videoID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid video id"))
	}

	ctx := c.Request().Context()

	// Auth dispatch: JWT path -> user from context;
	// token path -> user = nil. Both are valid
	// for owner; for non-owner the visibility
	// check below only runs on the JWT path.
	viewer, _ := middleware.UserFromContext(c)
	tokenPath := viewer == nil
	if tokenPath {
		rawToken := c.QueryParam("token")
		if rawToken == "" {
			return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "missing token"))
		}
		if verr := shared.VerifyMediaToken(rawToken, videoID.String(), h.Cfg.MediaTokenSecret); verr != nil {
			return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "invalid media token"))
		}
	}

	// Fetch the video row (status + hls_prefix + owner).
	row, err := h.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		slog.Error("GetVideoByID during playlist failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	if row.Status != "READY" {
		// Anti-enumeration: a non-READY video must
		// not be probeable via /playlist.m3u8.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not ready"))
	}
	if !row.HlsPrefix.Valid || row.HlsPrefix.String == "" {
		// Data integrity issue: a READY video
		// without hls_prefix. Treat as not found.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not ready"))
	}

	// Visibility check (JWT path only; token path
	// is anonymous-valid for the video).
	if !tokenPath {
		ownerID := row.UserID
		owner, uerr := h.Queries.GetUserByID(ctx, ownerID)
		if uerr != nil {
			if errors.Is(uerr, sql.ErrNoRows) {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
			}
			slog.Error("GetUserByID during playlist visibility failed", "err", uerr, "user_id", ownerID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load owner"))
		}
		if !owner.IsActive {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
		}
		if owner.IsPrivate {
			following, ferr := h.Queries.IsFollowing(ctx, db.IsFollowingParams{
				FollowerID: viewer.ID,
				FolloweeID: ownerID,
			})
			if ferr != nil {
				slog.Error("IsFollowing during playlist visibility failed", "err", ferr)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load follow state"))
			}
			if !following {
				return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "video not found"))
			}
		}
	}

	// Dispatch on ?variant=.
	variant := strings.TrimSpace(c.QueryParam("variant"))
	if variant == "" {
		return h.serveMasterPlaylist(c, row.HlsPrefix.String, videoID)
	}
	if _, ok := allowedVariants[variant]; !ok {
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "unsupported variant").
			WithDetails(shared.FieldError{Field: "variant", Message: "must be one of 480p, 720p"}))
	}
	if variant > hlsMaxVariant {
		// Defensive: if allowedVariants is
		// extended beyond 720p, refuse anything
		// that would exceed the documented max.
		return shared.RespondError(c, shared.NewAPIError(shared.CodeValidationError, "unsupported variant").
			WithDetails(shared.FieldError{Field: "variant", Message: "must be at most " + hlsMaxVariant}))
	}
	return h.serveVariantPlaylist(c, row.HlsPrefix.String, videoID, variant)
}

// serveMasterPlaylist fetches hls_prefix/master.m3u8,
// rewrites each variant URI to the API endpoint with a
// fresh media token, and returns the body with
// Content-Type: application/vnd.apple.mpegurl.
func (h *VideoHandler) serveMasterPlaylist(c echo.Context, hlsPrefix string, videoID uuid.UUID) error {
	ctx := c.Request().Context()
	// hls_prefix is stored WITHOUT a trailing slash (the worker
	// builds it as "hls/<userID>/<videoID>" in transcode.go), so
	// the separator must be added here. Concatenating directly
	// produced "hls/<user>/<videoID>master.m3u8" - a key that
	// never exists - making the playlist endpoint permanently
	// 404. (Pre-existing bug since fase 6, exposed by the Fase
	// 10 integration smoke; see the phase-10.5 commit.)
	masterKey := hlsPrefix + "/master.m3u8"
	body, err := h.R2.GetObject(ctx, masterKey, hlsVariantCap)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "playlist not found"))
		}
		slog.Error("GetObject master.m3u8 failed", "err", err, "key", masterKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load master playlist"))
	}

	rewritten, err := RewriteMasterPlaylist(body, h.Cfg.APIBaseURL, videoID, h.Cfg.MediaTokenSecret, presignTTL(h.Cfg))
	if err != nil {
		slog.Error("rewrite master failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "rewrite master playlist"))
	}
	return c.Blob(http.StatusOK, "application/vnd.apple.mpegurl", rewritten)
}

// serveVariantPlaylist fetches
// hls_prefix/<variant>/index.m3u8, rewrites each .ts
// URI to a presigned R2 URL, and returns the body.
func (h *VideoHandler) serveVariantPlaylist(c echo.Context, hlsPrefix string, videoID uuid.UUID, variant string) error {
	ctx := c.Request().Context()
	// Same trailing-slash contract as serveMasterPlaylist above.
	variantKey := hlsPrefix + "/" + variant + "/index.m3u8"
	body, err := h.R2.GetObject(ctx, variantKey, hlsVariantCap)
	if err != nil {
		if errors.Is(err, shared.ErrNotFound) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "variant playlist not found"))
		}
		slog.Error("GetObject variant playlist failed", "err", err, "key", variantKey)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load variant playlist"))
	}

	rewritten, err := RewriteVariantPlaylist(ctx, h.R2, body, hlsPrefix, variant, presignTTL(h.Cfg))
	if err != nil {
		slog.Error("rewrite variant failed", "err", err, "video_id", videoID, "variant", variant)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "rewrite variant playlist"))
	}
	return c.Blob(http.StatusOK, "application/vnd.apple.mpegurl", rewritten)
}

// RewriteMasterPlaylist scans each non-comment, non-tag
// line of master.m3u8 and, when it is a relative URI
// pointing at a variant (e.g. "480p/index.m3u8" or
// "720p/index.m3u8"), replaces it with a fully
// qualified API URL carrying a fresh media token.
//
// Master.m3u8 lines we care about (HLS RFC 8216):
//   - Lines starting with "#" are comments or tags,
//     passed through unchanged.
//   - URI lines (the lines after #EXT-X-STREAM-INF or
//     #EXT-X-MEDIA) are absolute or relative URLs.
//     We only rewrite the relative ones (anything
//     that does not start with http:// or https://
//     and is not a tag).
//
// The fresh token TTL matches the original so the
// player can fetch the variant with a single token
// round-trip and not be surprised by an earlier
// expiry.
func RewriteMasterPlaylist(body []byte, apiBaseURL string, videoID uuid.UUID, secret string, ttl time.Duration) ([]byte, error) {
	if apiBaseURL == "" || secret == "" {
		return nil, fmt.Errorf("rewriteMasterPlaylist: apiBaseURL or secret empty")
	}
	token, _, err := shared.NewMediaToken(videoID.String(), secret, ttl)
	if err != nil {
		return nil, fmt.Errorf("rewriteMasterPlaylist: sign token: %w", err)
	}
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Allow long lines (HLS playlists can carry long
	// #EXT-X-MEDIA URI attributes).
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		// URI line. If it is already absolute
		// (http(s)://), leave it alone - the
		// master playlist may reference external
		// caption tracks.
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		// Relative - rewrite to API endpoint with
		// a fresh token. Preserve the variant name
		// so "480p/index.m3u8" -> ".../playlist.m3u8?variant=480p".
		variant := strings.SplitN(trimmed, "/", 2)[0]
		newURI := fmt.Sprintf("%s/api/videos/%s/playlist.m3u8?variant=%s&token=%s",
			apiBaseURL, videoID, variant, token)
		out.WriteString(newURI)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rewriteMasterPlaylist: scan: %w", err)
	}
	return out.Bytes(), nil
}

// RewriteVariantPlaylist scans each line of a variant
// index.m3u8 and, when the line is a segment URI
// ending in .ts (or any other non-tag URI), replaces
// it with a presigned R2 GET URL.
//
// We rewrite the absolute path (e.g. segment0.ts) to
// the full R2 key: hls_prefix/<variant>/<segment>.
// The hls_prefix already has a trailing slash, so the
// concatenation is correct.
func RewriteVariantPlaylist(ctx context.Context, r2 *shared.R2Client, body []byte, hlsPrefix, variant string, ttl time.Duration) ([]byte, error) {
	if r2 == nil {
		return nil, fmt.Errorf("RewriteVariantPlaylist: r2 client is nil")
	}
	if !strings.HasSuffix(hlsPrefix, "/") {
		// Defence in depth: the column is set by
		// the worker with a trailing slash, but
		// if a future caller drops it the rewrite
		// would silently break. Refuse.
		return nil, fmt.Errorf("rewriteVariantPlaylist: hls_prefix %q missing trailing slash", hlsPrefix)
	}
	segmentBase := hlsPrefix + variant + "/"
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			// Already absolute - leave alone.
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		// Resolve relative path against the
		// variant base. The master playlist's
		// variant URI is "480p/index.m3u8" so the
		// segment URIs inside are "segment0.ts"
		// (relative to the variant directory).
		key := segmentBase + trimmed
		url, err := r2.PresignGet(ctx, key, ttl)
		if err != nil {
			return nil, fmt.Errorf("rewriteVariantPlaylist: presign %s: %w", key, err)
		}
		out.WriteString(url)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rewriteVariantPlaylist: scan: %w", err)
	}
	return out.Bytes(), nil
}
