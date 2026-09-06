// Package handlers - visibility.go is the package-level home
// of the video visibility rule shared by the social, video
// detail, and playlist endpoints.
//
// The rule has TWO deliberately different modes (the
// GetPlaylist trap in the testability refactor review):
//
//   - owner-or-visible (social/detail): the owner bypasses
//     EVERYTHING, including the READY check - an owner may
//     like/comment on their own video while it is still
//     PROCESSING (fase-7 SocialHandler.assertVideoVisible
//     behaviour, moved here verbatim).
//   - ready-visible (playlist): status READY is enforced by
//     the caller BEFORE these helpers run, and there is NO
//     owner bypass on the owner-active/private-following
//     sub-steps: a private owner consuming their own video
//     via JWT fails IsFollowing(self, self) and gets 404.
//     The owner is expected to use the ?token= media-token
//     path instead. This is existing GetPlaylist behaviour,
//     preserved and pinned by unit tests.
//
// Collapsing the two into one helper would silently break
// the anti-enumeration rule, so the difference lives here as
// two explicit entry points sharing the owner-active +
// private-following sub-steps.
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// visibilityStore is the consumer-side read interface the
// shared visibility helpers need: load the video row, load
// the owner user row, and check the follow edge. *db.Queries
// satisfies it, so production wiring is unchanged.
type visibilityStore interface {
	GetVideoByID(ctx context.Context, id uuid.UUID) (db.Video, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error)
	IsFollowing(ctx context.Context, arg db.IsFollowingParams) (bool, error)
}

// assertVideoOwnerOrVisible applies the social/detail
// visibility rule: the owner sees any status; a non-owner
// needs status=READY + an active owner + follower status when
// the owner is private. Any rejection collapses to
// ErrNotFound (anti-enumeration); infrastructure failures
// surface as ErrInternal. Returns the video row on success
// so callers don't re-fetch.
func assertVideoOwnerOrVisible(ctx context.Context, store visibilityStore, viewerID, videoID uuid.UUID) (db.Video, error) {
	video, err := store.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
		}
		slog.Error("GetVideoByID during visibility failed", "err", err, "video_id", videoID)
		return db.Video{}, shared.Wrap(shared.ErrInternal, "load video")
	}
	if video.UserID == viewerID {
		return video, nil // owner bypasses all checks
	}
	if video.Status != "READY" {
		return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
	}
	if err := assertOwnerActiveVisible(ctx, store, video.UserID, viewerID); err != nil {
		return db.Video{}, err
	}
	return video, nil
}

// assertVideoReadyVisible applies the playlist visibility
// mode: NO owner bypass. The status=READY gate itself stays at
// the caller (GetPlaylist needs the row first for the
// hls_prefix data-integrity check anyway and enforces READY
// before this runs); what this helper owns is the JWT-path
// sub-steps. A private owner viewing their own video via JWT
// is treated as a non-follower (IsFollowing(self, self) is
// always false) and gets 404 - the existing behaviour,
// deliberately kept, because playlist serving mints presigned
// R2 URLs and must not leak even to the owner before the
// transcode finishes. Pinned by
// TestVisibility_PlaylistMode_PrivateOwnerJWTNoBypass.
func assertVideoReadyVisible(ctx context.Context, store visibilityStore, ownerID, viewerID uuid.UUID) error {
	return assertOwnerActiveVisible(ctx, store, ownerID, viewerID)
}

// assertOwnerActiveVisible is the sub-step shared by both
// modes: the owner row must exist and be active, and a
// private owner requires the viewer to follow them. All
// rejections collapse to ErrNotFound so callers cannot tell
// a private video from a missing one.
func assertOwnerActiveVisible(ctx context.Context, store visibilityStore, ownerID, viewerID uuid.UUID) error {
	owner, err := store.GetUserByID(ctx, ownerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.Wrap(shared.ErrNotFound, "video not found")
		}
		slog.Error("GetUserID during visibility failed", "err", err, "user_id", ownerID)
		return shared.Wrap(shared.ErrInternal, "load owner")
	}
	if !owner.IsActive {
		return shared.Wrap(shared.ErrNotFound, "video not found")
	}
	if owner.IsPrivate {
		following, ferr := store.IsFollowing(ctx, db.IsFollowingParams{
			FollowerID: viewerID,
			FolloweeID: ownerID,
		})
		if ferr != nil {
			slog.Error("IsFollowing during visibility failed", "err", ferr, "viewer", viewerID, "owner", ownerID)
			return shared.Wrap(shared.ErrInternal, "load follow state")
		}
		if !following {
			return shared.Wrap(shared.ErrNotFound, "video not found")
		}
	}
	return nil
}
