// Package handlers - username_test.go covers Fitur B:
// PUT /api/users/me/username (self username change, 7-day
// cooldown, case-insensitive uniqueness, K11 field
// visibility). All 13 scenarios from the implementation
// plan live here; the store is the shared stubUserStore.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// usernameTakenErr is the SQLSTATE 23505 the unique index
// uq_users_username_lower raises on a duplicate.
var usernameTakenErr = pgconn.PgError{Code: "23505"}

// changeUsernameHarness wires the given user (their
// Username field is the CURRENT username) in front of
// UpdateUsername.
func changeUsernameHarness(u db.User, store *stubUserStore) *echo.Echo {
	return harnessAuthEcho(u, http.MethodPut, "/api/users/me/username", newUserHandlerForTest(store).UpdateUsername)
}

// decodeSelfProfile decodes the {data: ...} envelope of a
// self-profile response into a generic map.
func decodeSelfProfile(t *testing.T, body string) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return env.Data
}

// successRow is the UpdateUsername RETURNING row of a
// successful change at the given time.
func successRow(id [16]byte, name string, at time.Time) db.UpdateUsernameRow {
	return db.UpdateUsernameRow{
		ID:                id,
		Username:          name,
		UsernameChangedAt: sql.NullTime{Time: at, Valid: true},
	}
}

// Scenario 1: first change (username_changed_at NULL) ->
// 200 with new username + username_changed_at filled.
func TestUpdateUsername_FirstChange200(t *testing.T) {
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return successRow(arg.UserID, arg.NewUsername, time.Now()), nil
		},
	}
	e := changeUsernameHarness(viewerUser(), store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"newname"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	data := decodeSelfProfile(t, rec.Body.String())
	if data["username"] != "newname" {
		t.Errorf("username = %v, want newname", data["username"])
	}
	if s, ok := data["username_changed_at"].(string); !ok || s == "" {
		t.Errorf("username_changed_at missing on self response: %v", data["username_changed_at"])
	}
}

// cooldownProbe is the GetUsernameChangedAt row for a user
// that changed their username at `recent` (still cooling).
func cooldownProbe(recent time.Time) db.GetUsernameChangedAtRow {
	return db.GetUsernameChangedAtRow{
		UsernameChangedAt: sql.NullTime{Time: recent, Valid: true},
		NextChangeAt:      recent.Add(7 * 24 * time.Hour),
		RetryAfterSeconds: int64((7*24*time.Hour - time.Since(recent)).Seconds()),
	}
}

// Scenario 2: second change within 7 days (UPDATE affects
// 0 rows, changed_at recent) -> 429 USERNAME_CHANGE_COOLDOWN
// with next_change_at in details.
func TestUpdateUsername_WithinCooldown429(t *testing.T) {
	recent := time.Now().Add(-24 * time.Hour)
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return db.UpdateUsernameRow{}, sql.ErrNoRows // 0 rows affected
		},
		getUsernameChangedAt: func(ctx context.Context, userID uuid.UUID) (db.GetUsernameChangedAtRow, error) {
			return cooldownProbe(recent), nil
		},
	}
	u := viewerUser()
	u.UsernameChangedAt = sql.NullTime{Time: recent, Valid: true}
	e := changeUsernameHarness(u, store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"othername"}`)
	body := rec.Body.String()
	assertErrCode(t, rec, http.StatusTooManyRequests, shared.CodeUsernameChangeCooldown)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Error.Details) == 0 || env.Error.Details[0].Field != "next_change_at" {
		t.Fatalf("details = %+v, want next_change_at", env.Error.Details)
	}
	if _, err := time.Parse(time.RFC3339, env.Error.Details[0].Message); err != nil {
		t.Errorf("next_change_at %q is not RFC3339: %v", env.Error.Details[0].Message, err)
	}
}

// Scenario 3: exactly past 7 days (changed_at = now-7d-1h)
// -> 200. No time.Sleep - the mock simply holds an old
// changed_at, mirroring the DB conditional.
func TestUpdateUsername_AfterCooldown200(t *testing.T) {
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return successRow(arg.UserID, arg.NewUsername, time.Now()), nil
		},
	}
	u := viewerUser()
	u.UsernameChangedAt = sql.NullTime{
		Time:  time.Now().Add(-7*24*time.Hour - time.Hour),
		Valid: true,
	}
	e := changeUsernameHarness(u, store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"latename"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// Scenario 4: race - two parallel requests, only ONE may
// get RowsAffected=1 (the atomic UPDATE); the other gets 0
// rows -> 429. Exactly one 200, exactly one 429.
func TestUpdateUsername_RaceOnlyOneWins(t *testing.T) {
	var once sync.Once
	recent := time.Now().Add(-time.Hour)
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			var winner bool
			once.Do(func() { winner = true })
			if winner {
				return successRow(arg.UserID, arg.NewUsername, time.Now()), nil
			}
			return db.UpdateUsernameRow{}, sql.ErrNoRows
		},
		getUsernameChangedAt: func(ctx context.Context, userID uuid.UUID) (db.GetUsernameChangedAtRow, error) {
			return cooldownProbe(recent), nil
		},
	}
	u := viewerUser()
	u.UsernameChangedAt = sql.NullTime{Time: recent, Valid: true}
	e := changeUsernameHarness(u, store)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"racename"}`)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()
	wins, cooldowns := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			wins++
		case http.StatusTooManyRequests:
			cooldowns++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if wins != 1 || cooldowns != 1 {
		t.Errorf("race result = %v, want exactly one 200 and one 429", codes)
	}
}

