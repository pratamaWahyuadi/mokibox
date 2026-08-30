// Package handlers - video_object.go builds the on-the-wire
// VideoObject shape required by planning/04_api_contracts.md
// section 2 ("VideoObject"). The mapper is shared by the
// feed, video-detail, and (future) social handlers so the
// wire format stays identical regardless of which endpoint
// produced the row.
//
// Every URL on the wire is a presigned/short-lived one:
//   - thumbnail_url: R2 PresignGet(thumbnail_key) with
//     MediaTokenTTL so a feed view can show the thumbnail
//     for the same window as the playlist.
//   - hls_playlist_url: API_BASE_URL/api/videos/<id>/playlist.m3u8?token=<mediaToken>
//     - the token is verified by the same handler that
//       serves the playlist, so a single click in the feed
//       yields a playable stream without a second round
//       trip through the auth middleware.
//
// All fields follow the contract's "READY-only" rule:
// non-READY videos (owner view, processing, failed) return
// null for both URLs and DurationSeconds.
package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// UserSummary is the nested "user" object that lives
// inside VideoObject, CommentObject, and the
// /users/:id/followers + /following responses
// (planning/04_api_contracts.md section 2). The shape is
// intentionally a strict subset of UserProfile - it does
// not expose bio, is_active, or created_at because those
// are not part of the inline user reference. PII and
// presence are kept narrow on purpose.
type UserSummary struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName *string   `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
	IsPrivate   bool      `json:"is_private"`
}

// VideoObject is the on-the-wire shape of one feed /
// detail entry. It mirrors planning/04_api_contracts.md
// section 2 ("VideoObject") exactly. LikedByMe and
// IsOwner are NOT *bool because the contract pins them
// to a concrete type (false, not null, for non-owner /
// not-liked).
type VideoObject struct {
	ID              uuid.UUID    `json:"id"`
	UserID          uuid.UUID    `json:"user_id"`
	Title           *string      `json:"title"`
	Description     *string      `json:"description"`
	DurationSeconds *int         `json:"duration_seconds"`
	Status          string       `json:"status"`
	RetryCount      int          `json:"retry_count"`
	LikesCount      int          `json:"likes_count"`
	ViewsCount      int          `json:"views_count"`
	CommentsCount   int          `json:"comments_count"`
	CreatedAt       string       `json:"created_at"`
	ThumbnailURL    *string      `json:"thumbnail_url"`
	HLSPlaylistURL  *string      `json:"hls_playlist_url"`
	LikedByMe       bool         `json:"liked_by_me"`
	IsOwner         bool         `json:"is_owner"`
	User            *UserSummary `json:"user"`
}

// presignTTL is the expiry applied to both R2 thumbnail
// URLs and the media token embedded in hls_playlist_url.
// Centralised so the two URLs stay aligned - a feed view
// cannot outlive the playlist it links to.
func presignTTL(cfg *shared.APIConfig) time.Duration {
	if cfg != nil && cfg.MediaTokenTTL > 0 {
		return cfg.MediaTokenTTL
	}
	return 15 * time.Minute
}

// formatVideoTime is the canonical RFC3339 UTC wire
// format used by every video timestamp the API returns.
func formatVideoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
}

// userSummaryFromRow builds a UserSummary from the user
// fields sqlc returns in GetVideoDetail / ListFeedVideos.
// A nil receiver would panic; the caller must pass a
// populated row.
func userSummaryFromRow(displayName, avatarURL sql.NullString, id uuid.UUID, username string, isPrivate bool) UserSummary {
	out := UserSummary{
		ID:        id,
		Username:  username,
		IsPrivate: isPrivate,
	}
	if displayName.Valid {
		v := displayName.String
		out.DisplayName = &v
	}
	if avatarURL.Valid {
		v := avatarURL.String
		out.AvatarURL = &v
	}
	return out
}

// buildThumbnailURL returns nil if the video is not
// READY or has no thumbnail_key. R2 presign errors are
// swallowed: a feed that is missing a thumbnail is still
// usable, and a permanent R2 outage should not poison
// the whole page. The contract's thumbnail_url is
// nullable, so "null on error" is the right
// contract-level response.
func buildThumbnailURL(ctx context.Context, r2 *shared.R2Client, cfg *shared.APIConfig, thumbKey sql.NullString, status string) *string {
	if status != "READY" {
		return nil
	}
	if r2 == nil || !thumbKey.Valid || thumbKey.String == "" {
		return nil
	}
	url, err := r2.PresignGet(ctx, thumbKey.String, presignTTL(cfg))
	if err != nil || url == "" {
		return nil
	}
	return &url
}

// buildPlaylistURL signs a fresh media token bound to
// the video so the feed / detail client can request the
// HLS playlist without re-running the JWT middleware.
// Returns nil if the video is not READY (per contract)
// or token signing fails (misconfigured secret, etc.).
func buildPlaylistURL(cfg *shared.APIConfig, videoID uuid.UUID, status string) *string {
	if status != "READY" {
		return nil
	}
	if cfg == nil || cfg.APIBaseURL == "" || cfg.MediaTokenSecret == "" {
		return nil
	}
	token, _, err := shared.NewMediaToken(videoID.String(), cfg.MediaTokenSecret, presignTTL(cfg))
	if err != nil || token == "" {
		return nil
	}
	url := fmt.Sprintf("%s/api/videos/%s/playlist.m3u8?token=%s",
		cfg.APIBaseURL, videoID.String(), token)
	return &url
}
