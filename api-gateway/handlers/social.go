// Package handlers - social.go implements the social
// interaction endpoints from planning/04_api_contracts.md
// section 5/7 and LLD section 10 (Fase 7):
//
//	POST   /api/videos/:id/like      - like a video (idempotent)
//	DELETE /api/videos/:id/like      - unlike a video (idempotent)
//	POST   /api/videos/:id/view      - track a view (no dedup, FR-FEED-05)
//	POST   /api/videos/:id/comments  - create a top-level comment
//	GET    /api/videos/:id/comments  - list comments (cursor paginated)
//	DELETE /api/comments/:id         - delete own comment (subtree)
//	POST   /api/comments/:id/reply   - reply to a comment
//
// Like/unlike and comment create/reply/delete mutate the
// denormalised counters on the videos row, so every write
// runs inside a single *sql.Tx: per LLD section 10 best
// practice, "Like/unlike dan comment count diupdate dalam
// satu transaksi database" - the row insert/delete, the
// counter update, and the notification insert all commit
// together or roll back together.
//
// Visibility: every endpoint that touches a video runs the
// same rule as GetVideoDetail (owner bypass; non-owner
// needs status=READY + active owner + follower for private
// owners). Every unauthorised access collapses to 404
// (anti-enumeration, per API contract asumsi 6).
//
// Sentinel: missing rows surface as sql.ErrNoRows only
// (single *sql.DB pool since the pool-consolidation
// refactor; pgx.ErrNoRows never matches here).
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// commentContentMax bounds comment/reply body length.
// 1..1000 visible characters per LLD section 10 issue B.
const commentContentMax = 1000

// SocialHandler groups the like / view / comment /
// reply endpoints. Dependencies are injected via the
// constructor; DB is the single *sql.DB pool used to
// open transactions (Queries.WithTx binds to the same
// pool).
type SocialHandler struct {
	Queries *db.Queries
	DB      *sql.DB
}

// NewSocialHandler builds a SocialHandler. Both
// dependencies are required: Queries for reads and
// in-tx writes, DB for BeginTx. A nil on either is
// a wiring bug and refuses construction early.
func NewSocialHandler(queries *db.Queries, dbHandle *sql.DB) (*SocialHandler, error) {
	if queries == nil || dbHandle == nil {
		return nil, fmt.Errorf("NewSocialHandler: queries and db must be non-nil")
	}
	return &SocialHandler{Queries: queries, DB: dbHandle}, nil
}

// LikeObject is the wire shape for like/unlike
// (planning/04_api_contracts.md section 5):
// {video_id, liked, likes_count}.
type LikeObject struct {
	VideoID    uuid.UUID `json:"video_id"`
	Liked      bool      `json:"liked"`
	LikesCount int       `json:"likes_count"`
}

// viewObject is the wire shape for the view-tracking
// endpoint: {video_id, views_count}.
type viewObject struct {
	VideoID    uuid.UUID `json:"video_id"`
	ViewsCount int       `json:"views_count"`
}

// assertVideoVisible fetches the video and applies the
// phase-6 detail visibility rule:
//   - owner: any status is visible.
//   - non-owner: status READY + active owner +
//     follower when the owner is private.
//
// Any failure collapses to ErrNotFound (anti-enumeration);
// infrastructure failures surface as ErrInternal. Returns
// the video row on success so callers don't re-fetch.
func (h *SocialHandler) assertVideoVisible(ctx context.Context, viewerID, videoID uuid.UUID) (db.Video, error) {
	video, err := h.Queries.GetVideoByID(ctx, videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
		}
		slog.Error("GetVideoByID during social visibility failed", "err", err, "video_id", videoID)
		return db.Video{}, shared.Wrap(shared.ErrInternal, "load video")
	}
	if video.UserID == viewerID {
		return video, nil // owner bypasses all checks
	}
	if video.Status != "READY" {
		return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
	}
	owner, uerr := h.Queries.GetUserByID(ctx, video.UserID)
	if uerr != nil {
		if errors.Is(uerr, sql.ErrNoRows) {
			return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
		}
		slog.Error("GetUserByID during social visibility failed", "err", uerr, "user_id", video.UserID)
		return db.Video{}, shared.Wrap(shared.ErrInternal, "load owner")
	}
	if !owner.IsActive {
		return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
	}
	if owner.IsPrivate {
		following, ferr := h.Queries.IsFollowing(ctx, db.IsFollowingParams{
			FollowerID: viewerID,
			FolloweeID: video.UserID,
		})
		if ferr != nil {
			slog.Error("IsFollowing during social visibility failed", "err", ferr, "viewer", viewerID, "owner", video.UserID)
			return db.Video{}, shared.Wrap(shared.ErrInternal, "load follow state")
		}
		if !following {
			return db.Video{}, shared.Wrap(shared.ErrNotFound, "video not found")
		}
	}
	return video, nil
}