// Scenario 5: unique violation (SQLSTATE 23505) ->
// 409 USERNAME_TAKEN.
func TestUpdateUsername_CaseInsensitiveTaken409(t *testing.T) {
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return db.UpdateUsernameRow{}, &usernameTakenErr
		},
	}
	e := changeUsernameHarness(viewerUser(), store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"taken_name"}`)
	assertErrCode(t, rec, http.StatusConflict, shared.CodeUsernameTaken)
}

// Scenario 6: deleted_ prefix -> 400 VALIDATION_ERROR.
func TestUpdateUsername_DeletedPrefix400(t *testing.T) {
	store := &stubUserStore{}
	e := changeUsernameHarness(viewerUser(), store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"deleted_foo"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
}

// Scenario 7: reserved words -> 400 VALIDATION_ERROR.
func TestUpdateUsername_Reserved400(t *testing.T) {
	for _, name := range []string{"admin", "me", "mokibox", "api", "system", "support"} {
		store := &stubUserStore{}
		e := changeUsernameHarness(viewerUser(), store)
		rec := doJSON(e, http.MethodPut, "/api/users/me/username", fmt.Sprintf(`{"username":%q}`, name))
		assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
	}
}

// Scenario 8: two separate users do not affect each other -
// each request carries the user_id from the auth context.
func TestUpdateUsername_PerUserIsolation(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			mu.Lock()
			defer mu.Unlock()
			seen[arg.UserID.String()] = true
			return successRow(arg.UserID, arg.NewUsername, time.Now()), nil
		},
	}
	e := changeUsernameHarness(viewerUser(), store)
	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"viewer_new"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	e2 := changeUsernameHarness(activePublicOwner(), store)
	rec2 := doJSON(e2, http.MethodPut, "/api/users/me/username", `{"username":"owner_new"}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec2.Code, rec2.Body.String())
	}

	if len(seen) != 2 || !seen[testViewerID.String()] || !seen[testOwnerID.String()] {
		t.Errorf("user_ids seen by the store = %v, want both users' context ids", seen)
	}
}

