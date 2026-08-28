// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file implements the opaque pagination cursor mandated
// by planning/04_api_contracts.md. A cursor is the
// base64url encoding of "<RFC3339Nano>|<uuid>" and is used
// together with the SQL pattern
//
//	WHERE (created_at, id) < ($1, $2)
//	ORDER BY created_at DESC, id DESC
//	LIMIT n
//
// to page through a list whose ordering key is the
// (created_at, id) tuple. RFC3339Nano preserves nanosecond
// precision so two rows created in the same millisecond
// still order deterministically when the uuid is used as
// the tie-breaker.
package shared

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidCursor is returned by DecodeCursor when the
// input is not a valid cursor produced by EncodeCursor.
// It is exposed as a sentinel so handlers can map it to
// 400 VALIDATION_ERROR with a stable code.
var ErrInvalidCursor = errors.New("invalid cursor")

// EncodeCursor builds a cursor from a (createdAt, id) pair.
// The returned string is url-safe base64 with no padding so
// it can be used directly as a query parameter.
//
// createdAt is rendered with full nanosecond precision
// (time.RFC3339Nano). Pass time.Time{} if you want the
// empty cursor (returns ""), although callers usually
// guard that case at the handler layer.
func EncodeCursor(createdAt time.Time, id uuid.UUID) string {
	if createdAt.IsZero() || id == uuid.Nil {
		return ""
	}
	raw := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a cursor produced by EncodeCursor.
// It returns the timestamp and uuid. An empty input is
// treated as "first page" and returns zero values with a
// nil error so handlers can pass the request cursor
// through without a special case.
func DecodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	if cursor == "" {
		return time.Time{}, uuid.Nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidCursor, err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: missing separator", ErrInvalidCursor)
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad timestamp: %v", ErrInvalidCursor, err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad uuid: %v", ErrInvalidCursor, err)
	}
	return ts, id, nil
}
