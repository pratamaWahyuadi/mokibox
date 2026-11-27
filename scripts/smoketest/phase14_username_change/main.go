// Phase 14 smoke test: Fitur B — penggantian username
// sendiri (PUT /api/users/me/username), cooldown 7 hari.
//
// Scenarios (per implementation plan Tahap 5):
//
//  1. PUT a new username with a valid token -> 200, the
//     response carries username + username_changed_at
//     (RFC3339).
//  2. PUT again immediately -> 429 USERNAME_CHANGE_COOLDOWN
//     with a Retry-After header (seconds) and a
//     next_change_at field detail.
//  3. Operator clears the cooldown in the dev DB
//     (UPDATE users SET username_changed_at = now() -
//     interval '7 days' — NO sleep) -> PUT again -> 200.
//  4. PUT a username already owned by user 2 (seeded
//     directly in the dev DB) -> 409 USERNAME_TAKEN
//     (case-insensitive).
//  5. PUT "deleted_foo" -> 400 VALIDATION_ERROR.
//  6. User 2 is untouched by every operation above: its
//     username and username_changed_at (NULL) stay put.
//
// Usage (against a dev deployment; the token must belong
// to a dedicated smoke test user):
//
//	API_BASE_URL=https://api.example.com \
//	SMOKE_ACCESS_TOKEN=<test-user-jwt> \
//	DATABASE_URL=postgres://... \
//	go run ./scripts/smoketest/phase14_username_change
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	phase14User2Username = "smoke_phase14_user2"
	phase14User2Prefix   = "smoke-phase14-user2-"
)

type smokeClient struct {
	api   string
	token string
	hc    *http.Client
}

type apiResult struct {
	status     int
	code       string           // error.code, "" when none
	details    []map[string]any // error.details
	data       map[string]any
	headers    http.Header
	errSummary string
}

