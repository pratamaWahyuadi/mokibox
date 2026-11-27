// ratelimit_mixed_test.go - tests for the combined
// per-user / per-IP rate limiter used by the
// auth-optional endpoints /api/search and /api/feed/home
// (Fitur A, K5/K6).
//
// Covered behaviour:
//   - different client IPs get separate buckets
//   - exhausting the burst returns 429 RATE_LIMITED
//   - spoofed X-Real-IP / X-Forwarded-For from a
//     NON-trusted peer does NOT change the key (the
//     direct peer IP is used instead)
//   - from a TRUSTED peer the key is taken from
//     X-Real-IP (preferred) / X-Forwarded-For (fallback)
//   - a request with a user on the context is keyed per
//     user id, NOT per IP (logged-in users never share
//     the anonymous per-IP bucket)
//   - empty/invalid TRUSTED_PROXY_CIDRS falls back to
//     the dev default without crashing
package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// newMixedEcho builds an Echo app with the combined
// limiter in front of a 200-OK probe handler.
func newMixedEcho(mw echo.MiddlewareFunc) *echo.Echo {
	e := echo.New()
	e.GET("/api/probe", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}, mw)
	return e
}

func mixedReq(e *echo.Echo, remoteAddr string, headers map[string]string, user *db.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	if user != nil {
		// Simulate what Authenticate/AuthenticateOptional
		// store on the context before the limiter runs.
		e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set(userContextKey, user)
				return next(c)
			}
		})
	}
	e.ServeHTTP(rec, req)
	return rec
}

func newMixedLimiter(t *testing.T, perMin int, burst int, cidrs string) echo.MiddlewareFunc {
	t.Helper()
	mw, err := NewRateLimitOptionalAuth(RateLimitConfig{
		Rate:          rate.Limit(float64(perMin) / 60.0),
		Burst:         burst,
		TTL:           10 * time.Minute,
		SweepInterval: 5 * time.Minute,
	}, cidrs)
	if err != nil {
		t.Fatalf("NewRateLimitOptionalAuth: %v", err)
	}
	return mw
}

func TestMixedLimiter_SeparateBucketsPerIP(t *testing.T) {
	// 2 req/min, burst 2: each IP may pass exactly 2 requests.
	e := newMixedEcho(newMixedLimiter(t, 2, 2, "127.0.0.1/32"))
	for i := 0; i < 2; i++ {
		rec := mixedReq(e, "10.1.1.1:5000", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d from ip1: expected 200, got %d", i, rec.Code)
		}
	}
	rec := mixedReq(e, "10.1.1.1:5000", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("ip1 third request: expected 429, got %d", rec.Code)
	}
	// A different IP still has a fresh bucket.
	rec = mixedReq(e, "10.1.1.2:5000", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ip1 exhausted but ip2 expected 200, got %d", rec.Code)
	}
}

func TestMixedLimiter_ThrottleEnvelope(t *testing.T) {
	e := newMixedEcho(newMixedLimiter(t, 1, 1, "127.0.0.1/32"))
	_ = mixedReq(e, "10.2.2.2:5000", nil, nil)
	rec := mixedReq(e, "10.2.2.2:5000", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "RATE_LIMITED") {
		t.Fatalf("expected RATE_LIMITED envelope, got %s", rec.Body.String())
	}
}

func TestMixedLimiter_SpoofedHeadersIgnoredFromUntrustedPeer(t *testing.T) {
	// Trusted = loopback only; the request comes from a
	// private-range peer that is NOT trusted, so both
	// spoofing headers must be ignored and the peer IP
	// used as the key.
	e := newMixedEcho(newMixedLimiter(t, 1, 1, "127.0.0.1/32,::1/128"))
	headers := map[string]string{
		"X-Real-IP":         "8.8.8.8",
		"X-Forwarded-For":   "8.8.8.8, 1.1.1.1",
	}
	_ = mixedReq(e, "10.3.3.3:5000", headers, nil)
	rec := mixedReq(e, "10.3.3.3:5000", headers, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed headers must not change the key: expected 429 on repeat, got %d", rec.Code)
	}
	// The real client 8.8.8.8 was never actually seen;
	// a fresh peer still passes.
	rec = mixedReq(e, "10.3.3.4:5000", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh peer expected 200, got %d", rec.Code)
	}
}

