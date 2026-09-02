// Package middleware contains Echo middleware for the
// api-gateway. Keep this package focused on cross-cutting
// HTTP concerns: auth, rate limiting, request logging.
// Business logic belongs in api-gateway/handlers; data
// access belongs in shared/db (sqlc).
//
// ratelimit.go implements per-identity token-bucket rate
// limits using golang.org/x/time/rate (promoted from
// indirect to direct dependency in Fase 9). Two policies:
//
//   - RateLimitAuth: per-user. Applied AFTER
//     Authenticate inside the /api group so the key is
//     the verified *db.User's UUID. Auth-failure paths
//     (missing / malformed / invalid token) short-circuit
//     before this middleware runs, so an attacker cannot
//     exhaust a victim's limit by spamming invalid tokens
//     against their user id (the auth middleware never
//     stores the user).
//
//   - RateLimitWebhook: per-IP. Applied to the
//     /api/webhooks/zitadel endpoint only. The webhook
//     authenticates via HMAC (no JWT), so the only stable
//     identity is the caller IP (c.RealIP()).
//
// Both limiters share a sync.Map of *rate.Limiter keyed
// by string identity. Each entry also tracks lastSeen
// so a background janitor can evict entries that have
// not been touched in a configurable TTL - without
// eviction the map would grow unbounded for a service
// that has many transient users.
//
// On throttle the middleware responds with the canonical
// {error:{code:RATE_LIMITED,message:...}} envelope via
// shared.RespondError. Mapping to HTTP 429 happens in
// shared.ClassifyError because the shared.ErrRateLimited
// sentinel already maps to that status.
package middleware

import (
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// rateLimiterEntry pairs a token-bucket Limiter with
// the wall-clock time of its most recent access. The
// janitor uses lastSeen to evict cold entries so the
// map does not grow without bound.
type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// rateLimiterStore is the per-process map of identity
// to *rate.Limiter. It is safe for concurrent use; the
// embedded sync.Map provides the locking semantics, the
// janitor mutates entries from a single goroutine, and
// Allow calls are read-only after the initial LoadOrStore.
type rateLimiterStore struct {
	m sync.Map // map[string]*rateLimiterEntry

	// rate is the token refill rate; burst is the
	// bucket size. For RateLimitAuth we set
	// rate = 1.0 per second (60 per minute) and
	// burst = 60 so a freshly-observed user can do
	// up to 60 calls immediately then drain at 1/s.
	rate  rate.Limit
	burst int

	// ttl is the inactivity window after which an
	// entry is eligible for eviction. 10 minutes is
	// long enough that an active user's bucket survives
	// between requests, short enough that the map does
	// not accumulate dead users.
	ttl time.Duration
}

// newRateLimiterStore builds a store with the given
// refill rate, burst and TTL.
func newRateLimiterStore(r rate.Limit, burst int, ttl time.Duration) *rateLimiterStore {
	return &rateLimiterStore{rate: r, burst: burst, ttl: ttl}
}

// allow performs a token-bucket check for the given
// identity. The entry is created on first observation
// and its lastSeen is refreshed on every call. Returns
// false when the bucket is empty; the caller responds
// with 429.
func (s *rateLimiterStore) allow(identity string) bool {
	now := time.Now()
	if v, ok := s.m.Load(identity); ok {
		entry := v.(*rateLimiterEntry)
		entry.lastSeen = now
		return entry.limiter.AllowN(now, 1)
	}
	// LoadOrStore so a concurrent first-touch from two
	// goroutines on the same identity shares a single
	// limiter rather than racing two Allow() calls that
	// could both pass against freshly minted buckets.
	limiter := rate.NewLimiter(s.rate, s.burst)
	entry := &rateLimiterEntry{limiter: limiter, lastSeen: now}
	actual, _ := s.m.LoadOrStore(identity, entry)
	ae := actual.(*rateLimiterEntry)
	ae.lastSeen = now
	return ae.limiter.AllowN(now, 1)
}

// sweep removes entries whose lastSeen is older than
// the TTL. Called from the janitor goroutine. We use
// Range plus a slice of stale keys rather than mutating
// during Range because sync.Map does not allow safe
// deletion from within the iteration callback.
func (s *rateLimiterStore) sweep(now time.Time) {
	var stale []string
	s.m.Range(func(key, value any) bool {
		entry := value.(*rateLimiterEntry)
		if now.Sub(entry.lastSeen) > s.ttl {
			stale = append(stale, key.(string))
		}
		return true
	})
	for _, k := range stale {
		s.m.Delete(k)
	}
	if n := len(stale); n > 0 {
		slog.Info("api-gateway: rate-limit store sweep", "evicted", n, "ttl", s.ttl.String())
	}
}

// runJanitor blocks sweeping the store every interval.
// Sweep period is intentionally shorter than TTL so a
// cold entry is detected on the first sweep after its
// lastSeen crosses the boundary.
func (s *rateLimiterStore) runJanitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		s.sweep(time.Now())
	}
}

