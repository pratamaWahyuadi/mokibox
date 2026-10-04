// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file implements the short-lived HMAC WebSocket ticket
// used by the chat feature. Browsers cannot attach an
// Authorization header to a WebSocket handshake, so a client
// first calls POST /api/chat/ws-ticket (authenticated with its
// normal JWT) and then opens
//
//	GET /api/chat/ws?ticket=<ticket>
//
// The design mirrors the media token in mediatoken.go: the
// long-lived JWT never appears in a URL, only a ticket that is
// bound to one user and expires after a few seconds.
//
// Ticket format (after URL-safe base64 encoding):
//
//	<unix-expiry>.<user-uuid>.<hex-hmac-sha256>
//
// where the HMAC is computed over "ws_ticket:<user-uuid>:<expiry>".
// The "ws_ticket:" prefix separates this token from a media token
// ("video_id:..."), so the two can share one secret without a token
// for one purpose ever verifying for the other.
package shared

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrWSTicketInvalid is returned for every verification failure
// (malformed, bad signature, expired). They collapse to one
// sentinel so a client cannot tell how close a forged ticket was.
var ErrWSTicketInvalid = errors.New("ws ticket invalid")

// NewWSTicket returns a ticket bound to userID that expires after ttl.
func NewWSTicket(userID uuid.UUID, secret string, ttl time.Duration) (string, time.Time, error) {
	if userID == uuid.Nil {
		return "", time.Time{}, fmt.Errorf("%w: empty user id", ErrWSTicketInvalid)
	}
	if secret == "" {
		return "", time.Time{}, fmt.Errorf("%w: empty secret", ErrWSTicketInvalid)
	}
	if ttl <= 0 {
		return "", time.Time{}, fmt.Errorf("%w: ttl must be > 0", ErrWSTicketInvalid)
	}
	expiry := time.Now().Add(ttl)
	ticket, err := SignWSTicket(userID, secret, expiry)
	return ticket, expiry, err
}

// SignWSTicket signs a ticket for an explicit expiry. It is exposed
// so tests can build expired or future tickets deterministically.
func SignWSTicket(userID uuid.UUID, secret string, expiry time.Time) (string, error) {
	expiryUnix := strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte("ws_ticket:" + userID.String() + ":" + expiryUnix)); err != nil {
		return "", fmt.Errorf("sign ws ticket: %w", err)
	}
	raw := expiryUnix + "." + userID.String() + "." + hex.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

// VerifyWSTicket validates a ticket and returns the user it was
// issued to. Any failure is ErrWSTicketInvalid (wrapped).
func VerifyWSTicket(ticket, secret string) (uuid.UUID, error) {
	if ticket == "" || secret == "" {
		return uuid.Nil, fmt.Errorf("%w: empty ticket or secret", ErrWSTicketInvalid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrWSTicketInvalid, err)
	}
	parts := strings.SplitN(string(raw), ".", 3)
	if len(parts) != 3 {
		return uuid.Nil, fmt.Errorf("%w: bad format", ErrWSTicketInvalid)
	}
	expiryUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad expiry", ErrWSTicketInvalid)
	}
	userID, err := uuid.Parse(parts[1])
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: bad user id", ErrWSTicketInvalid)
	}
	got, err := hex.DecodeString(parts[2])
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad signature encoding", ErrWSTicketInvalid)
	}

	// Check the signature before the expiry so the failure reason
	// is not an oracle for forged-but-expired tickets.
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte("ws_ticket:" + userID.String() + ":" + parts[0])); err != nil {
		return uuid.Nil, fmt.Errorf("verify ws ticket: %w", err)
	}
	// hmac.Equal is constant-time; do NOT replace with bytes.Equal or ==.
	if !hmac.Equal(mac.Sum(nil), got) {
		return uuid.Nil, fmt.Errorf("%w: signature mismatch", ErrWSTicketInvalid)
	}
	if time.Now().Unix() > expiryUnix {
		return uuid.Nil, fmt.Errorf("%w: expired", ErrWSTicketInvalid)
	}
	return userID, nil
}
