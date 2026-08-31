// Package middleware contains Echo middleware for the
// api-gateway. Keep this package focused on cross-cutting
// HTTP concerns: auth, rate limiting, request logging.
// Business logic belongs in api-gateway/handlers; data
// access belongs in shared/db (sqlc).
//
// auth.go implements the OIDC authentication middleware
// for /api/*. The verification logic itself lives in
// github.com/zitadel/zitadel-go/v3 (we never re-implement
// JWT signature verification, JWKS fetch, or audience
// checks by hand - per SEC-07). This file is responsible
// for:
//   1. Extracting the bearer token from the request.
//   2. Asking the configured TokenVerifier to validate
//      the token against Zitadel (JWKS + issuer +
//      audience + expiry + algorithm, all done by
//      authorization.DefaultJWTAuthorization).
//   3. Reading the `sub` claim from the verified context.
//   4. Resolving the local *db.User (get-or-create on
//      first login).
//   5. Rejecting deactivated users (is_active=false) with
//      401 UNAUTHORIZED.
//   6. Storing *db.User on the Echo context so downstream
//      handlers can read it via UserFromContext.
package middleware

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/zitadel/zitadel-go/v3/pkg/authorization"
	"github.com/zitadel/zitadel-go/v3/pkg/authorization/oauth"
	"github.com/zitadel/zitadel-go/v3/pkg/zitadel"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// TokenVerifier is the narrow interface the middleware
// needs from the JWT verification layer. It is defined
// here (consumer side) rather than in the zitadel-go
// package so handlers and tests can provide a one-method
// mock without depending on the full authorization stack.
//
// CheckToken returns the verified subject identifier
// (Zitadel `sub` claim) for the given raw bearer token.
// An empty string together with a non-nil error means the
// token failed validation.
type TokenVerifier interface {
	// CheckToken validates the bearer token and returns
	// the user's Zitadel subject (sub) on success. Any
	// non-nil error must be mapped to 401 UNAUTHORIZED
	// by the caller.
	CheckToken(ctx context.Context, rawToken string) (sub string, err error)
}

// VerifierFactory builds a TokenVerifier from a context.
// It exists so main.go can pass in the live
// *authorization.Authorizer (which itself needs a context
// to perform OIDC discovery + JWKS fetch) and so tests can
// pass a constructor that returns a stub.
type VerifierFactory func(ctx context.Context) (TokenVerifier, error)

// UserContextKey is the key under which the verified
// *db.User is stored on the Echo context. Handler code
// must read it via UserFromContext, not by indexing the
// context directly, so the key is centralised.
const userContextKey = "auth.currentUser"

// UserFromContext returns the *db.User that the
// Authenticate middleware stored on the Echo context.
// Returns (nil, false) when the context does not carry a
// user (which should only happen on routes mounted
// outside the auth group, e.g. the Zitadel webhook and
// the health check).
func UserFromContext(c echo.Context) (*db.User, bool) {
	v := c.Get(userContextKey)
	if v == nil {
		return nil, false
	}
	u, ok := v.(*db.User)
	return u, ok
}

// newUsername returns a fresh username of the form
// user_<12 hex chars>. The randomness comes from
// crypto/rand because math/rand is predictable and we do
// not want two near-simultaneous first logins to collide
// (the column has a UNIQUE constraint, see
// migrations/001_init.sql).
func newUsername() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return "user_" + hex.EncodeToString(b[:]), nil
}

// getOrCreateUser resolves a *db.User for a Zitadel
// subject. If the user does not exist yet, a new row is
// inserted with a generated username. The sqlc query
// CreateUser uses ON CONFLICT DO NOTHING RETURNING * so a
// concurrent first-login for the same sub collapses to a
// single row; in that case CreateUser returns
// sql.ErrNoRows and we re-select. This keeps the
// get-or-create path race-free without taking a row lock.
func getOrCreateUser(ctx context.Context, q *db.Queries, sub string) (db.User, error) {
	existing, err := q.GetUserByZitadelID(ctx, sub)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.User{}, fmt.Errorf("get user by zitadel id: %w", err)
	}

	username, err := newUsername()
	if err != nil {
		return db.User{}, fmt.Errorf("generate username: %w", err)
	}

	created, err := q.CreateUser(ctx, db.CreateUserParams{
		ZitadelID:   sub,
		Username:    username,
		DisplayName: sql.NullString{String: username, Valid: true},
	})
	if err == nil {
		return created, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		// sql.ErrNoRows here means ON CONFLICT DO NOTHING
		// swallowed a concurrent insert; any other error is
		// a real failure and must propagate.
		return db.User{}, fmt.Errorf("create user: %w", err)
	}

	// Race: another request inserted the same sub between
	// our GetUserByZitadelID and CreateUser. Re-select the
	// winner's row.
	existing, err = q.GetUserByZitadelID(ctx, sub)
	if err != nil {
		return db.User{}, fmt.Errorf("re-select user after race: %w", err)
	}
	return existing, nil
}

