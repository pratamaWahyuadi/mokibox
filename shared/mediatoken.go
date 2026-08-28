// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
//
// This file implements the short-lived HMAC media token
// described in planning/04_api_contracts.md (A5) and PRD
// FR-VIDEO-10 / SEC-06. The token authorizes a browser or
// video player to fetch a private HLS playlist for a given
// video for a bounded window, so we do not need to issue
// a full JWT and we do not need to validate the token on
// every .ts segment request at the API gateway (segments
// are fetched directly from R2 with their own presigned
// URL).
//
// Token format (after URL-safe base64 encoding):
//
//	<unix-expiry>.<hex-hmac-sha256>
//
// where the HMAC is computed over the literal string
// "video_id:<expiry>". This binds the token to a specific
// video so a token issued for video A cannot be replayed
// against video B. Verification uses hmac.Equal for
// constant-time comparison.
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
)

// ErrMediaTokenInvalid is returned when a token fails
// structural validation, has a bad HMAC, or has expired.
// All three collapse to one sentinel because the wire
// response is identical: the client must request a fresh
// playlist URL. Splitting them would leak whether a token
// "almost" matched.
var ErrMediaTokenInvalid = errors.New("media token invalid")

// NewMediaToken returns a MediaToken bound to the given
// videoID. ttl controls the lifetime; expiry is now() + ttl.
// secret is the shared HMAC key and must be at least 32
// bytes for the SHA-256 output to be meaningful; shorter
// values are accepted but discouraged.
//
// NewMediaToken does not consult the clock; it stamps
// expiry = time.Now() at call time. Tests that need a
// stable "now" must call SignMediaToken with a custom now
// function.
func NewMediaToken(videoID, secret string, ttl time.Duration) (string, time.Time, error) {
	if videoID == "" {
		return "", time.Time{}, fmt.Errorf("%w: empty videoID", ErrMediaTokenInvalid)
	}
	if secret == "" {
		return "", time.Time{}, fmt.Errorf("%w: empty secret", ErrMediaTokenInvalid)
	}
	if ttl <= 0 {
		return "", time.Time{}, fmt.Errorf("%w: ttl must be > 0", ErrMediaTokenInvalid)
	}
	expiry := time.Now().Add(ttl)
	token, err := SignMediaToken(videoID, secret, expiry)
	return token, expiry, err
}

// SignMediaToken computes a token for a given expiry. It is
// exposed so tests and the (future) worker can sign tokens
// deterministically without depending on the wall clock.
func SignMediaToken(videoID, secret string, expiry time.Time) (string, error) {
	expiryUnix := strconv.FormatInt(expiry.Unix(), 10)
	payload := "video_id:" + videoID + ":" + expiryUnix
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(payload)); err != nil {
		// hmac.Hash.Write never returns an error in the
		// current implementation, but we propagate to be
		// future-safe.
		return "", fmt.Errorf("sign media token: %w", err)
	}
	sig := hex.EncodeToString(mac.Sum(nil))
	raw := expiryUnix + "." + sig
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

// VerifyMediaToken validates a token against videoID and
// secret. It returns nil if the token is well-formed, has a
// matching HMAC, has not expired, and was bound to the
// expected videoID.
//
// A nil error means the caller may serve the playlist.
// Any other error is ErrMediaTokenInvalid (wrapped) so the
// central error handler can map it to 401 UNAUTHORIZED.
func VerifyMediaToken(token, videoID, secret string) error {
	if token == "" {
		return fmt.Errorf("%w: empty token", ErrMediaTokenInvalid)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMediaTokenInvalid, err)
	}
	parts := strings.SplitN(string(raw), ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("%w: bad format", ErrMediaTokenInvalid)
	}
	expiryUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad expiry: %v", ErrMediaTokenInvalid, err)
	}
	if time.Now().Unix() > expiryUnix {
		return fmt.Errorf("%w: expired", ErrMediaTokenInvalid)
	}
	expectedPayload := "video_id:" + videoID + ":" + parts[0]
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(expectedPayload)); err != nil {
		return fmt.Errorf("verify media token: %w", err)
	}
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("%w: bad signature encoding: %v", ErrMediaTokenInvalid, err)
	}
	// hmac.Equal is constant-time; do NOT replace with
	// bytes.Equal or ==.
	if !hmac.Equal(expected, got) {
		return fmt.Errorf("%w: signature mismatch", ErrMediaTokenInvalid)
	}
	return nil
}
