// Phase 11 search smoke test for GET /api/search endpoint.
// Issue #57 - TikTok Parity Backend Roadmap.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type memorySearchStore struct {
	users  []db.SearchUsersRow
	videos []db.SearchVideosRow
}

func (m *memorySearchStore) SearchUsers(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
	var result []db.SearchUsersRow
	for _, u := range m.users {
		if strings.Contains(strings.ToLower(u.Username), strings.ToLower(arg.Query)) ||
			(u.DisplayName.Valid && strings.Contains(strings.ToLower(u.DisplayName.String), strings.ToLower(arg.Query))) {
			result = append(result, u)
		}
	}
	start := int(arg.PageOffset)
	if start >= len(result) {
		return []db.SearchUsersRow{}, nil
	}
	end := start + int(arg.PageLimit)
	if end > len(result) {
		end = len(result)
	}
	return result[start:end], nil
}

func (m *memorySearchStore) SearchVideos(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error) {
	var result []db.SearchVideosRow
	for _, v := range m.videos {
		if v.Status != "READY" {
			continue
		}
		title := ""
		if v.Title.Valid {
			title = v.Title.String
		}
		desc := ""
		if v.Description.Valid {
			desc = v.Description.String
		}
		if strings.Contains(strings.ToLower(title), strings.ToLower(arg.Query)) ||
			strings.Contains(strings.ToLower(desc), strings.ToLower(arg.Query)) {
			result = append(result, v)
		}
	}
	start := int(arg.PageOffset)
	if start >= len(result) {
		return []db.SearchVideosRow{}, nil
	}
	end := start + int(arg.PageLimit)
	if end > len(result) {
		end = len(result)
	}
	return result[start:end], nil
}

type dummyR2Store struct{}

func (d *dummyR2Store) PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
	return "https://r2.example.com/put/" + key, nil
}
func (d *dummyR2Store) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "https://r2.example.com/get/" + key, nil
}
func (d *dummyR2Store) HeadObject(ctx context.Context, key string) (int64, error) {
	return 1024, nil
}
func (d *dummyR2Store) GetObject(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	return []byte("data"), nil
}

