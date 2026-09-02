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
	"strings"
	"time"

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

// CommentObject is the on-the-wire shape of one
// comment (planning/04_api_contracts.md section 2).
// ParentID is null for top-level comments. User is the
// author summary joined from users.
type CommentObject struct {
	ID        uuid.UUID    `json:"id"`
	VideoID   uuid.UUID    `json:"video_id"`
	UserID    uuid.UUID    `json:"user_id"`
	ParentID  *uuid.UUID   `json:"parent_id"`
	Content   string       `json:"content"`
	CreatedAt string       `json:"created_at"`
	User      *UserSummary `json:"user"`
}

// commentRequestBody is the JSON body accepted by
// CreateComment and ReplyComment: {"content": "..."}.
// The validate tag enforces 1..commentContentMax
// characters before the trim, which means a body of
// 1000 spaces would pass — we still TrimSpace in
// bindCommentContent and reject whitespace-only
// results as belt-and-braces.
type commentRequestBody struct {
	Content string `json:"content" validate:"required,min=1,max=1000"`
}

// bindCommentContent decodes and validates the
// {content} body: 1..commentContentMax visible
// characters after trimming whitespace. The
// required/length checks delegate to validator/v10
// via the struct tag on commentRequestBody; this
// helper only adds the post-trim whitespace check
// (validator's `required` does not see through a
// whitespace-only value).
func bindCommentContent(c echo.Context) (string, error) {
	var body commentRequestBody
	if err := c.Bind(&body); err != nil {
		return "", shared.NewAPIError(shared.CodeValidationError, "invalid JSON body").
			WithDetails(shared.FieldError{Field: "content", Message: "body must be JSON"})
	}
	if err := c.Validate(&body); err != nil {
		return "", err
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		return "", shared.NewAPIError(shared.CodeValidationError, "content is required").
			WithDetails(shared.FieldError{Field: "content", Message: "must be 1-1000 non-whitespace characters"})
	}
	return content, nil
}

// commentObjectFromRow maps a joined comment row
// (GetCommentByIDRow / ListCommentsByVideoRow share
// the same projection) into the wire shape.
func commentObjectFromRow(id, videoID, userID uuid.UUID, parentID uuid.NullUUID,
	content string, createdAt time.Time, username string,
	displayName, avatarURL sql.NullString, isPrivate bool,
) CommentObject {
	out := CommentObject{
		ID:        id,
		VideoID:   videoID,
		UserID:    userID,
		Content:   content,
		CreatedAt: formatVideoTime(createdAt),
		User: &UserSummary{
			ID:        userID,
			Username:  username,
			IsPrivate: isPrivate,
		},
	}
	if parentID.Valid {
		v := parentID.UUID
		out.ParentID = &v
	}
	if displayName.Valid {
		v := displayName.String
		out.User.DisplayName = &v
	}
	if avatarURL.Valid {
		v := avatarURL.String
		out.User.AvatarURL = &v
	}
	return out
}

// CreateComment handles POST /api/videos/:id/comments.
//
// tx body (LLD section 10, issue B): InsertComment
// (parent NULL) -> IncrementCommentsCount ->
// notification to the video owner (skipped on
// self-comment). All three commit together.
func (h *SocialHandler) CreateComment(c echo.Context) error {
	if h.Queries == nil || h.DB == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, videoID, ok := parseAuthVideoParam(c)
	if !ok {
		return nil
	}
	content, err := bindCommentContent(c)
	if err != nil {
		return shared.RespondError(c, err)
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

	comment, err := qtx.InsertComment(ctx, db.InsertCommentParams{
		VideoID: videoID,
		UserID:  user.ID,
		Content: content,
	})
	if err != nil {
		slog.Error("InsertComment failed", "err", err, "user_id", user.ID, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "create comment"))
	}
	if err := qtx.IncrementCommentsCount(ctx, videoID); err != nil {
		slog.Error("IncrementCommentsCount failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update comments count"))
	}
	if user.ID != video.UserID { // never notify self-comment
		nerr := insertSocialNotification(ctx, qtx, video.UserID, user.ID, "comment", map[string]string{
			"username":   user.Username,
			"video_id":   videoID.String(),
			"comment_id": comment.ID.String(),
		})
		if nerr != nil {
			slog.Error("insert comment notification failed", "err", nerr, "video_id", videoID)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "insert notification"))
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Error("comment tx commit failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit comment"))
	}

	row, err := h.Queries.GetCommentByID(ctx, comment.ID)
	if err != nil {
		slog.Error("GetCommentByID after insert failed", "err", err, "comment_id", comment.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load comment"))
	}
	return shared.RespondCreated(c, commentObjectFromRow(
		row.ID, row.VideoID, row.UserID, row.ParentID,
		row.Content, row.CreatedAt, row.Username,
		row.DisplayName, row.AvatarUrl, row.IsPrivate,
	))
}

