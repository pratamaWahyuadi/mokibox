// Phase 12 avatar smoke test. This exercises the deployed HTTP API, R2 CORS,
// presigned upload/download, and persisted PostgreSQL state.
//
// Strengthened checks (on top of the original flow):
//
//   - After upload-intent: the users row has pending_avatar_key ==
//     intent.AvatarKey and pending_avatar_expires_at within 1m of the API's
//     reported expiry.
//   - After the PUT to R2 and before confirm: HEAD the object (size ==
//     uploaded size) and GET it (bytes equal the PNG; decoded format "png").
//     The shared client's HeadObject does not surface the stored
//     Content-Type, so the format is verified via byte equality +
//     image.DecodeConfig.
//   - Run 2: the confirmed avatar key differs from run 1's key and the DB
//     avatar_key is replaced.
//   - Per run: PUT /api/users/me with a unique bio ("smoke-bio-<unix-nano>");
//     GET profile + DB must show the bio changed while avatar_key is
//     unchanged (catches UpdateUserProfile regressions).
//   - Negative scenarios after both runs (all must fail with the right
//     status, never 5xx):
//   - GET /api/users/me without Authorization -> 401
//   - upload-intent with content_type image/gif -> 400
//   - confirm with a random key scoped to the user (never intended) -> 4xx
//   - confirm with a key prefixed by another user's id -> 4xx
//   - replay the successful confirm (key already cleared) -> 4xx
//     After all of them the DB avatar_key must still hold the valid key.
//
// Run against an integration deployment with a dedicated test user:
//
//	API_BASE_URL=https://api.example.com \
//	SMOKE_ACCESS_TOKEN=<test-user-jwt> \
//	SMOKE_FRONTEND_ORIGIN=https://app.example.com \
//	DATABASE_URL=postgres://... \
//	R2_BUCKET=mokibox \
//	go run ./scripts/smoketest/phase12_avatar
//
// Optional R2 object checks (used by the "after PUT" check above):
//
//	R2_ACCOUNT_ID=... R2_ACCESS_KEY_ID=... R2_SECRET_ACCESS_KEY=... R2_ENDPOINT=...
//
// The smoke runs twice back-to-back. It replaces the dedicated user's avatar
// each run and leaves the last smoke image active. It only ever touches the
// authenticated test user - no other accounts.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/google/uuid"
	"github.com/pratamaWahyuadi/mokibox/shared"
)

const (
	maxAvatarSize   = 5 * 1024 * 1024
	maxResponseSize = 1 * 1024 * 1024
)

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type profile struct {
	ID        uuid.UUID `json:"id"`
	AvatarURL *string   `json:"avatar_url"`
	Bio       *string   `json:"bio"`
}

type uploadIntent struct {
	AvatarKey     string            `json:"avatar_key"`
	HTTPMethod    string            `json:"http_method"`
	UploadURL     string            `json:"upload_url"`
	UploadHeaders map[string]string `json:"upload_headers"`
	MaxSizeBytes  int64             `json:"max_size_bytes"`
	ExpiresAt     time.Time         `json:"expires_at"`
}

type confirmResponse struct {
	AvatarURL string `json:"avatar_url"`
}

type avatarRow struct {
	AvatarKey         sql.NullString
	AvatarURL         sql.NullString
	PendingAvatarKey  sql.NullString
	PendingAvatarDate sql.NullTime
}

type pendingAvatarRow struct {
	PendingAvatarKey sql.NullString
	ExpiresAt        sql.NullTime
}

type bioCheckRow struct {
	Bio       sql.NullString
	AvatarKey sql.NullString
}

type runResult struct {
	UserID       uuid.UUID
	ConfirmedKey string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL phase12_avatar:", err)
		os.Exit(1)
	}
	fmt.Println("PASS phase12_avatar (2x back-to-back)")
}

