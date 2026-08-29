// Package handlers - webhook.go implements
// POST /api/webhooks/zitadel, the endpoint Zitadel
// calls as an Actions V2 Target for user-lifecycle
// events (user.deactivated, user.removed, etc.).
//
// Security model:
//   - The endpoint is mounted OUTSIDE the JWT auth
//     middleware (see api-gateway/routes.go). It is
//     authenticated entirely by the ZITADEL-Signature
//     HMAC header on each request, verified against
//     ZITADEL_TARGET_SIGNING_KEY using the official
//     zitadel-go helper actions.ValidateRequestPayload.
//     We never re-implement HMAC by hand - per SEC-05
//     and the zitadel skill's critical note that the
//     helper is the only safe way to keep dual-header
//     + timestamp tolerance consistent with Zitadel.
//   - Rate limiting is phase 9's
//     middleware.RateLimitWebhook. Phase 3 leaves the
//     slot open so a future commit can add it without
//     touching this file.
//
// Wire format accepted (Actions V2):
//   - aggregateID, aggregateType, resourceOwner,
//     instanceID, version, sequence
//   - event_type: machine identifier of the event
//   - created_at: RFC3339 timestamp
//   - userID: Zitadel subject of the affected user
//     (NOTE: the field is `userID` not `user_id`; this
//     matches the spec'd Actions V2 payload from
//     planning/04_api_contracts.md section 0.1)
//   - event_payload: object whose shape depends on
//     event_type (unused for the events we handle)
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/zitadel/zitadel-go/v3/pkg/actions"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// Event types this handler knows how to process. Any
// other event_type is mapped to WEBHOOK_EVENT_UNSUPPORTED
// so an accidentally-misconfigured Execution does not
// silently succeed.
const (
	EventUserDeactivated = "user.deactivated"
	EventUserRemoved     = "user.removed"
)

// WebhookHandler holds the dependencies the
// /api/webhooks/zitadel endpoint needs. Phase 3 only
// uses Queries to call DeactivateUser. R2 / Queue are
// not consumed here - phase 8 (delete account) will
// pull in DeleteUserData, which itself needs R2
// cleanup and the Asynq client. Keeping the field
// on the struct now means phase 8 can add them
// without changing the constructor signature.
type WebhookHandler struct {
	Queries    *db.Queries
	SigningKey string
}

// NewWebhookHandler returns a WebhookHandler with the
// given dependencies. signingKey is the
// ZITADEL_TARGET_SIGNING_KEY; an empty value means
// signature verification is impossible and the handler
// will reject every request (defence in depth - the
// env loader also rejects an empty value at startup).
func NewWebhookHandler(queries *db.Queries, signingKey string) *WebhookHandler {
	return &WebhookHandler{Queries: queries, SigningKey: signingKey}
}

// zitadelActionV2Event is the subset of the Actions V2
// payload this handler reads. We intentionally do NOT
// use encoding/json's case-insensitive matching: the
// spec'd field names (`userID`, `event_type`) are
// case-sensitive per the Zitadel skill's notes, and
// using strict tags keeps any drift visible at the
// test layer.
type zitadelActionV2Event struct {
	AggregateID   string          `json:"aggregateID"`
	AggregateType string          `json:"aggregateType"`
	ResourceOwner string          `json:"resourceOwner"`
	InstanceID    string          `json:"instanceID"`
	Version       string          `json:"version"`
	Sequence      json.Number     `json:"sequence"`
	EventType     string          `json:"event_type"`
	CreatedAt     string          `json:"created_at"`
	UserID        string          `json:"userID"`
	EventPayload  json.RawMessage `json:"event_payload"`
}

