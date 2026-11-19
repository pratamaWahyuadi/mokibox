// Phase 12 avatar smoke test. This exercises the deployed HTTP API, R2 CORS,
// presigned upload/download, and persisted PostgreSQL state.
//
// Run against an integration deployment with a dedicated test user:
//
//	API_BASE_URL=https://api.example.com \
//	SMOKE_ACCESS_TOKEN=<test-user-jwt> \
//	SMOKE_FRONTEND_ORIGIN=https://app.example.com \
//	DATABASE_URL=postgres://... \
//	go run ./scripts/smoketest/phase12_avatar
//
// The smoke runs twice back-to-back. It replaces the dedicated user's avatar
// each run and leaves the last smoke image active.
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
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	maxAvatarSize   = 5 * 1024 * 1024
	maxResponseSize = 1 * 1024 * 1024
)

type apiEnvelope struct {
	Data json.RawMessage `json:"data"`
}

type profile struct {
	ID        uuid.UUID `json:"id"`
	AvatarURL *string   `json:"avatar_url"`
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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open smoke database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect smoke database: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	for run := 1; run <= 2; run++ {
		if err := runOnce(ctx, client, db, apiBase, token, frontendOrigin); err != nil {
			return fmt.Errorf("run %d: %w", run, err)
		}
		fmt.Printf("PASS phase12_avatar run %d\n", run)
	}
	return nil
}

func runOnce(ctx context.Context, client *http.Client, db *sql.DB, apiBase, token, frontendOrigin string) error {
	var before profile
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodGet, token, nil, http.StatusOK, &before); err != nil {
		return fmt.Errorf("GET profile: %w", err)
	}

	intentBody, err := json.Marshal(map[string]string{"content_type": "image/png"})
	if err != nil {
		return fmt.Errorf("encode upload intent: %w", err)
	}
	var intent uploadIntent
	if err := requestJSON(ctx, client, apiBase+"/api/users/me/avatar/upload-intent", http.MethodPost, token, intentBody, http.StatusCreated, &intent); err != nil {
		return fmt.Errorf("create upload intent: %w", err)
	}
	if intent.HTTPMethod != http.MethodPut || intent.UploadHeaders["Content-Type"] != "image/png" ||
		intent.MaxSizeBytes != maxAvatarSize || !intent.ExpiresAt.After(time.Now()) {
		return errors.New("upload-intent response has invalid method, headers, size limit, or expiry")
	}
	if !strings.HasPrefix(intent.AvatarKey, "avatars/"+before.ID.String()+"/") || !strings.HasSuffix(intent.AvatarKey, ".png") {
		return errors.New("upload key is not scoped to the authenticated user")
	}

	pngBytes, err := testPNG()
	if err != nil {
		return fmt.Errorf("create PNG fixture: %w", err)
	}
	if err := checkCORS(ctx, client, intent.UploadURL, frontendOrigin); err != nil {
		return fmt.Errorf("R2 CORS preflight: %w", err)
	}
	if err := putObject(ctx, client, intent.UploadURL, frontendOrigin, pngBytes); err != nil {
		return fmt.Errorf("presigned PUT: %w", err)
	}

	confirmBody, err := json.Marshal(map[string]string{"avatar_key": intent.AvatarKey})
	if err != nil {
		return fmt.Errorf("encode confirm: %w", err)
	}
	var confirmed confirmResponse
	if err := requestJSON(ctx, client, apiBase+"/api/users/me/avatar/confirm", http.MethodPost, token, confirmBody, http.StatusOK, &confirmed); err != nil {
		return fmt.Errorf("confirm upload: %w", err)
	}
	if confirmed.AvatarURL == "" {
		return errors.New("confirm response did not include avatar_url")
	}

	var afterConfirm profile
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodGet, token, nil, http.StatusOK, &afterConfirm); err != nil {
		return fmt.Errorf("GET profile after confirm: %w", err)
	}
	if afterConfirm.AvatarURL == nil {
		return errors.New("profile has no avatar URL after confirm")
	}
	if err := getObject(ctx, client, *afterConfirm.AvatarURL, frontendOrigin, pngBytes); err != nil {
		return fmt.Errorf("presigned GET: %w", err)
	}
	if err := assertAvatarRow(ctx, db, before.ID, intent.AvatarKey); err != nil {
		return fmt.Errorf("database after confirm: %w", err)
	}

	echoBody, err := json.Marshal(map[string]string{"avatar_url": *afterConfirm.AvatarURL})
	if err != nil {
		return fmt.Errorf("encode echoed profile update: %w", err)
	}
	if err := requestJSON(ctx, client, apiBase+"/api/users/me", http.MethodPut, token, echoBody, http.StatusOK, &profile{}); err != nil {
		return fmt.Errorf("PUT profile with echoed presigned avatar URL: %w", err)
	}
	if err := assertAvatarRow(ctx, db, before.ID, intent.AvatarKey); err != nil {
		return fmt.Errorf("database after echoed PUT: %w", err)
	}
	return nil
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
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s returned HTTP %d, want %d", method, resp.StatusCode, wantStatus)
	}
	if out == nil {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
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
		return errors.New("send presigned R2 GET request")
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