// Scenario 9: malformed payloads -> 400 VALIDATION_ERROR,
// and the store is never touched.
func TestUpdateUsername_MalformedPayloads400(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"username":"Ab"}`,
		`{"username":".abc"}`,
		`{"username":"abc."}`,
		`{"username":"a..b"}`,
		`{"username":"AB CD"}`,
	}
	for _, body := range bodies {
		store := &stubUserStore{}
		e := changeUsernameHarness(viewerUser(), store)
		rec := doJSON(e, http.MethodPut, "/api/users/me/username", body)
		assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
		if atomic.LoadInt32(&store.updateUsernameCalls) != 0 {
			t.Errorf("%s: store must not be hit on invalid input", body)
		}
	}
}

// Scenario 10 (K11): another user's profile
// (GET /api/users/:id) must NOT contain
// username_changed_at.
func TestUpdateUsername_PublicProfileOmitsChangedAt(t *testing.T) {
	store := &stubUserStore{
		getProfileWithStats: func(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error) {
			return db.GetUserProfileWithStatsRow{
				ID: arg.ID, Username: "target", IsActive: true,
				CreatedAt: time.Now(),
			}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/users/:id", uh.GetUserProfile)

	rec := doJSON(e, http.MethodGet, "/api/users/"+testOwnerID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "username_changed_at") {
		t.Errorf("public profile leaked username_changed_at: %s", rec.Body.String())
	}
	data := decodeSelfProfile(t, rec.Body.String())
	if _, ok := data["username_changed_at"]; ok {
		t.Error("public profile must not carry username_changed_at key")
	}
}

// Scenario 11 (item 4b): same-as-current username ->
// 400 VALIDATION_ERROR and the store proves UpdateUsername
// was NEVER called (cooldown must not be consumed).
func TestUpdateUsername_SameAsOwnNotConsumed(t *testing.T) {
	store := &stubUserStore{}
	e := changeUsernameHarness(viewerUser(), store) // viewer's username = "viewer"

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"VIEWER"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
	if atomic.LoadInt32(&store.updateUsernameCalls) != 0 {
		t.Errorf("UpdateUsername called %d times for a no-op change; cooldown must not be consumed",
			store.updateUsernameCalls)
	}
}

// Scenario 12 (item 4a): 0-row UPDATE because the user is
// not active -> 404.
func TestUpdateUsername_InactiveUser404(t *testing.T) {
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return db.UpdateUsernameRow{}, sql.ErrNoRows
		},
		getUsernameChangedAt: func(ctx context.Context, userID uuid.UUID) (db.GetUsernameChangedAtRow, error) {
			return db.GetUsernameChangedAtRow{}, sql.ErrNoRows
		},
	}
	e := changeUsernameHarness(viewerUser(), store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"gone_name"}`)
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

// Scenario 13: the 429 response carries a Retry-After
// header in seconds, in addition to next_change_at details.
func TestUpdateUsername_CooldownRetryAfterHeader(t *testing.T) {
	recent := time.Now().Add(-time.Hour)
	store := &stubUserStore{
		updateUsername: func(ctx context.Context, arg db.UpdateUsernameParams) (db.UpdateUsernameRow, error) {
			return db.UpdateUsernameRow{}, sql.ErrNoRows
		},
		getUsernameChangedAt: func(ctx context.Context, userID uuid.UUID) (db.GetUsernameChangedAtRow, error) {
			row := cooldownProbe(recent)
			row.RetryAfterSeconds = 3600
			return row, nil
		},
	}
	u := viewerUser()
	u.UsernameChangedAt = sql.NullTime{Time: recent, Valid: true}
	e := changeUsernameHarness(u, store)

	rec := doJSON(e, http.MethodPut, "/api/users/me/username", `{"username":"later_name"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "3600" {
		t.Errorf("Retry-After = %q, want \"3600\" (seconds from DB time)", got)
	}
}

// changeUsernameRules unit table - lowercase normalization
// happens in the handler; rules operate on the normalized
// value. auto-generated user_<hex> names stay valid.
func TestChangeUsernameRules(t *testing.T) {
	valid := []string{"abc", "a.b_c9", "user_1a2b3c4d5e6f", strings.Repeat("a", 30)}
	invalid := []string{"ab", strings.Repeat("a", 31), "ab-cd", "Abc", "a b", ".ab", "ab.", "a..b", "deleted_x", "admin", "", "ünicode"}
	for _, s := range valid {
		if err := changeUsernameRules(s); err != nil {
			t.Errorf("changeUsernameRules(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range invalid {
		if err := changeUsernameRules(s); err == nil {
			t.Errorf("changeUsernameRules(%q) = nil, want error", s)
		}
	}
}