// AuthenticateConfig bundles the dependencies the
// Authenticate middleware needs. The struct is exported so
// main.go can pass it by value; the middleware is a
// method on it so we do not have a package-level global.
type AuthenticateConfig struct {
	// Verifier is the JWT validator. It is built once at
	// startup (zitadel.New + oauth.DefaultJWTAuthorization
	// in main.go) and shared across requests.
	Verifier TokenVerifier
	// Queries is the sqlc-bound *db.Queries used to read
	// and insert the local users row.
	Queries *db.Queries
}

// Authenticate returns an Echo middleware that validates
// the Authorization header against the configured
// TokenVerifier, resolves the local *db.User for the
// verified subject, and stores it on the context.
//
// Failure modes (all map to 401 UNAUTHORIZED via
// shared.RespondError):
//   - missing / malformed Authorization header
//   - JWT signature invalid, expired, wrong audience, etc.
//   - the Zitadel subject is empty after verification
//   - the local user row is marked is_active = false
//     (deactivated, tombstoned, or pending delete)
//
// On success the next handler runs with a *db.User
// available via UserFromContext.
func Authenticate(cfg AuthenticateConfig) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rawToken, err := extractBearer(c)
			if err != nil {
				return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, err.Error()))
			}

			ctx := c.Request().Context()
			sub, err := cfg.Verifier.CheckToken(ctx, rawToken)
			if err != nil || sub == "" {
				return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "invalid access token"))
			}

			user, err := getOrCreateUser(ctx, cfg.Queries, sub)
			if err != nil {
				slog.Error("get-or-create user failed", "err", err, "sub", sub)
				return shared.RespondError(c, shared.Wrap(shared.ErrInternal, "resolve user"))
			}
			if !user.IsActive {
				// Tombstoned / deactivated users must not be
				// able to keep using the API with a
				// pre-deactivation token. The webhook
				// user.deactivated is the only place that
				// flips this flag.
				return shared.RespondError(c, shared.Wrap(shared.ErrUnauthorized, "user is inactive"))
			}

			// Hand the resolved user to downstream handlers
			// via the Echo context. We do not expose the
			// raw sub or the JWT itself; handlers only see
			// the local row.
			c.Set(userContextKey, &user)
			return next(c)
		}
	}
}

// extractBearer pulls the raw token out of the
// Authorization header. We accept the standard "Bearer
// <token>" form; the value is returned exactly as it
// appears (no trimming, no decoding) because the verifier
// needs the raw JWT to check the signature. Any deviation
// from the expected shape is an error mapped to
// UNAUTHORIZED upstream.
func extractBearer(c echo.Context) (string, error) {
	h := c.Request().Header.Get(echo.HeaderAuthorization)
	if h == "" {
		return "", errors.New("missing Authorization header")
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", errors.New("malformed Authorization header")
	}
	token := h[len(prefix):]
	if token == "" {
		return "", errors.New("empty bearer token")
	}
	return token, nil
}

// UserIDFromContext is a small convenience used by
// handlers that only need the user id (uuid.UUID) instead
// of the full *db.User. Returns (uuid.Nil, false) when
// the auth middleware did not run on this route.
func UserIDFromContext(c echo.Context) (uuid.UUID, bool) {
	u, ok := UserFromContext(c)
	if !ok || u == nil {
		return uuid.Nil, false
	}
	return u.ID, true
}

// zitadelTokenVerifier is the production TokenVerifier.
// It wraps the *authorization.Authorizer built by
// authorization.New + oauth.DefaultJWTAuthorization,
// which performs OIDC discovery, JWKS fetch + cache +
// auto-refresh, and verifies signature, issuer, audience,
// and expiry in a single call. We never replicate any of
// that logic here.
type zitadelTokenVerifier struct {
	authZ *authorization.Authorizer[*oauth.IntrospectionContext]
}

// CheckToken validates the bearer token and returns the
// Zitadel subject (the `sub` claim). An empty subject
// together with a non-nil error is treated by the
// middleware as 401.
func (v *zitadelTokenVerifier) CheckToken(ctx context.Context, rawToken string) (string, error) {
	authCtx, err := v.authZ.CheckAuthorization(ctx, rawToken)
	if err != nil {
		return "", err
	}
	if !authCtx.IsAuthorized() {
		return "", errors.New("token not authorized")
	}
	return authCtx.UserID(), nil
}

// NewZitadelVerifier builds the production TokenVerifier
// for the api-gateway. It performs the OIDC discovery +
// JWKS fetch eagerly so a misconfigured issuer URL is
// surfaced at startup rather than on the first request.
// The returned TokenVerifier is safe for concurrent use
// and is meant to be built once and passed to
// AuthenticateConfig.
func NewZitadelVerifier(ctx context.Context, issuerURL, apiClientID string) (TokenVerifier, error) {
	if issuerURL == "" {
		return nil, errors.New("NewZitadelVerifier: issuerURL is empty")
	}
	if apiClientID == "" {
		return nil, errors.New("NewZitadelVerifier: apiClientID is empty")
	}
	authZ, err := authorization.New(
		ctx,
		zitadel.New(issuerURL),
		oauth.DefaultJWTAuthorization(apiClientID),
	)
	if err != nil {
		return nil, fmt.Errorf("zitadel authorization.New: %w", err)
	}
	return &zitadelTokenVerifier{authZ: authZ}, nil
}
