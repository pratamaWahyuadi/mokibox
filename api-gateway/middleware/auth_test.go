package middleware

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// stubVerifier is a test-only TokenVerifier. It satisfies
// the consumer-side TokenVerifier interface (one method)
// and NEVER touches Zitadel. This is the pattern mandated
// by the phase-10 issue #29: no reintroduction of a
// production deny-all verifier; stubs live in _test.go.
type stubVerifier struct {
	// sub is returned on success. When err is non-nil the
	// token is rejected.
	sub string
	err error
	// calls records every raw token handed to
	// CheckToken so tests can assert the middleware
	// passed the exact bearer value.
	calls []string
}

func (s *stubVerifier) CheckToken(ctx context.Context, rawToken string) (string, error) {
	s.calls = append(s.calls, rawToken)
	return s.sub, s.err
}

// stubUserStore is a test-only UserStore. Each lookup /
// insert is driven by a function field so a test can
// simulate existing users, races, and DB failures.
type stubUserStore struct {
	getByZitadelID func(ctx context.Context, zitadelID string) (db.User, error)
	create         func(ctx context.Context, arg db.CreateUserParams) (db.User, error)
}

func (s *stubUserStore) GetUserByZitadelID(ctx context.Context, zitadelID string) (db.User, error) {
	return s.getByZitadelID(ctx, zitadelID)
}

func (s *stubUserStore) CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
	return s.create(ctx, arg)
}

// userHolder lets the probe handler publish the resolved
// user back to the test after ServeHTTP returns.
type userHolder struct {
	user *db.User
}

// newAuthEcho builds a minimal Echo app with the
// Authenticate middleware in front of a probe handler
// that records the resolved user.
func newAuthEcho(cfg AuthenticateConfig) (*echo.Echo, *userHolder) {
	e := echo.New()
	h := &userHolder{}
	e.Use(Authenticate(cfg))
	e.GET("/api/probe", func(c echo.Context) error {
		u, ok := UserFromContext(c)
		if !ok {
			return c.NoContent(http.StatusNoContent)
		}
		h.user = u
		return c.String(http.StatusOK, "ok")
	})
	return e, h
}

func doReq(e *echo.Echo, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	if authHeader != "" {
		req.Header.Set(echo.HeaderAuthorization, authHeader)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticate_MissingAndMalformedHeader(t *testing.T) {
	verifier := &stubVerifier{}
	store := &stubUserStore{}
	cfg := AuthenticateConfig{Verifier: verifier, Queries: store}
	e, _ := newAuthEcho(cfg)

	cases := []struct {
		name string
		hdr  string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic abcdef"},
		{"bare token no prefix", "sometoken"},
		{"prefix only", "Bearer "},
		{"prefix no space content", "Bearer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doReq(e, c.hdr)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"UNAUTHORIZED"`) {
				t.Errorf("body should carry UNAUTHORIZED code, got %q", body)
			}
		})
	}
	if len(verifier.calls) != 0 {
		t.Errorf("verifier must not be called for malformed headers, got %d calls", len(verifier.calls))
	}
}

func TestAuthenticate_InvalidToken(t *testing.T) {
	verifier := &stubVerifier{err: errors.New("jwt: signature invalid")}
	store := &stubUserStore{}
	e, _ := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer bad-token")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthenticate_EmptySubRejected(t *testing.T) {
	// The zitadel-go verifier can succeed structurally
	// while yielding an empty subject; the middleware
	// must treat that as 401, never as a user.
	verifier := &stubVerifier{sub: ""}
	store := &stubUserStore{}
	e, _ := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer valid-but-empty-sub")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestAuthenticate_ExistingUserHappyPath(t *testing.T) {
	sub := "388995719870021635"
	uid := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	verifier := &stubVerifier{sub: sub}
	store := &stubUserStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			if zitadelID != sub {
				return db.User{}, errors.New("unexpected sub")
			}
			return db.User{ID: uid, ZitadelID: zitadelID, IsActive: true}, nil
		},
	}
	e, seen := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer good-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(verifier.calls) != 1 || verifier.calls[0] != "good-token" {
		t.Errorf("verifier must receive the raw bearer token, got %v", verifier.calls)
	}
	if seen.user == nil || seen.user.ID != uid {
		t.Errorf("downstream handler should see the resolved user %s, got %+v", uid, seen)
	}
}