// beginSocialTx opens a tx and binds a sqlc querier to
// it. On error it returns ErrInternal already wrapped;
// the caller must defer tx.Rollback() (a no-op after
// Commit).
func (h *SocialHandler) beginSocialTx(ctx context.Context) (*sql.Tx, *db.Queries, error) {
	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("social BeginTx failed", "err", err)
		return nil, nil, shared.Wrap(shared.ErrInternal, "begin transaction")
	}
	return tx, h.Queries.WithTx(tx), nil
}

// insertSocialNotification marshals a notification
// payload and inserts one row. Returns nil on success;
// on failure the caller decides whether the error is
// fatal (in-tx, so a non-nil return aborts the whole
// mutation - per LLD the notification is atomic with
// the counter update).
func insertSocialNotification(ctx context.Context, qtx *db.Queries, userID, actorID uuid.UUID, notifType string, payload map[string]string) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal notification payload: %w", err)
	}
	_, err = qtx.InsertNotification(ctx, db.InsertNotificationParams{
		UserID:  userID,
		ActorID: actorID,
		Type:    notifType,
		Payload: raw,
	})
	return err
}

// parseAuthVideoParam resolves the authenticated user
// and the :id video path param, mapping failures to
// 401 / 400. Returns ok=false after writing the error
// response.
func parseAuthVideoParam(c echo.Context) (*db.User, uuid.UUID, bool) {
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		_ = shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
		return nil, uuid.Nil, false
	}
	videoID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		_ = shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid video id"))
		return nil, uuid.Nil, false
	}
	return user, videoID, true
}

