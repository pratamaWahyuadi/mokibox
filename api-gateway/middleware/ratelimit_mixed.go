// ratelimit_mixed.go implements the COMBINED per-user /
// per-IP token-bucket rate limiter for the auth-OPTIONAL
// read endpoints GET /api/search and GET /api/feed/home
// (Fitur A, decision K5/K6).
//
// Key selection:
//   - when a *db.User is already on the Echo context
//     (AuthenticateOptional resolved a valid JWT), the key
//     is the user UUID — a logged-in user is NEVER counted
//     against an IP bucket.
//   - otherwise the key is the client IP resolved through
//     the trusted-proxy rules (K6): a peer inside
//     TRUSTED_PROXY_CIDRS may set X-Real-IP (preferred) /
//     X-Forwarded-For (fallback) and it is honoured; an
//     untrusted peer's forwarding headers are IGNORED and
//     the direct peer IP is used (anti-spoof).
//
// Configuration comes from env via shared.APIConfig:
//
//	SEARCH_RATE_LIMIT_PER_MIN (default 60, anon burst 15)
//	FEED_RATE_LIMIT_PER_MIN   (default 120, anon burst 40)
//	TRUSTED_PROXY_CIDRS       (default dev: loopback +
//	                           docker dev range 172.16.0.0/12;
//	                           production overrides to
//	                           127.0.0.1/32,::1/128,192.168.144.1/32)
//
// Empty/invalid env values fall back to the defaults
// without crashing (K5). The limiter store/janitor is
// reused from ratelimit.go; throttle responses use the
// canonical shared.ErrRateLimited envelope (429).
package middleware

import (
	"log/slog"
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// DefaultTrustedProxyCIDRs is the DEV default for
// TRUSTED_PROXY_CIDRS when the env var is empty: loopback
// plus the docker dev bridge range. Production sets the
// explicit value 127.0.0.1/32,::1/128,192.168.144.1/32.
const DefaultTrustedProxyCIDRs = "127.0.0.1/32,::1/128,172.16.0.0/12"

// Anonymous burst sizes for the two auth-optional
// endpoints (K5). The per-minute refill rate is env
// configurable; the burst is fixed because a larger burst
// would defeat the anti-scraping intent for anonymous
// callers.
const (
	SearchRateLimitBurstAnon = 15
	FeedRateLimitBurstAnon   = 40
)

// DefaultSearchRateLimitPerMin / DefaultFeedRateLimitPerMin
// are applied when the corresponding env vars are empty or
// invalid (K5: never crash on config).
const (
	DefaultSearchRateLimitPerMin = 60
	DefaultFeedRateLimitPerMin   = 120
)

// parseTrustedProxyCIDRs parses a comma-separated CIDR
// list. Invalid entries are skipped (with a log line) so a
// typo cannot crash the gateway at startup; an empty list
// falls back to DefaultTrustedProxyCIDRs.
func parseTrustedProxyCIDRs(raw string) []*net.IPNet {
	if strings.TrimSpace(raw) == "" {
		raw = DefaultTrustedProxyCIDRs
	}
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(part)
		if err != nil {
			slog.Warn("api-gateway: invalid TRUSTED_PROXY_CIDRS entry ignored", "entry", part)
			continue
		}
		out = append(out, ipnet)
	}
	return out
}

// peerIP extracts the host part of the TCP peer address.
// Returns nil when RemoteAddr is missing or malformed
// (which should not happen for a real request).
func peerIP(c echo.Context) net.IP {
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		host = c.Request().RemoteAddr
	}
	return net.ParseIP(host)
}

// isTrustedPeer reports whether the direct TCP peer is
// inside one of the trusted CIDRs.
func isTrustedPeer(peer net.IP, trusted []*net.IPNet) bool {
	if peer == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(peer) {
			return true
		}
	}
	return false
}

// forwardedIP validates a header value as an IP, returning
// "" when it is absent or not a plain IP (anti-spoof: a
// malformed value is never trusted).
func forwardedIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// For X-Forwarded-For take the first hop only when the
	// whole header is a plain IP list; the first entry is
	// what the closest trusted proxy claims the client is.
	if i := strings.Index(value, ","); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	if net.ParseIP(value) == nil {
		return ""
	}
	return value
}

// resolveClientIP implements K6: trusted peer ->
// X-Real-IP first, X-Forwarded-For fallback, then the peer
// IP; untrusted peer -> peer IP with headers ignored.
func resolveClientIP(c echo.Context, trusted []*net.IPNet) string {
	peer := peerIP(c)
	if isTrustedPeer(peer, trusted) {
		if ip := forwardedIP(c.Request().Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if ip := forwardedIP(c.Request().Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
	}
	if peer != nil {
		return peer.String()
	}
	// Last-resort fallback so the limiter key is never
	// empty (an empty key would be a shared bucket).
	return "unknown"
}

// mixedLimitKeyFunc returns the limiter identity: the user
// UUID when a user is on the context, else the resolved
// client IP.
func mixedLimitKeyFunc(trusted []*net.IPNet) rateLimitKeyFunc {
	return func(c echo.Context) (string, error) {
		if userID, ok := UserIDFromContext(c); ok && userID != uuid.Nil {
			return "user:" + userID.String(), nil
		}
		ip := resolveClientIP(c, trusted)
		if ip == "" {
			return "", shared.Wrap(shared.ErrUnauthorized, "rate limit requires source IP")
		}
		return "ip:" + ip, nil
	}
}

// NewRateLimitOptionalAuth returns the combined per-user /
// per-IP limiter for the auth-optional endpoints. It MUST
// be installed AFTER middleware.AuthenticateOptional so a
// valid token has already resolved the user onto the
// context. Anonymous requests are keyed per client IP
// (K6 trusted-proxy rules); authenticated requests are
// keyed per user id and never share an IP bucket.
//
// On throttle: 429 with the canonical RATE_LIMITED
// envelope (shared.ClassifyError maps ErrRateLimited).
func NewRateLimitOptionalAuth(cfg RateLimitConfig, trustedProxyCIDRs string) (echo.MiddlewareFunc, error) {
	return NewRateLimitAuthWithKey(cfg, mixedLimitKeyFunc(parseTrustedProxyCIDRs(trustedProxyCIDRs)))
}