func (c *smokeClient) do(ctx context.Context, method, path, body string) (*apiResult, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.api, "/")+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	out := &apiResult{status: res.StatusCode, headers: res.Header}
	var env struct {
		Data  map[string]any   `json:"data"`
		Error *struct {
			Code    string           `json:"code"`
			Message string           `json:"message"`
			Details []map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil {
		out.data = env.Data
		if env.Error != nil {
			out.code = env.Error.Code
			out.details = env.Error.Details
			out.errSummary = env.Error.Message
		}
	}
	return out, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase14_username_change")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	api := os.Getenv("API_BASE_URL")
	if api == "" {
		return fmt.Errorf("API_BASE_URL is required")
	}
	token := os.Getenv("SMOKE_ACCESS_TOKEN")
	if token == "" {
		return fmt.Errorf("SMOKE_ACCESS_TOKEN is required")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required (dev DB; used to seed user 2 and clear the cooldown)")
	}
	dbh, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer dbh.Close()
	if err := dbh.PingContext(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	c := &smokeClient{api: api, token: token, hc: &http.Client{Timeout: 20 * time.Second}}

	// ----- Seed user 2 directly in the dev DB -----
	// (idempotent reruns: reuse the row when present).
	var user2ID string
	err = dbh.QueryRowContext(ctx, `SELECT id FROM users WHERE lower(username) = $1`, phase14User2Username).Scan(&user2ID)
	if err == sql.ErrNoRows {
		err = dbh.QueryRowContext(ctx, `
			INSERT INTO users (zitadel_id, username, display_name)
			VALUES ($1, $2, 'phase14 smoke user 2')
			RETURNING id`,
			phase14User2Prefix+fmt.Sprint(time.Now().UnixNano()), phase14User2Username).Scan(&user2ID)
	}
	if err != nil {
		return fmt.Errorf("seed user 2: %w", err)
	}

	// Pre-clean: reset user 2's cooldown so scenario 6's
	// assertion is meaningful, and clear user 1's cooldown
	// for a deterministic first change.
	if _, err := dbh.ExecContext(ctx, `UPDATE users SET username_changed_at = NULL WHERE id = $1`, user2ID); err != nil {
		return fmt.Errorf("reset user 2: %w", err)
	}
	uid1, err := currentUserID(ctx, dbh, token)
	if err != nil {
		return fmt.Errorf("resolve smoke user id: %w", err)
	}
	if _, err := dbh.ExecContext(ctx, `UPDATE users SET username_changed_at = NULL WHERE id = $1`, uid1); err != nil {
		return fmt.Errorf("reset user 1 cooldown: %w", err)
	}

	// ----- 1. First change -> 200 -----
	uniq := fmt.Sprintf("p14_%d", time.Now().UnixNano()%1_000_000)
	r, err := c.do(ctx, http.MethodPut, "/api/users/me/username", fmt.Sprintf(`{"username":%q}`, uniq))
	if err != nil {
		return err
	}
	if r.status != 200 {
		return fmt.Errorf("scenario 1: status=%d body=%s", r.status, r.errSummary)
	}
	if r.data["username"] != uniq {
		return fmt.Errorf("scenario 1: username=%v want %v", r.data["username"], uniq)
	}
	if _, ok := r.data["username_changed_at"].(string); !ok {
		return fmt.Errorf("scenario 1: username_changed_at missing: %v", r.data)
	}
	log.Printf("PASS scenario 1: first change -> 200, username_changed_at set")

	// ----- 2. Immediate second change -> 429 -----
	r, err = c.do(ctx, http.MethodPut, "/api/users/me/username", `{"username":"p14_second"}`)
	if err != nil {
		return err
	}
	if r.status != 429 || r.code != "USERNAME_CHANGE_COOLDOWN" {
		return fmt.Errorf("scenario 2: status=%d code=%s, want 429 USERNAME_CHANGE_COOLDOWN", r.status, r.code)
	}
	if ra := r.headers.Get("Retry-After"); ra == "" {
		return fmt.Errorf("scenario 2: Retry-After header missing")
	}
	if !hasDetail(r.details, "next_change_at") {
		return fmt.Errorf("scenario 2: next_change_at detail missing: %v", r.details)
	}
	log.Printf("PASS scenario 2: immediate retry -> 429 + Retry-After=%s", r.headers.Get("Retry-After"))

	// ----- 3. Operator clears cooldown in DB -> 200 -----
	if _, err := dbh.ExecContext(ctx,
		`UPDATE users SET username_changed_at = now() - interval '7 days' WHERE id = $1`, uid1); err != nil {
		return fmt.Errorf("scenario 3 db update: %w", err)
	}
	uniq2 := uniq + "b"
	r, err = c.do(ctx, http.MethodPut, "/api/users/me/username", fmt.Sprintf(`{"username":%q}`, uniq2))
	if err != nil {
		return err
	}
	if r.status != 200 {
		return fmt.Errorf("scenario 3: status=%d code=%s, want 200 after cooldown cleared", r.status, r.code)
	}
	log.Printf("PASS scenario 3: after DB-cleared cooldown -> 200")

	// ----- 4. Username owned by user 2 -> 409 -----
	// Scenario 3 consumed the cooldown (changed_at=now()).
	// Clear it again so the UPDATE passes the cooldown
	// predicate and reaches the unique-index violation
	// (23505) instead of short-circuiting as 429.
	if _, err := dbh.ExecContext(ctx,
		`UPDATE users SET username_changed_at = now() - interval '7 days' WHERE id = $1`, uid1); err != nil {
		return fmt.Errorf("scenario 4 db update: %w", err)
	}
	r, err = c.do(ctx, http.MethodPut, "/api/users/me/username", fmt.Sprintf(`{"username":%q}`, strings.ToUpper(phase14User2Username)))
	if err != nil {
		return err
	}
	if r.status != 409 || r.code != "USERNAME_TAKEN" {
		return fmt.Errorf("scenario 4: status=%d code=%s, want 409 USERNAME_TAKEN", r.status, r.code)
	}
	log.Printf("PASS scenario 4: case-insensitive collision -> 409 USERNAME_TAKEN")

	// ----- 5. deleted_ prefix -> 400 -----
	r, err = c.do(ctx, http.MethodPut, "/api/users/me/username", `{"username":"deleted_foo"}`)
	if err != nil {
		return err
	}
	if r.status != 400 || r.code != "VALIDATION_ERROR" {
		return fmt.Errorf("scenario 5: status=%d code=%s, want 400 VALIDATION_ERROR", r.status, r.code)
	}
	log.Printf("PASS scenario 5: deleted_ prefix -> 400")

	// ----- 6. User 2 untouched -----
	var u2name string
	var u2changed sql.NullTime
	if err := dbh.QueryRowContext(ctx,
		`SELECT username, username_changed_at FROM users WHERE id = $1`, user2ID).
		Scan(&u2name, &u2changed); err != nil {
		return fmt.Errorf("scenario 6 read: %w", err)
	}
	if u2name != phase14User2Username || u2changed.Valid {
		return fmt.Errorf("scenario 6: user 2 drifted: username=%s changed_at=%v", u2name, u2changed)
	}
	log.Printf("PASS scenario 6: user 2 untouched by user 1's operations")

	return nil
}

func hasDetail(details []map[string]any, field string) bool {
	for _, d := range details {
		if f, _ := d["field"].(string); f == field {
			return true
		}
	}
	return false
}

// currentUserID decodes the JWT payload's sub claim and
// looks the user up locally (no Zitadel call needed for a
// smoke run).
func currentUserID(ctx context.Context, dbh *sql.DB, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("SMOKE_ACCESS_TOKEN is not a JWT")
	}
	payload, err := base64UrlDecodeJson(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", fmt.Errorf("JWT payload has no sub claim")
	}
	var id string
	if err := dbh.QueryRowContext(ctx, `SELECT id FROM users WHERE zitadel_id = $1`, claims.Sub).Scan(&id); err != nil {
		return "", fmt.Errorf("lookup smoke user by sub: %w", err)
	}
	return id, nil
}

// base64UrlDecodeJson decodes a padded-or-not base64url
// segment into raw JSON bytes.
func base64UrlDecodeJson(seg string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
}