// ListComments handles GET /api/videos/:id/comments.
// Flat list ordered created_at DESC, id DESC with
// cursor pagination. The video visibility rule applies
// (a private video's comment list must not leak).
func (h *SocialHandler) ListComments(c echo.Context) error {
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

	limit, err := parseLimit(c.QueryParam("limit"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, err.Error()))
	}
	cursorTS, cursorID, err := shared.DecodeCursor(c.QueryParam("cursor"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid cursor"))
	}

	params := db.ListCommentsByVideoParams{
		VideoID:   videoID,
		PageLimit: int32(limit),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}
	rows, err := h.Queries.ListCommentsByVideo(ctx, params)
	if err != nil {
		slog.Error("ListCommentsByVideo failed", "err", err, "video_id", videoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "list comments"))
	}

	items := make([]CommentObject, 0, len(rows))
	for _, r := range rows {
		items = append(items, commentObjectFromRow(
			r.ID, r.VideoID, r.UserID, r.ParentID,
			r.Content, r.CreatedAt, r.Username,
			r.DisplayName, r.AvatarUrl, r.IsPrivate,
		))
	}
	var nextCursor *string
	if len(rows) == limit {
		nc := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// DeleteComment handles DELETE /api/comments/:id.
// Owner-only; a non-owner gets 404 (anti-enumeration).
//
// tx body (LLD section 10, issue B): CountCommentSubtree
// (recursive CTE counts this comment + every reply
// underneath) -> DeleteCommentByID (ON DELETE CASCADE
// removes the replies) -> DecrementCommentsCountBy the
// subtree size. The counter and the row deletion commit
// atomically so comments_count can never drift from the
// physical row count on this path.
func (h *SocialHandler) DeleteComment(c echo.Context) error {
	if h.Queries == nil || h.DB == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	commentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid comment id"))
	}
	ctx := c.Request().Context()

	comment, err := h.Queries.GetCommentByID(ctx, commentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "comment not found"))
		}
		slog.Error("GetCommentByID during delete failed", "err", err, "comment_id", commentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load comment"))
	}
	if comment.UserID != user.ID {
		// Anti-enumeration: a non-owner deleting gets
		// the same response as a missing comment.
		return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "comment not found"))
	}

	tx, qtx, err := h.beginSocialTx(ctx)
	if err != nil {
		return shared.RespondError(c, err)
	}
	defer func() { _ = tx.Rollback() }()

	subtree, err := qtx.CountCommentSubtree(ctx, commentID)
	if err != nil {
		slog.Error("CountCommentSubtree failed", "err", err, "comment_id", commentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "count comment subtree"))
	}
	if err := qtx.DeleteCommentByID(ctx, commentID); err != nil {
		slog.Error("DeleteCommentByID failed", "err", err, "comment_id", commentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "delete comment"))
	}
	if subtree > 0 {
		if err := qtx.DecrementCommentsCountBy(ctx, db.DecrementCommentsCountByParams{
			ID:            comment.VideoID,
			CommentsCount: int32(subtree),
		}); err != nil {
			slog.Error("DecrementCommentsCountBy failed", "err", err, "video_id", comment.VideoID, "subtree", subtree)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update comments count"))
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Error("delete comment tx commit failed", "err", err, "comment_id", commentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit delete comment"))
	}
	return shared.RespondNoContent(c)
}

// ReplyComment handles POST /api/comments/:id/reply.
//
// The composite FK (parent_id, video_id) guarantees a
// reply always lands in the same video as its parent.
// Notification fan-out (dedup per user): the video
// owner and the parent comment's author each get a
// type=comment notification, unless the recipient is
// the actor (self-reply) or already received one (owner
// == parent author).
func (h *SocialHandler) ReplyComment(c echo.Context) error {
	if h.Queries == nil || h.DB == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "social handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}
	parentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid comment id"))
	}
	content, err := bindCommentContent(c)
	if err != nil {
		return shared.RespondError(c, err)
	}
	ctx := c.Request().Context()

	parent, err := h.Queries.GetCommentByID(ctx, parentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return shared.RespondError(c, shared.Wrap(shared.ErrNotFound, "comment not found"))
		}
		slog.Error("GetCommentByID during reply failed", "err", err, "comment_id", parentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load parent comment"))
	}

	video, err := h.assertVideoVisible(ctx, user.ID, parent.VideoID)
	if err != nil {
		return shared.RespondError(c, err)
	}

	tx, qtx, err := h.beginSocialTx(ctx)
	if err != nil {
		return shared.RespondError(c, err)
	}
	defer func() { _ = tx.Rollback() }()

	reply, err := qtx.InsertComment(ctx, db.InsertCommentParams{
		VideoID:  parent.VideoID,
		UserID:   user.ID,
		ParentID: uuid.NullUUID{UUID: parentID, Valid: true},
		Content:  content,
	})
	if err != nil {
		slog.Error("InsertComment (reply) failed", "err", err, "parent_id", parentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "create reply"))
	}
	if err := qtx.IncrementCommentsCount(ctx, parent.VideoID); err != nil {
		slog.Error("IncrementCommentsCount (reply) failed", "err", err, "video_id", parent.VideoID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "update comments count"))
	}

	// Notification fan-out with dedup: notify the video
	// owner and the parent comment author, skipping the
	// actor and any recipient already notified.
	recipients := map[uuid.UUID]struct{}{}
	if video.UserID != user.ID {
		recipients[video.UserID] = struct{}{}
	}
	if parent.UserID != user.ID {
		recipients[parent.UserID] = struct{}{}
	}
	payload := map[string]string{
		"username":   user.Username,
		"video_id":   parent.VideoID.String(),
		"comment_id": reply.ID.String(),
	}
	for recipient := range recipients {
		if nerr := insertSocialNotification(ctx, qtx, recipient, user.ID, "comment", payload); nerr != nil {
			slog.Error("insert reply notification failed", "err", nerr, "recipient", recipient)
			return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "insert notification"))
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Error("reply tx commit failed", "err", err, "parent_id", parentID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "commit reply"))
	}

	row, err := h.Queries.GetCommentByID(ctx, reply.ID)
	if err != nil {
		slog.Error("GetCommentByID after reply failed", "err", err, "comment_id", reply.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "load reply"))
	}
	return shared.RespondCreated(c, commentObjectFromRow(
		row.ID, row.VideoID, row.UserID, row.ParentID,
		row.Content, row.CreatedAt, row.Username,
		row.DisplayName, row.AvatarUrl, row.IsPrivate,
	))
}