func run() error {
	apiBase := strings.TrimRight(os.Getenv("API_BASE_URL"), "/")
	token := os.Getenv("SMOKE_ACCESS_TOKEN")
	frontendOrigin := os.Getenv("SMOKE_FRONTEND_ORIGIN")
	databaseURL := os.Getenv("DATABASE_URL")
	if apiBase == "" || token == "" || frontendOrigin == "" || databaseURL == "" {
		return errors.New("API_BASE_URL, SMOKE_ACCESS_TOKEN, SMOKE_FRONTEND_ORIGIN, and DATABASE_URL are required")
	}
	bucket := os.Getenv("R2_BUCKET")
	if bucket == "" {
		return errors.New("R2_BUCKET env is required (the smoke extracts the confirmed key from the avatar URL)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	r2c, err := newR2ClientIfAvailable(ctx)
	if err != nil {
		return err
	}
	if r2c == nil {
		fmt.Println("SKIP: R2 object checks (R2_ACCOUNT_ID/R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_ENDPOINT not provided)")
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open smoke database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect smoke database: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var prev runResult
	for run := 1; run <= 2; run++ {
		res, err := runOnce(ctx, client, db, r2c, apiBase, token, frontendOrigin, bucket)
		if err != nil {
			return fmt.Errorf("run %d: %w", run, err)
		}
		if res.ConfirmedKey == "" {
			return errors.New("run completed without a confirmed avatar key")
		}
		if run > 1 && res.ConfirmedKey == prev.ConfirmedKey {
			return fmt.Errorf("run 2 confirmed the same avatar key as run 1 (%q); expected a new key", res.ConfirmedKey)
		}
		prev = res
		fmt.Printf("PASS phase12_avatar run %d\n", run)
	}

	if err := runNegativeScenarios(ctx, client, db, apiBase, token, prev); err != nil {
		return err
	}
	fmt.Println("PASS phase12_avatar negative scenarios")
	return nil
}

func runOnce(ctx context.Context, client *http.Client, db *sql.DB, r2c *shared.R2Client, apiBase, token, frontendOrigin, bucket string) (runResult, error) {
	var before profile
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodGet, token, nil, http.StatusOK, &before); err != nil {
		return runResult{}, fmt.Errorf("GET profile: %w", err)
	}

	intentBody, err := json.Marshal(map[string]string{"content_type": "image/png"})
	if err != nil {
		return runResult{}, fmt.Errorf("encode upload intent: %w", err)
	}
	var intent uploadIntent
	if err := requestJSON(ctx, client, apiBase+"/api/users/me/avatar/upload-intent", http.MethodPost, token, intentBody, http.StatusCreated, &intent); err != nil {
		return runResult{}, fmt.Errorf("create upload intent: %w", err)
	}
	if intent.HTTPMethod != http.MethodPut || intent.UploadHeaders["Content-Type"] != "image/png" ||
		intent.MaxSizeBytes != maxAvatarSize || !intent.ExpiresAt.After(time.Now()) {
		return runResult{}, errors.New("upload-intent response has invalid method, headers, size limit, or expiry")
	}
	if !strings.HasPrefix(intent.AvatarKey, "avatars/"+before.ID.String()+"/") || !strings.HasSuffix(intent.AvatarKey, ".png") {
		return runResult{}, errors.New("upload key is not scoped to the authenticated user")
	}

	// Strengthened: right after upload-intent the users row must carry
	// the issued pending key with the API-reported expiry.
	if err := assertPendingAvatarUploadRow(ctx, db, before.ID, intent.AvatarKey, intent.ExpiresAt); err != nil {
		return runResult{}, fmt.Errorf("database after upload-intent: %w", err)
	}

	pngBytes, err := testPNG()
	if err != nil {
		return runResult{}, fmt.Errorf("create PNG fixture: %w", err)
	}
	if err := checkCORS(ctx, client, intent.UploadURL, frontendOrigin); err != nil {
		return runResult{}, fmt.Errorf("R2 CORS preflight: %w", err)
	}
	if err := putObject(ctx, client, intent.UploadURL, frontendOrigin, pngBytes); err != nil {
		return runResult{}, fmt.Errorf("presigned PUT: %w", err)
	}

	// Strengthened: verify the object in R2 before confirm (HEAD size +
	// GET bytes, decoded format must be png).
	if r2c != nil {
		if err := checkR2Object(ctx, r2c, intent.AvatarKey, pngBytes); err != nil {
			return runResult{}, fmt.Errorf("R2 object after PUT (before confirm): %w", err)
		}
	}

	confirmBody, err := json.Marshal(map[string]string{"avatar_key": intent.AvatarKey})
	if err != nil {
		return runResult{}, fmt.Errorf("encode confirm: %w", err)
	}
	var confirmed confirmResponse
	if err := requestJSON(ctx, client, apiBase+"/api/users/me/avatar/confirm", http.MethodPost, token, confirmBody, http.StatusOK, &confirmed); err != nil {
		return runResult{}, fmt.Errorf("confirm upload: %w", err)
	}
	if confirmed.AvatarURL == "" {
		return runResult{}, errors.New("confirm response did not include avatar_url")
	}

	var afterConfirm profile
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodGet, token, nil, http.StatusOK, &afterConfirm); err != nil {
		return runResult{}, fmt.Errorf("GET profile after confirm: %w", err)
	}
	if afterConfirm.AvatarURL == nil {
		return runResult{}, errors.New("profile has no avatar URL after confirm")
	}

	confirmedKey, err := avatarKeyFromURL(*afterConfirm.AvatarURL, bucket)
	if err != nil {
		return runResult{}, fmt.Errorf("extract confirmed avatar key: %w", err)
	}
	if confirmedKey != intent.AvatarKey {
		return runResult{}, fmt.Errorf("confirmed key %q from avatar URL differs from upload-intent key %q", confirmedKey, intent.AvatarKey)
	}

	if err := getObject(ctx, client, *afterConfirm.AvatarURL, frontendOrigin, pngBytes); err != nil {
		return runResult{}, fmt.Errorf("presigned GET: %w", err)
	}
	if err := assertAvatarRow(ctx, db, before.ID, intent.AvatarKey); err != nil {
		return runResult{}, fmt.Errorf("database after confirm: %w", err)
	}

	echoBody, err := json.Marshal(map[string]string{"avatar_url": *afterConfirm.AvatarURL})
	if err != nil {
		return runResult{}, fmt.Errorf("encode echoed profile update: %w", err)
	}
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodPut, token, echoBody, http.StatusOK, &profile{}); err != nil {
		return runResult{}, fmt.Errorf("PUT profile with echoed presigned avatar URL: %w", err)
	}
	if err := assertAvatarRow(ctx, db, before.ID, intent.AvatarKey); err != nil {
		return runResult{}, fmt.Errorf("database after echoed PUT: %w", err)
	}

	// Strengthened: per-run bio update. GET profile + DB must show the bio
	// changed while the confirmed avatar_key is unchanged.
	bio := fmt.Sprintf("smoke-bio-%d", time.Now().UnixNano())
	bioBody, err := json.Marshal(map[string]string{"bio": bio})
	if err != nil {
		return runResult{}, fmt.Errorf("encode bio update: %w", err)
	}
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodPut, token, bioBody, http.StatusOK, &profile{}); err != nil {
		return runResult{}, fmt.Errorf("PUT profile with unique bio: %w", err)
	}
	var afterBio profile
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodGet, token, nil, http.StatusOK, &afterBio); err != nil {
		return runResult{}, fmt.Errorf("GET profile after bio update: %w", err)
	}
	if afterBio.Bio == nil || *afterBio.Bio != bio {
		return runResult{}, fmt.Errorf("profile bio after update = %q, want %q", ptrString(afterBio.Bio), bio)
	}
	var bioRow bioCheckRow
	if err := db.QueryRowContext(ctx, `SELECT bio, avatar_key FROM users WHERE id = $1`, before.ID).Scan(&bioRow.Bio, &bioRow.AvatarKey); err != nil {
		return runResult{}, fmt.Errorf("query bio/avatar_key row: %w", err)
	}
	if !bioRow.Bio.Valid || bioRow.Bio.String != bio {
		return runResult{}, fmt.Errorf("DB bio = %v, want %q", bioRow.Bio, bio)
	}
	if !bioRow.AvatarKey.Valid || bioRow.AvatarKey.String != intent.AvatarKey {
		return runResult{}, fmt.Errorf("DB avatar_key = %v, want %q (unchanged by bio update)", bioRow.AvatarKey, intent.AvatarKey)
	}

	return runResult{UserID: before.ID, ConfirmedKey: confirmedKey}, nil
}