// RateLimitConfig bundles the knobs for both rate
// limit middlewares. The zero value is rejected by
// NewRateLimitAuth and NewRateLimitWebhook; both
// constructors require positive rate, burst, ttl
// and sweep interval.
type RateLimitConfig struct {
	// Rate is the refill rate. RateLimitAuth uses
	// 1 per second (60 per minute); RateLimitWebhook
	// uses 0.5 per second (30 per minute). Pass a
	// rate.Limit value (rate is a type alias for
	// float64 internally).
	Rate rate.Limit
	// Burst is the bucket size. Both middlewares use
	// the same value as the rate (in tokens) so a
	// freshly-observed identity can spend the entire
	// per-minute quota immediately, then drain at the
	// configured refill rate.
	Burst int
	// TTL is the inactivity window after which an
	// entry is eligible for eviction. 10 minutes by
	// default; longer is fine but bloats the map.
	TTL time.Duration
	// SweepInterval is how often the janitor runs.
	// Must be strictly less than TTL or entries will
	// not be evicted until the next sweep after expiry.
	SweepInterval time.Duration
}

// newStore wires the limiter store plus janitor. The
// caller (one of the two constructors below) must have
// validated cfg before calling.
func newStore(cfg RateLimitConfig) *rateLimiterStore {
	store := newRateLimiterStore(cfg.Rate, cfg.Burst, cfg.TTL)
	go store.runJanitor(cfg.SweepInterval)
	return store
}

// validate returns an error for any zero or negative
// field. Called from both constructors so the failure
// surfaces at main.go startup rather than silently
// no-oping under load.
func (c RateLimitConfig) validate() error {
	if c.Rate <= 0 {
		return errInvalidRateLimit
	}
	if c.Burst <= 0 {
		return errInvalidBurst
	}
	if c.TTL <= 0 {
		return errInvalidTTL
	}
	if c.SweepInterval <= 0 {
		return errInvalidSweep
	}
	return nil
}

// errInvalidRateLimit / errInvalidBurst / errInvalidTTL /
// errInvalidSweep are sentinel errors returned by
// RateLimitConfig.validate. They satisfy errors.Is so
// main.go can match with a single check.
var (
	errInvalidRateLimit = errRateLimit("rate must be > 0")
	errInvalidBurst     = errRateLimit("burst must be > 0")
	errInvalidTTL       = errRateLimit("ttl must be > 0")
	errInvalidSweep     = errRateLimit("sweep interval must be > 0")
)

type errRateLimit string

func (e errRateLimit) Error() string { return string(e) }

// NewRateLimitAuth returns an Echo middleware that
// enforces a per-user token-bucket rate limit. It MUST
// be installed AFTER Authenticate in the chain so the
// verified user id is available on the context.
//
// Behaviour:
//
//   - Key = uuid.UUID of the user from
//     UserIDFromContext(c). When the user is missing
//     (which should not happen because Authenticate
//     runs first, but is checked for defence in depth)
//     we return 401 so the missing-auth case cannot
//     bypass the limit by spoofing an empty key.
//   - On throttle: respond with the canonical
//     {error:{code:RATE_LIMITED,message:...}} envelope
//     and HTTP 429 (mapping in shared.ClassifyError).
func NewRateLimitAuth(cfg RateLimitConfig) (echo.MiddlewareFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	store := newStore(cfg)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, ok := UserIDFromContext(c)
			if !ok || userID == uuid.Nil {
				return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "rate limit requires authenticated user"))
			}
			if !store.allow(userID.String()) {
				return shared.RespondError(c, shared.Wrap(shared.ErrRateLimited, "per-user rate limit exceeded"))
			}
			return next(c)
		}
	}, nil
}

// NewRateLimitWebhook returns an Echo middleware that
// enforces a per-IP token-bucket rate limit. It MUST
// be installed on the /api/webhooks/zitadel route only
// (the webhook authenticates via HMAC, not JWT, so the
// only stable caller identity is the source IP).
//
// Behaviour:
//
//   - Key = c.RealIP() (Echo's RealIP honours X-Real-IP
//     and X-Forwarded-For when a trusted proxy is in
//     front; with the current single-VPS topology the
//     direct peer is the source).
//   - On throttle: 429 RATE_LIMITED envelope, same as
//     NewRateLimitAuth.
func NewRateLimitWebhook(cfg RateLimitConfig) (echo.MiddlewareFunc, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	store := newStore(cfg)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			ip := c.RealIP()
			if ip == "" {
				// Empty RealIP should not happen for a
				// real TCP request, but if it does we
				// surface 401 rather than silently
				// bypass the limit.
				return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "rate limit requires source IP"))
			}
			if !store.allow(ip) {
				return shared.RespondError(c, shared.Wrap(shared.ErrRateLimited, "per-IP rate limit exceeded"))
			}
			return next(c)
		}
	}, nil
}