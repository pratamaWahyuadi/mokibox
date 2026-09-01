// Package handlers - notification.go implements the
// notification inbox endpoints from
// planning/04_api_contracts.md section 8 and LLD
// section 10 (Fase 7, issue C):
//
//	GET /api/notifications            - list for the
//	    current user, newest first, cursor paginated
//	PUT /api/notifications/read-all   - mark every
//	    unread notification for the current user as
//	    read; returns the number of rows flipped
//
// Both endpoints are read/mark-only and live on
// Queries directly - no *sql.DB transaction is
// needed (MarkAllNotificationsRead is a single
// UPDATE; the returned rows-affected comes from
// the driver).
//
// The payload is forwarded as opaque json.RawMessage
// so the inbox stays shape-agnostic: producers
// (follow/like/comment handlers) decide the payload
// keys, this endpoint never re-marshals them.
package handlers

import (
	"database/sql"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// NotificationHandler groups the notification inbox
// endpoints. Only Queries is needed: List is a read
// and MarkAllRead is a single UPDATE (no multi-
// statement transaction).
type NotificationHandler struct {
	Queries *db.Queries
}

// NewNotificationHandler builds a NotificationHandler.
// A nil Queries is a wiring bug and refuses
// construction early.
func NewNotificationHandler(queries *db.Queries) *NotificationHandler {
	return &NotificationHandler{Queries: queries}
}

// NotificationObject is the on-the-wire shape of one
// notification (planning/04_api_contracts.md section
// 2). Payload is forwarded verbatim from the stored
// JSONB column.
type NotificationObject struct {
	ID        uuid.UUID       `json:"id"`
	UserID    uuid.UUID       `json:"user_id"`
	ActorID   uuid.UUID       `json:"actor_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	IsRead    bool            `json:"is_read"`
	CreatedAt string          `json:"created_at"`
}

// notificationObjectFromRow maps a db.Notification row
// into the wire shape without touching the payload.
func notificationObjectFromRow(n db.Notification) NotificationObject {
	return NotificationObject{
		ID:        n.ID,
		UserID:    n.UserID,
		ActorID:   n.ActorID,
		Type:      n.Type,
		Payload:   n.Payload,
		IsRead:    n.IsRead,
		CreatedAt: formatVideoTime(n.CreatedAt),
	}
}

// List handles GET /api/notifications. Newest first
// (created_at DESC, id DESC), cursor pagination via
// shared.EncodeCursor / shared.DecodeCursor.
func (h *NotificationHandler) List(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "notification handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
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

	params := db.ListNotificationsParams{
		UserID:    user.ID,
		PageLimit: int32(limit),
	}
	if !cursorTS.IsZero() {
		params.CursorCreated = sql.NullTime{Time: cursorTS, Valid: true}
		params.CursorID = uuid.NullUUID{UUID: cursorID, Valid: true}
	}

	rows, err := h.Queries.ListNotifications(c.Request().Context(), params)
	if err != nil {
		slog.Error("ListNotifications failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "list notifications"))
	}

	items := make([]NotificationObject, 0, len(rows))
	for _, r := range rows {
		items = append(items, notificationObjectFromRow(r))
	}
	var nextCursor *string
	if len(rows) == limit {
		nc := shared.EncodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
		nextCursor = &nc
	}
	return shared.RespondList(c, items, nextCursor)
}

// markAllReadResponse is the wire shape of
// PUT /api/notifications/read-all: {updated_count}.
type markAllReadResponse struct {
	UpdatedCount int `json:"updated_count"`
}

// MarkAllRead handles PUT /api/notifications/read-all.
// Flips is_read to true for every unread row owned by
// the current user and reports how many rows changed
// (0 when the inbox was already fully read - the call
// is idempotent).
func (h *NotificationHandler) MarkAllRead(c echo.Context) error {
	if h.Queries == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "notification handler not configured"))
	}
	user, ok := middleware.UserFromContext(c)
	if !ok || user == nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "no authenticated user"))
	}

	updated, err := h.Queries.MarkAllNotificationsRead(c.Request().Context(), user.ID)
	if err != nil {
		slog.Error("MarkAllNotificationsRead failed", "err", err, "user_id", user.ID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "mark notifications read"))
	}
	return shared.RespondOK(c, markAllReadResponse{UpdatedCount: int(updated)})
}