func runNegativeScenarios(ctx context.Context, client *http.Client, db *sql.DB, apiBase, token string, res runResult) error {
	if res.ConfirmedKey == "" {
		return errors.New("negative scenarios: no confirmed avatar key available")
	}

	// 1. Unauthenticated request must be rejected with 401.
	if err := expectStatus(ctx, client, apiBase+"/api/users/me", http.MethodGet, "", nil, http.StatusUnauthorized); err != nil {
		return fmt.Errorf("negative: unauthenticated GET /api/users/me: %w", err)
	}

	// 2. upload-intent with an unsupported content type -> 400.
	gifBody, err := json.Marshal(map[string]string{"content_type": "image/gif"})
	if err != nil {
		return fmt.Errorf("encode gif upload intent: %w", err)
	}
	if err := expectStatus(ctx, client, apiBase+"/api/users/me/avatar/upload-intent", http.MethodPost, token, gifBody, http.StatusBadRequest); err != nil {
		return fmt.Errorf("negative: upload-intent with image/gif: %w", err)
	}

	// 3. confirm with a random key scoped to the user (never intended) -> 4xx.
	randomKey := fmt.Sprintf("avatars/%s/%s.png", res.UserID.String(), uuid.NewString())
	if err := confirmExpect4xx(ctx, client, apiBase, token, randomKey); err != nil {
		return fmt.Errorf("negative: confirm random scoped key: %w", err)
	}

	// 4. confirm with a key prefixed by another user's id -> 4xx.
	otherKey := fmt.Sprintf("avatars/%s/%s.png", uuid.NewString(), uuid.NewString())
	if err := confirmExpect4xx(ctx, client, apiBase, token, otherKey); err != nil {
		return fmt.Errorf("negative: confirm other-user key: %w", err)
	}

	// 5. replay the successful confirm (pending already cleared) -> 4xx.
	if err := confirmExpect4xx(ctx, client, apiBase, token, res.ConfirmedKey); err != nil {
		return fmt.Errorf("negative: replay confirm: %w", err)
	}

	// 6. DB: the active avatar_key must still be the confirmed one.
	var dbKey sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT avatar_key FROM users WHERE id = $1`, res.UserID).Scan(&dbKey); err != nil {
		return fmt.Errorf("query avatar_key after negatives: %w", err)
	}
	if !dbKey.Valid || dbKey.String != res.ConfirmedKey {
		return fmt.Errorf("DB avatar_key after negatives = %v, want %q", dbKey, res.ConfirmedKey)
	}
	return nil
}

func confirmExpect4xx(ctx context.Context, client *http.Client, apiBase, token, avatarKey string) error {
	body, err := json.Marshal(map[string]string{"avatar_key": avatarKey})
	if err != nil {
		return fmt.Errorf("encode confirm: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/users/me/avatar/confirm", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build confirm request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send confirm request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read confirm response: %w", err)
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return fmt.Errorf("confirm with key %q returned HTTP %d, want 4xx%s", avatarKey, resp.StatusCode, errorDetail(payload))
	}
	return nil
}

func expectStatus(ctx context.Context, client *http.Client, endpoint, method, token string, body []byte, wantStatus int) error {
	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed", method)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s returned HTTP %d, want %d%s", method, resp.StatusCode, wantStatus, errorDetail(payload))
	}
	return nil
}

func newR2ClientIfAvailable(ctx context.Context) (*shared.R2Client, error) {
	for _, k := range []string{"R2_ACCOUNT_ID", "R2_ACCESS_KEY_ID", "R2_SECRET_ACCESS_KEY", "R2_BUCKET", "R2_ENDPOINT"} {
		if os.Getenv(k) == "" {
			return nil, nil
		}
	}
	client, err := shared.NewR2Client(ctx, shared.R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
		Endpoint:        os.Getenv("R2_ENDPOINT"),
	})
	if err != nil {
		return nil, fmt.Errorf("build R2 client: %w", err)
	}
	return client, nil
}

func checkR2Object(ctx context.Context, client *shared.R2Client, key string, data []byte) error {
	size, err := client.HeadObject(ctx, key)
	if err != nil {
		return fmt.Errorf("HEAD avatar object: %w", err)
	}
	if size != int64(len(data)) {
		return fmt.Errorf("avatar object size = %d, want %d", size, len(data))
	}
	got, err := client.GetObject(ctx, key, maxAvatarSize)
	if err != nil {
		return fmt.Errorf("GET avatar object: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("avatar object bytes differ from uploaded PNG")
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(got)); err != nil {
		return fmt.Errorf("decode avatar object image config: %w", err)
	} else if format != "png" {
		return fmt.Errorf("avatar object format = %q, want png", format)
	}
	return nil
}

func avatarKeyFromURL(rawURL, bucket string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse avatar URL: %w", err)
	}
	prefix := "/" + bucket + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return "", fmt.Errorf("avatar URL path %q is missing %q prefix", u.Path, prefix)
	}
	return strings.TrimPrefix(u.Path, prefix), nil
}

func assertPendingAvatarUploadRow(ctx context.Context, db *sql.DB, userID uuid.UUID, key string, intentExpiresAt time.Time) error {
	var row pendingAvatarRow
	err := db.QueryRowContext(ctx, `
		SELECT pending_avatar_key, pending_avatar_expires_at
		FROM users
		WHERE id = $1
	`, userID).Scan(&row.PendingAvatarKey, &row.ExpiresAt)
	if err != nil {
		return fmt.Errorf("query pending avatar row: %w", err)
	}
	if !row.PendingAvatarKey.Valid || row.PendingAvatarKey.String != key {
		return fmt.Errorf("pending_avatar_key = %v, want %q", row.PendingAvatarKey, key)
	}
	if !row.ExpiresAt.Valid {
		return errors.New("pending_avatar_expires_at is NULL")
	}
	if diff := row.ExpiresAt.Time.Sub(intentExpiresAt); diff > time.Minute || diff < -time.Minute {
		return fmt.Errorf("pending_avatar_expires_at = %s, want within 1m of the API-reported expiry %s",
			row.ExpiresAt.Time.UTC(), intentExpiresAt.UTC())
	}
	return nil
}

func ptrString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func errorDetail(payload []byte) string {
	var env apiErrorEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return ""
	}
	msg := strings.TrimSpace(env.Error.Message)
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if env.Error.Code == "" && msg == "" {
		return ""
	}
	return fmt.Sprintf(" (code=%s, message=%q)", env.Error.Code, msg)
}

func requestJSON(ctx context.Context, client *http.Client, endpoint, method, token string, body []byte, wantStatus int, out any) error {
	var requestBody io.Reader
	if body != nil {
		requestBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed", method)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s returned HTTP %d, want %d%s", method, resp.StatusCode, wantStatus, errorDetail(payload))
	}
	if out == nil {
		return nil
	}
	var envelope apiEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode %s response envelope: %w", method, err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("%s response has no data", method)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode %s response data: %w", method, err)
	}
	return nil
}

func checkCORS(ctx context.Context, client *http.Client, uploadURL, frontendOrigin string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, uploadURL, nil)
	if err != nil {
		return errors.New("build CORS preflight request")
	}
	req.Header.Set("Origin", frontendOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPut)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("send CORS preflight request")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("preflight returned HTTP %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != frontendOrigin {
		return errors.New("R2 did not allow the configured frontend origin")
	}
	if !headerContainsToken(resp.Header.Get("Access-Control-Allow-Methods"), http.MethodPut) {
		return errors.New("R2 CORS does not allow PUT")
	}
	if !headerContainsToken(resp.Header.Get("Access-Control-Allow-Headers"), "content-type") {
		return errors.New("R2 CORS does not allow Content-Type")
	}
	return nil
}

func putObject(ctx context.Context, client *http.Client, uploadURL, frontendOrigin string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return errors.New("build R2 PUT request")
	}
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("Origin", frontendOrigin)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("send presigned R2 PUT request")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("presigned PUT returned HTTP %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != frontendOrigin {
		return errors.New("R2 PUT response did not allow the configured frontend origin")
	}
	return nil
}

func getObject(ctx context.Context, client *http.Client, signedURL, frontendOrigin string, expected []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return errors.New("build presigned GET request")
	}
	req.Header.Set("Origin", frontendOrigin)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("send presigned GET request")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("presigned GET returned HTTP %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != frontendOrigin {
		return errors.New("R2 GET response did not allow the configured frontend origin")
	}
	actual, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return errors.New("read presigned GET response")
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("downloaded avatar bytes differ from uploaded PNG")
	}
	return nil
}

func assertAvatarRow(ctx context.Context, db *sql.DB, userID uuid.UUID, key string) error {
	var row avatarRow
	err := db.QueryRowContext(ctx, `
		SELECT avatar_key, avatar_url, pending_avatar_key, pending_avatar_expires_at
		FROM users
		WHERE id = $1
	`, userID).Scan(&row.AvatarKey, &row.AvatarURL, &row.PendingAvatarKey, &row.PendingAvatarDate)
	if err != nil {
		return fmt.Errorf("query avatar row: %w", err)
	}
	if !row.AvatarKey.Valid || row.AvatarKey.String != key ||
		row.AvatarURL.Valid || row.PendingAvatarKey.Valid || row.PendingAvatarDate.Valid {
		return fmt.Errorf("avatar row did not retain the confirmed object key and clear pending fields")
	}
	return nil
}

func testPNG() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func headerContainsToken(value, expected string) bool {
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), expected) {
			return true
		}
	}
	return false
}