func TestAuthenticate_GetOrCreateInsertsOnFirstLogin(t *testing.T) {
	sub := "newuser123"
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	verifier := &stubVerifier{sub: sub}
	var inserted db.CreateUserParams
	store := &stubUserStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			// First lookup: not found.
			return db.User{}, sql.ErrNoRows
		},
		create: func(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
			inserted = arg
			return db.User{ID: uid, ZitadelID: arg.ZitadelID, Username: arg.Username, IsActive: true}, nil
		},
	}
	e, seen := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer first-login-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if inserted.ZitadelID != sub {
		t.Errorf("CreateUser must receive the verified sub, got %q", inserted.ZitadelID)
	}
	if !strings.HasPrefix(inserted.Username, "user_") {
		t.Errorf("generated username should start with user_, got %q", inserted.Username)
	}
	if inserted.DisplayName.Valid && inserted.DisplayName.String == "" {
		t.Error("display name should be null or non-empty, not empty-string valid")
	}
	if seen.user == nil || seen.user.ID != uid {
		t.Errorf("created user should flow to the handler, got %+v", seen)
	}
}

func TestAuthenticate_GetOrCreateRaceReSelects(t *testing.T) {
	// Simulate the ON CONFLICT DO NOTHING race: CreateUser
	// returns sql.ErrNoRows because a concurrent request
	// won the insert; the middleware must re-select and
	// succeed.
	sub := "raceuser"
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	verifier := &stubVerifier{sub: sub}
	lookups := 0
	store := &stubUserStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			lookups++
			if lookups == 1 {
				return db.User{}, sql.ErrNoRows
			}
			return db.User{ID: uid, ZitadelID: zitadelID, IsActive: true}, nil
		},
		create: func(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
			return db.User{}, sql.ErrNoRows // lost the race
		},
	}
	e, seen := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer racing-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if seen.user == nil || seen.user.ID != uid {
		t.Errorf("race winner's row should flow to the handler, got %+v", seen)
	}
	if lookups != 2 {
		t.Errorf("expected 2 lookups (initial + re-select), got %d", lookups)
	}
}

func TestAuthenticate_DBFailureIs500(t *testing.T) {
	sub := "dbdown"
	verifier := &stubVerifier{sub: sub}
	store := &stubUserStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			return db.User{}, errors.New("connection refused")
		},
	}
	e, _ := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer token-with-db-down")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"INTERNAL_ERROR"`) {
		t.Errorf("body should carry INTERNAL_ERROR, got %q", rec.Body.String())
	}
}

func TestAuthenticate_InactiveUserRejected(t *testing.T) {
	// Tombstoned / deactivated users must not be able to
	// keep using the API with a pre-deactivation token
	// (SEC-04 / webhook user.deactivated path).
	sub := "tombstoned"
	uid := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	verifier := &stubVerifier{sub: sub}
	store := &stubUserStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			return db.User{ID: uid, ZitadelID: zitadelID, IsActive: false}, nil
		},
	}
	e, seen := newAuthEcho(AuthenticateConfig{Verifier: verifier, Queries: store})

	rec := doReq(e, "Bearer pre-deactivation-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if seen.user != nil {
		t.Error("inactive user must NOT reach the downstream handler")
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		name string
		hdr  string
		want string
		ok   bool
	}{
		{"missing", "", "", false},
		{"no prefix", "token123", "", false},
		{"prefix only", "Bearer ", "", false},
		{"wrong scheme lowercase", "bearer token123", "", false},
		{"valid", "Bearer token123", "token123", true},
		{"exact prefix length", "BearerX", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.hdr != "" {
				req.Header.Set(echo.HeaderAuthorization, c.hdr)
			}
			rec := httptest.NewRecorder()
			ctx := e.NewContext(req, rec)
			got, err := extractBearer(ctx)
			if c.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != c.want {
					t.Errorf("token = %q, want %q", got, c.want)
				}
			} else if err == nil {
				t.Errorf("expected error for %q, got token %q", c.hdr, got)
			}
		})
	}
}

func TestUserFromContext(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// Empty context: (nil, false).
	if u, ok := UserFromContext(c); u != nil || ok {
		t.Errorf("empty context should yield (nil,false), got (%v,%v)", u, ok)
	}

	// Wrong type stored: (nil, false), not a panic.
	c.Set(userContextKey, "not-a-user")
	if u, ok := UserFromContext(c); u != nil || ok {
		t.Errorf("wrong type should yield (nil,false), got (%v,%v)", u, ok)
	}

	// Real user: (user, true).
	want := &db.User{ID: uuid.MustParse("00000000-0000-0000-0000-000000000004")}
	c.Set(userContextKey, want)
	got, ok := UserFromContext(c)
	if !ok || got != want {
		t.Errorf("want (%p,true), got (%p,%v)", want, got, ok)
	}
}

func TestUserIDFromContext(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// No user on context.
	if id, ok := UserIDFromContext(c); ok || id != uuid.Nil {
		t.Errorf("no user: want (Nil,false), got (%v,%v)", id, ok)
	}

	want := uuid.MustParse("00000000-0000-0000-0000-000000000005")
	c.Set(userContextKey, &db.User{ID: want})
	if id, ok := UserIDFromContext(c); !ok || id != want {
		t.Errorf("want (%s,true), got (%v,%v)", want, id, ok)
	}
}