// LikeVideo handles POST /api/videos/:id/like.
//
// Flow (LLD section 10, issue A): visibility check ->
// tx{ InsertLike (ON CONFLICT DO NOTHING; a new row
// triggers IncrementLikesCount) -> notification to the
// video owner (skipped on self-like) } -> refetch the
// fresh likes_count for the response.
//
// Idempotent: liking an already-liked video returns
// 200 with the current count and no counter change.
func (h *SocialHandler) LikeVideo(c echo.Context) error {
	if h.Queries == nil || h.DB == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, videoID, ok := parseAuthVideoParam(c)
	if !ok {
		return nil
	}
	ctx := c.Request().Context()

	video, err := h.assertVideoVisible(ctx, user.ID, videoID)
	if err != nil {
		return shared.RespondError(c, err)
	}

	tx, qtx, err := h.beginSocialTx(ctx)
	if err != nil {
		return shared.RespondError(c, err)
	}
	defer func() { _ = tx.Rollback() }()

	// InsertLike returns a row only when this request
	// inserted a NEW like (ON CONFLICT DO NOTHING).
	// sql.ErrNoRows = already liked -> skip the
	// increment + the notification.
	_, lerr := qtx.InsertLike(ctx, db.InsertLikeParams{UserID: user.ID, VideoID: videoID})
	switch {
	case lerr == nil:
		if err := qtx.IncrementLikesCount(ctx, videoID); err != nil {
			slog.Error("IncrementLikesCount failed", "err", err, "video_id", videoID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update likes count"))
		}
		if user.ID != video.UserID { // never notify self-like
			nerr := insertSocialNotification(ctx, qtx, video.UserID, user.ID, "like", map[string]string{
				"username": user.Username,
				"video_id": videoID.String(),
			})
			if nerr != nil {
				slog.Error("insert like notification failed", "err", nerr, "video_id", videoID, "owner", video.UserID)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "insert notification"))
			}
		}
	case errors.Is(lerr, sql.ErrNoRows):
		// Idempotent re-like: nothing to do.
	default:
		slog.Error("InsertLike failed", "err", lerr, "user_id", user.ID, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "insert like"))
	}

	if err := tx.Commit(); err != nil {
		slog.Error("like tx commit failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit like"))
	}

	refreshed, ferr := h.Queries.GetVideoByID(ctx, videoID)
	if ferr != nil {
		slog.Error("GetVideoByID after like failed", "err", ferr, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	return shared.RespondOK(c, LikeObject{
		VideoID:    videoID,
		Liked:      true,
		LikesCount: int(refreshed.LikesCount),
	})
}

// UnlikeVideo handles DELETE /api/videos/:id/like.
// Mirror of LikeVideo: DeleteLike returns a row only
// when a like actually existed; only then is the
// counter decremented. Unliking never notifies.
func (h *SocialHandler) UnlikeVideo(c echo.Context) error {
	if h.Queries == nil || h.DB == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, videoID, ok := parseAuthVideoParam(c)
	if !ok {
		return nil
	}
	ctx := c.Request().Context()

	if _, err := h.assertVideoVisible(ctx, user.ID, videoID); err != nil {
		return shared.RespondError(c, err)
	}

	tx, qtx, err := h.beginSocialTx(ctx)
	if err != nil {
		return shared.RespondError(c, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, derr := qtx.DeleteLike(ctx, db.DeleteLikeParams{UserID: user.ID, VideoID: videoID})
	switch {
	case derr == nil:
		if err := qtx.DecrementLikesCount(ctx, videoID); err != nil {
			slog.Error("DecrementLikesCount failed", "err", err, "video_id", videoID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update likes count"))
		}
	case errors.Is(derr, sql.ErrNoRows):
		// Idempotent unlike: row did not exist.
	default:
		slog.Error("DeleteLike failed", "err", derr, "user_id", user.ID, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "delete like"))
	}

	if err := tx.Commit(); err != nil {
		slog.Error("unlike tx commit failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit unlike"))
	}

	refreshed, ferr := h.Queries.GetVideoByID(ctx, videoID)
	if ferr != nil {
		slog.Error("GetVideoByID after unlike failed", "err", ferr, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	return shared.RespondOK(c, LikeObject{
		VideoID:    videoID,
		Liked:      false,
		LikesCount: int(refreshed.LikesCount),
	})
}

// TrackView handles POST /api/videos/:id/view
// (FR-FEED-05). There is deliberately NO per-user
// dedup: every POST counts one view. The increment is
// a single atomic UPDATE; no tx is needed beyond the
// statement itself.
func (h *SocialHandler) TrackView(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, videoID, ok := parseAuthVideoParam(c)
	if !ok {
		return nil
	}
	ctx := c.Request().Context()

	if _, err := h.assertVideoVisible(ctx, user.ID, videoID); err != nil {
		return shared.RespondError(c, err)
	}
	if err := h.Queries.IncrementViews(ctx, videoID); err != nil {
		slog.Error("IncrementViews failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "track view"))
	}
	refreshed, ferr := h.Queries.GetVideoByID(ctx, videoID)
	if ferr != nil {
		slog.Error("GetVideoByID after view failed", "err", ferr, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load video"))
	}
	return shared.RespondOK(c, viewObject{
		VideoID:    videoID,
		ViewsCount: int(refreshed.ViewsCount),
	})
}