func main() {
	log.Println("Running phase11_search smoke test run 1...")
	if err := runSmoke(); err != nil {
		log.Fatalf("FAIL (run 1): %v", err)
	}

	log.Println("Running phase11_search smoke test run 2 (back-to-back state reset check)...")
	if err := runSmoke(); err != nil {
		log.Fatalf("FAIL (run 2): %v", err)
	}

	log.Println("PASS phase11_search")
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

func runSmoke() error {
	viewerID := uuid.New()
	targetUserID1 := uuid.New()
	targetUserID2 := uuid.New()
	videoID1 := uuid.New()
	videoID2 := uuid.New()

	// State setup (reset on each run)
	store := &memorySearchStore{
		users: []db.SearchUsersRow{
			{
				ID:          targetUserID1,
				Username:    "pratama_wahyu",
				DisplayName: nullStr("Pratama Wahyuadi"),
				IsPrivate:   false,
			},
			{
				ID:          targetUserID2,
				Username:    "pratama_dev",
				DisplayName: nullStr("Pratama Developer"),
				IsPrivate:   false,
			},
		},
		videos: []db.SearchVideosRow{
			{
				ID:              videoID1,
				UserID:          targetUserID1,
				Title:           nullStr("Pratama tutorial video"),
				Description:     nullStr("Learn Go backend development with pratama"),
				Status:          "READY",
				ThumbnailKey:    nullStr("thumb1.jpg"),
				CreatedAt:       time.Now(),
				UserUsername:    "pratama_wahyu",
				UserDisplayName: nullStr("Pratama Wahyuadi"),
				UserIsPrivate:   false,
			},
			{
				ID:              videoID2,
				UserID:          targetUserID2,
				Title:           nullStr("Pratama second video"),
				Description:     nullStr("Another great clip by pratama"),
				Status:          "READY",
				ThumbnailKey:    nullStr("thumb2.jpg"),
				CreatedAt:       time.Now(),
				UserUsername:    "pratama_dev",
				UserDisplayName: nullStr("Pratama Developer"),
				UserIsPrivate:   false,
			},
		},
	}

	cfg := &shared.APIConfig{
		APIBaseURL:       "http://localhost:8080",
		MediaTokenSecret: "secret-key-1234567890-secret-key-1234567890",
		MediaTokenTTL:    15 * time.Minute,
	}

	e := echo.New()

	// Mount auth mock middleware
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("auth.currentUser", &db.User{ID: viewerID, Username: "viewer"})
			return next(c)
		}
	})

	testHandler := handlers.NewSearchHandlerForTest(store, &dummyR2Store{}, cfg)
	e.GET("/api/search", testHandler.Search)

	ts := httptest.NewServer(e)
	defer ts.Close()

	client := ts.Client()

	// 1. Happy path: GET /api/search?q=pratama
	{
		res, err := client.Get(ts.URL + "/api/search?q=pratama")
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("happy path expected 200, got %d, body: %s", res.StatusCode, string(body))
		}

		var envelope map[string]interface{}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("failed to unmarshal JSON: %w", err)
		}

		data, ok := envelope["data"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("response missing data section")
		}

		users, ok := data["users"].([]interface{})
		if !ok || len(users) != 2 {
			return fmt.Errorf("expected 2 users in search results, got %v", users)
		}

		videos, ok := data["videos"].([]interface{})
		if !ok || len(videos) != 2 {
			return fmt.Errorf("expected 2 videos in search results, got %v", videos)
		}
	}

	// 2. Empty query validation error: q=""
	{
		res, err := client.Get(ts.URL + "/api/search?q=")
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("empty query expected 400, got %d, body: %s", res.StatusCode, string(body))
		}

		var errEnvelope map[string]interface{}
		if err := json.Unmarshal(body, &errEnvelope); err != nil {
			return fmt.Errorf("failed to unmarshal error JSON: %w", err)
		}
		errObj, ok := errEnvelope["error"].(map[string]interface{})
		if !ok || errObj["code"] != "VALIDATION_ERROR" {
			return fmt.Errorf("expected VALIDATION_ERROR code, got %v", errEnvelope)
		}
	}

	// 3. Query too long: > 100 chars
	{
		longQ := strings.Repeat("a", 101)
		res, err := client.Get(ts.URL + "/api/search?q=" + url.QueryEscape(longQ))
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("long query expected 400, got %d, body: %s", res.StatusCode, string(body))
		}
	}

	// 4. Type filter: type=users
	{
		res, err := client.Get(ts.URL + "/api/search?q=pratama&type=users")
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("type=users expected 200, got %d, body: %s", res.StatusCode, string(body))
		}

		var envelope map[string]interface{}
		_ = json.Unmarshal(body, &envelope)
		data := envelope["data"].(map[string]interface{})
		videos := data["videos"].([]interface{})
		if len(videos) != 0 {
			return fmt.Errorf("expected videos=[] for type=users, got %d items", len(videos))
		}
	}

	// 5. Type filter: type=videos
	{
		res, err := client.Get(ts.URL + "/api/search?q=pratama&type=videos")
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()

		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("type=videos expected 200, got %d, body: %s", res.StatusCode, string(body))
		}

		var envelope map[string]interface{}
		_ = json.Unmarshal(body, &envelope)
		data := envelope["data"].(map[string]interface{})
		users := data["users"].([]interface{})
		if len(users) != 0 {
			return fmt.Errorf("expected users=[] for type=videos, got %d items", len(users))
		}
	}

	// 6. Pagination test: limit=1
	{
		res1, err := client.Get(ts.URL + "/api/search?q=pratama&type=users&limit=1")
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body1, _ := io.ReadAll(res1.Body)
		res1.Body.Close()

		var env1 map[string]interface{}
		_ = json.Unmarshal(body1, &env1)
		pag1, ok := env1["pagination"].(map[string]interface{})
		if !ok || pag1["users_next_cursor"] == nil {
			return fmt.Errorf("expected users_next_cursor in pagination for limit=1, got %v", env1)
		}

		nextCursor := pag1["users_next_cursor"].(string)

		// Page 2
		res2, err := client.Get(ts.URL + "/api/search?q=pratama&type=users&limit=1&cursor=" + url.QueryEscape(nextCursor))
		if err != nil {
			return fmt.Errorf("HTTP GET failed: %w", err)
		}
		body2, _ := io.ReadAll(res2.Body)
		res2.Body.Close()

		var env2 map[string]interface{}
		_ = json.Unmarshal(body2, &env2)
		data2 := env2["data"].(map[string]interface{})
		users2 := data2["users"].([]interface{})
		if len(users2) != 1 {
			return fmt.Errorf("expected 1 user on page 2, got %d", len(users2))
		}
		user2Obj := users2[0].(map[string]interface{})
		if user2Obj["username"] != "pratama_dev" {
			return fmt.Errorf("expected user2 'pratama_dev', got %v", user2Obj["username"])
		}
	}

	return nil
}