func TestMixedLimiter_TrustedPeerUsesRealIP(t *testing.T) {
	// Peer 192.168.144.1 is trusted (production docker
	// bridge gateway): X-Real-IP becomes the key.
	e := newMixedEcho(newMixedLimiter(t, 1, 1, "127.0.0.1/32,::1/128,192.168.144.1/32"))
	headers := map[string]string{"X-Real-IP": "203.0.113.7"}
	_ = mixedReq(e, "192.168.144.1:5000", headers, nil)
	rec := mixedReq(e, "192.168.144.1:5000", headers, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("trusted peer: same X-Real-IP should share a bucket, got %d", rec.Code)
	}
	// X-Forwarded-For fallback when X-Real-IP is absent.
	h2 := map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"}
	_ = mixedReq(e, "192.168.144.1:5001", h2, nil)
	rec = mixedReq(e, "192.168.144.1:5001", h2, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("trusted peer: XFF fallback should share a bucket, got %d", rec.Code)
	}
	// A different X-Real-IP is a different bucket.
	rec = mixedReq(e, "192.168.144.1:5002", map[string]string{"X-Real-IP": "203.0.113.8"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("different X-Real-IP should be a fresh bucket, got %d", rec.Code)
	}
}

func TestMixedLimiter_LoggedInUserKeyedPerUser(t *testing.T) {
	cidrs := "127.0.0.1/32"
	e := newMixedEcho(newMixedLimiter(t, 1, 1, cidrs))
	user := &db.User{ID: mustUUID(t)}

	// Same IP as an anonymous flood source: the anonymous
	// bucket is drained first.
	anonHeaders := map[string]string{"X-Real-IP": "203.0.113.50"}
	anonIP := "203.0.113.50"
	_ = mixedReq(e, anonIP+":5000", nil, nil)
	rec := mixedReq(e, anonIP+":5001", nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("anonymous bucket should be exhausted, got %d", rec.Code)
	}

	// The logged-in user from the SAME IP with the same
	// spoofed-looking header still passes: keyed per user.
	rec = mixedReq(e, "192.168.144.1:5000", anonHeaders, user)
	if rec.Code != http.StatusOK {
		t.Fatalf("logged-in user must not share the anonymous per-IP bucket, got %d", rec.Code)
	}
	// Second request from the same user drains their own bucket.
	rec = mixedReq(e, "192.168.144.1:5000", anonHeaders, user)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("user bucket should drain after burst, got %d", rec.Code)
	}
}

func TestMixedLimiter_EmptyAndInvalidCIDRsFallBackToDefault(t *testing.T) {
	// Empty string -> dev default (loopback). A loopback
	// peer IS trusted, so X-Real-IP is honoured.
	e := newMixedEcho(newMixedLimiter(t, 1, 1, ""))
	_ = mixedReq(e, "127.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.60"}, nil)
	rec := mixedReq(e, "127.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.60"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("empty CIDRs must fall back to dev default (loopback trusted), got %d", rec.Code)
	}

	// Invalid CIDR entries are skipped, not fatal.
	e2 := newMixedEcho(newMixedLimiter(t, 1, 1, "not-a-cidr,127.0.0.1/32"))
	_ = mixedReq(e2, "127.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.61"}, nil)
	rec = mixedReq(e2, "127.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.61"}, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("invalid CIDR entries must be skipped without crashing, got %d", rec.Code)
	}
}

func TestResolveClientIP_PrefersRealIPForTrustedPeer(t *testing.T) {
	trusted := parseTrustedProxyCIDRs("127.0.0.1/32")
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:44444"
	req.Header.Set("X-Real-IP", "198.51.100.9")
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	c := e.NewContext(req, nil)
	got := resolveClientIP(c, trusted)
	if got != "198.51.100.9" {
		t.Fatalf("expected X-Real-IP preferred, got %q", got)
	}
}

func TestResolveClientIP_UntrustedPeerIgnoresHeaders(t *testing.T) {
	trusted := parseTrustedProxyCIDRs("127.0.0.1/32")
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.9.9.9:1234"
	req.Header.Set("X-Real-IP", "198.51.100.9")
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	c := e.NewContext(req, nil)
	got := resolveClientIP(c, trusted)
	if got != "10.9.9.9" {
		t.Fatalf("expected peer IP, got %q", got)
	}
}

func TestParseTrustedProxyCIDRs(t *testing.T) {
	nets := parseTrustedProxyCIDRs("127.0.0.1/32, ::1/128, garbage, 172.16.0.0/12")
	if len(nets) != 3 {
		t.Fatalf("expected 3 valid CIDRs (garbage skipped), got %d", len(nets))
	}
	ip := net.ParseIP("192.168.144.1")
	matched := false
	for _, n := range nets {
		if n.Contains(ip) {
			matched = true
		}
	}
	if matched {
		t.Fatalf("192.168.144.1 must not be trusted by this list")
	}
	if !containsIP(nets, net.ParseIP("172.20.0.5")) {
		t.Fatalf("172.20.0.5 must be inside trusted 172.16.0.0/12")
	}
	if len(parseTrustedProxyCIDRs("")) != 3 {
		t.Fatalf("empty input must yield the dev default (3 CIDRs)")
	}
}

func containsIP(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse("0198f2a0-1111-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("uuid.Parse: %v", err)
	}
	return id
}