// Handle is the Echo entry point. The flow is:
//  1. read the raw body (signature verification needs
//     the exact bytes, not a re-marshalled struct)
//  2. verify the ZITADEL-Signature HMAC
//  3. parse the JSON
//  4. dispatch by event_type
//
// Every failure path returns through shared.RespondError
// so the wire envelope stays consistent.
func (h *WebhookHandler) Handle(c echo.Context) error {
	if h.SigningKey == "" {
		// Defence in depth: the env loader also refuses
		// to start without this key, but if the handler
		// is constructed by a test or future code path
		// that forgot to plumb it, refuse every request
		// rather than silently accept everything.
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "webhook signing key not configured"))
	}
	if h.Queries == nil {
		// Same defence in depth for the DB layer: a
		// misconfigured router that forgets to pass a
		// *db.Queries must NOT crash the process with
		// a nil-pointer panic, but return 500 so the
		// caller can see the misconfiguration.
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "webhook queries not configured"))
	}

	rawBody, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "read request body"))
	}

	// Verify HMAC against the ZITADEL-Signature header.
	// The helper handles dual-header fallback, timestamp
	// tolerance (default 5 minutes), and constant-time
	// HMAC compare internally - we never look at the
	// signature bytes ourselves.
	if err := actions.ValidateRequestPayload(rawBody, &c.Request().Header, h.SigningKey); err != nil {
		slog.Warn("zitadel webhook signature verification failed",
			"err", err, "path", c.Path(), "remote", c.RealIP())
		return shared.RespondError(c, shared.Wrap(shared.ErrWebhookSignature, "signature verification failed"))
	}

	var evt zitadelActionV2Event
	if err := json.Unmarshal(rawBody, &evt); err != nil {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "invalid JSON body"))
	}
	if evt.EventType == "" {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "missing event_type"))
	}
	if evt.UserID == "" {
		return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "missing userID"))
	}

	// Audit log: capture the raw body + event_type so
	// debugging a misrouted Action does not require
	// re-deriving the payload from Zitadel.
	slog.Info("zitadel webhook accepted",
		"event_type", evt.EventType,
		"userID", evt.UserID,
		"sequence", evt.Sequence.String(),
		"remote", c.RealIP(),
	)

	ctx := c.Request().Context()
	switch evt.EventType {
	case EventUserDeactivated:
		return h.handleUserDeactivated(ctx, c, evt.UserID)
	case EventUserRemoved:
		// DEVIATION: full implementation lives in phase 8
		// (DeleteUserData: tombstone + delete rows +
		// enqueue R2 cleanup). Phase 3 only logs the
		// event and returns 500 so a real Zitadel
		// webhook is not silently dropped on the floor.
		slog.Error("user.removed received but DeleteUserData is phase 8 TODO",
			"userID", evt.UserID, "remote", c.RealIP())
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal,
			fmt.Sprintf("user.removed handler not implemented in phase 3 (userID=%s)", evt.UserID)))
	default:
		return shared.RespondError(c, shared.Wrap(shared.ErrWebhookEvent,
			fmt.Sprintf("event_type %q is not supported", evt.EventType)))
	}
}

// errUserNotFound is the sentinel returned by
// lookupUserIDByZitadelID when no local row matches
// the Zitadel sub. The webhook treats this as a
// benign "no local user, nothing to deactivate" so
// Zitadel does not retry forever.
var errUserNotFound = errors.New("local user not found")

// lookupUserIDByZitadelID is a thin wrapper around
// GetUserByZitadelID that returns the user id
// (uuid.UUID) instead of the full row, so the webhook
// handler does not have to import the db.User type.
// Hiding the row keeps the handler narrow and makes
// the missing-row case ergonomic.
//
// sqlc-generated code over pgxpool can surface a
// missing row as either pgx.ErrNoRows (when the
// consumer is a *pgxpool.Pool) or sql.ErrNoRows (when
// the consumer is a *sql.DB opened via
// pgx/v5/stdlib, as is the case in main.go). We check
// for both so the handler works in either wiring.
func lookupUserIDByZitadelID(ctx context.Context, q *db.Queries, sub string) (uuid.UUID, error) {
	row, err := q.GetUserByZitadelID(ctx, sub)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, errUserNotFound
		}
		return uuid.Nil, fmt.Errorf("GetUserByZitadelID: %w", err)
	}
	return row.ID, nil
}

// handleUserDeactivated mirrors the schema: set
// is_active=FALSE and stamp deleted_at. If the local
// row does not exist (e.g. the user never logged in
// after the very first webhook delivery), that is a
// 200 - there is nothing to deactivate and a 4xx
// would cause Zitadel to retry, which we do not want.
func (h *WebhookHandler) handleUserDeactivated(ctx context.Context, c echo.Context, zitadelSub string) error {
	userID, err := lookupUserIDByZitadelID(ctx, h.Queries, zitadelSub)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			// No local row to deactivate; ack the
			// webhook so Zitadel stops retrying.
			return shared.RespondOK(c, map[string]string{"status": "processed"})
		}
		slog.Error("deactivate user lookup failed", "err", err, "sub", zitadelSub)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "lookup user"))
	}

	_, err = h.Queries.DeactivateUser(ctx, userID)
	if err != nil {
		slog.Error("DeactivateUser failed", "err", err, "user_id", userID)
		return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "deactivate user"))
	}
	return shared.RespondOK(c, map[string]string{"status": "processed"})
}
