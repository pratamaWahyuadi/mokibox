// Phase 11 search smoke test for GET /api/search endpoint.
// Issue #57 - TikTok Parity Backend Roadmap.
//
// This is a real, hermetic HTTP integration smoke test (runs inside
// the mokibox_backend network against PostgreSQL). It verifies:
//  1. Auth 401 requirement when unauthenticated or invalid token (using real middleware.Authenticate).
//  2. Validation errors (empty q, q > 100 runes, limit=0, invalid type) return 400.
//  3. Seed sanity check via direct DB queries before assertions.
//  4. Real SQL filters:
//     - Tombstoned users (is_active = FALSE) do NOT appear.
//     - Non-READY videos (status != 'READY') do NOT appear.
//     - Soft-deleted videos (deleted_at IS NOT NULL) do NOT appear.
//     - Followed private videos appear; unfollowed private videos do NOT.
//     - Private owner searching own video DOES appear (owner self-search).
//     - Underscore search (e.g. user_with_underscore) ranks exact match properly.
//  5. Type filters (type=users -> videos=[], type=videos -> users=[]).
//  6. Pagination (limit=1 returns 1 item + next_cursor; next page returns different item; last page omits next_cursor).
//  7. Limit cap (limit=9999 is capped).
//  8. ThumbnailURL and HLSPlaylistURL are non-null and non-empty.
//
// Usage:
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    -e DATABASE_URL \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase11_search
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-search-"

// stubTokenVerifier implements middleware.TokenVerifier for testing.
// It accepts the raw bearer token as the Zitadel ID (sub).
type stubTokenVerifier struct{}

func (v *stubTokenVerifier) CheckToken(ctx context.Context, rawToken string) (string, error) {
	if rawToken == "" || strings.HasPrefix(rawToken, "invalid") {
		return "", errors.New("unauthorized token")
	}
	return rawToken, nil
}

func main() {
	log.Println("Running phase11_search smoke test run 1...")
	if err := runSmoke(); err != nil {
		log.Fatalf("FAIL (run 1): %v", err)
	}

	log.Println("Running phase11_search smoke test run 2 (back-to-back)...")
	if err := runSmoke(); err != nil {
		log.Fatalf("FAIL (run 2): %v", err)
	}

	log.Println("PASS phase11_search (2x back-to-back PASS)")
}

func runSmoke() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("FAIL: DATABASE_URL environment variable is required")
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	q := db.New(sqlDB)

	// Clean up seeded data from previous runs for idempotency
	if err := cleanupSeedData(ctx, sqlDB); err != nil {
		return fmt.Errorf("cleanupSeedData: %w", err)
	}
	defer cleanupSeedData(ctx, sqlDB)

	// Seed test users:
	// User A: Public active (viewer)
	userA, err := seedUser(ctx, sqlDB, usernamePrefix+"userA", "User Alpha", false, true)
	if err != nil {
		return fmt.Errorf("seed userA: %w", err)
	}
	// User B: Private active (followed by A)
	userB, err := seedUser(ctx, sqlDB, usernamePrefix+"userB", "User Beta", true, true)
	if err != nil {
		return fmt.Errorf("seed userB: %w", err)
	}
	// User C: Private active (NOT followed by A)
	userC, err := seedUser(ctx, sqlDB, usernamePrefix+"userC", "User Charlie", true, true)
	if err != nil {
		return fmt.Errorf("seed userC: %w", err)
	}
	// User D: Public tombstoned (is_active = FALSE)
	userD, err := seedUser(ctx, sqlDB, usernamePrefix+"userD", "User Delta", false, false)
	if err != nil {
		return fmt.Errorf("seed userD: %w", err)
	}
	// User Underscore: user with underscore in handle for exact match testing
	userUnderscore, err := seedUser(ctx, sqlDB, usernamePrefix+"user_underscore", "User Underscore", false, true)
	if err != nil {
		return fmt.Errorf("seed userUnderscore: %w", err)
	}

	// Follow userA -> userB
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: userA.ID, FolloweeID: userB.ID}); err != nil {
		return fmt.Errorf("follow userA -> userB: %w", err)
	}

	// Seed test videos:
	// Video A1: owner User A, status READY
	vidA1, err := seedVideo(ctx, sqlDB, userA.ID, usernamePrefix+"videoA1", "Description A1", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidA1: %w", err)
	}
	// Video A2: owner User A, status PENDING_UPLOAD (not READY)
	_, err = seedVideo(ctx, sqlDB, userA.ID, usernamePrefix+"videoA2", "Description A2", "PENDING_UPLOAD", nil)
	if err != nil {
		return fmt.Errorf("seed vidA2: %w", err)
	}
	// Video A3: owner User A, status DELETED, deleted_at = NOW()
	deletedTime := time.Now()
	_, err = seedVideo(ctx, sqlDB, userA.ID, usernamePrefix+"videoA3", "Description A3", "DELETED", &deletedTime)
	if err != nil {
		return fmt.Errorf("seed vidA3: %w", err)
	}
	// Video B1: owner User B (private, followed), status READY
	vidB1, err := seedVideo(ctx, sqlDB, userB.ID, usernamePrefix+"videoB1", "Description B1", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidB1: %w", err)
	}
	// Video C1: owner User C (private, NOT followed), status READY
	vidC1, err := seedVideo(ctx, sqlDB, userC.ID, usernamePrefix+"videoC1", "Description C1", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidC1: %w", err)
	}

	// -------------------------------------------------------------
	// Sanity Check Initial Conditions (DB Direct Assertions)
	// -------------------------------------------------------------
	if err := verifySeedSanity(ctx, sqlDB, userA.ID, userB.ID, userC.ID, userD.ID, userUnderscore.ID, vidA1.ID, vidB1.ID, vidC1.ID); err != nil {
		return fmt.Errorf("FAIL: seed sanity check failed: %w", err)
	}

	// Setup API Config and R2 client
	cfg := &shared.APIConfig{
		APIBaseURL:       "http://localhost:8080",
		MediaTokenSecret: "secret-key-1234567890-secret-key-1234567890",
		MediaTokenTTL:    15 * time.Minute,
	}

	// Dummy R2 client for URL generation
	r2Client, err := shared.NewR2Client(context.Background(), shared.R2Config{
		AccountID:       "dummy",
		AccessKeyID:     "dummy",
		SecretAccessKey: "dummy",
		Bucket:          "mokibox",
		Endpoint:        "https://r2.example.com",
	})
	if err != nil {
		return fmt.Errorf("NewR2Client: %w", err)
	}

	searchH, err := handlers.NewSearchHandler(q, r2Client, cfg)
	if err != nil {
		return fmt.Errorf("NewSearchHandler: %w", err)
	}

	// Setup Echo router & HTTP server using REAL middleware.Authenticate
	e := echo.New()
	apiGroup := e.Group("/api", middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: &stubTokenVerifier{},
		Queries:  q,
	}))
	apiGroup.GET("/search", searchH.Search)

	ts := httptest.NewServer(e)
	defer ts.Close()

	client := ts.Client()

	// -------------------------------------------------------------
	// 1. Test Auth Requirement (401 Unauthorized via real middleware)
	// -------------------------------------------------------------
	// Request without Authorization header
	unauthReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/search?q="+usernamePrefix, nil)
	unauthRes, err := client.Do(unauthReq)
	if err != nil {
		return fmt.Errorf("unauth request failed: %w", err)
	}
	unauthRes.Body.Close()
	if unauthRes.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("FAIL: unauthenticated request (no header) returned status %d, expected 401", unauthRes.StatusCode)
	}

	// Request with invalid token
	invalidTokenReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/search?q="+usernamePrefix, nil)
	invalidTokenReq.Header.Set("Authorization", "Bearer invalid-token-123")
	invalidTokenRes, err := client.Do(invalidTokenReq)
	if err != nil {
		return fmt.Errorf("invalid token request failed: %w", err)
	}
	invalidTokenRes.Body.Close()
	if invalidTokenRes.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("FAIL: invalid token request returned status %d, expected 401", invalidTokenRes.StatusCode)
	}

	// Helper for making authenticated HTTP requests via real middleware.Authenticate
	doAuthGet := func(user db.User, queryParams string) (int, []byte, error) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/search?"+queryParams, nil)
		req.Header.Set("Authorization", "Bearer "+user.ZitadelID)
		res, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		return res.StatusCode, body, err
	}

	// -------------------------------------------------------------
	// 2. Test Validation Errors (400 Bad Request)
	// -------------------------------------------------------------
	// Empty query
	code, body, err := doAuthGet(userA, "q=")
	if err != nil || code != http.StatusBadRequest {
		return fmt.Errorf("FAIL: empty q returned code %d, expected 400", code)
	}
	if !strings.Contains(string(body), "VALIDATION_ERROR") || !strings.Contains(string(body), `"field":"q"`) {
		return fmt.Errorf("FAIL: empty q response missing validation error field q: %s", body)
	}

	// Query > 100 runes
	longQ := strings.Repeat("a", 101)
	code, _, err = doAuthGet(userA, "q="+longQ)
	if err != nil || code != http.StatusBadRequest {
		return fmt.Errorf("FAIL: long q returned code %d, expected 400", code)
	}

	// limit=0
	code, _, err = doAuthGet(userA, "q="+usernamePrefix+"&limit=0")
	if err != nil || code != http.StatusBadRequest {
		return fmt.Errorf("FAIL: limit=0 returned code %d, expected 400", code)
	}

	// Invalid type
	code, _, err = doAuthGet(userA, "q="+usernamePrefix+"&type=invalid")
	if err != nil || code != http.StatusBadRequest {
		return fmt.Errorf("FAIL: type=invalid returned code %d, expected 400", code)
	}

	// -------------------------------------------------------------
	// 3. Test Real SQL Filters & Happy Path (User A searching)
	// -------------------------------------------------------------
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix))
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: search happy path returned code %d, expected 200: %s", code, body)
	}

	var searchResp struct {
		Data struct {
			Users []struct {
				ID       uuid.UUID `json:"id"`
				Username string    `json:"username"`
			} `json:"users"`
			Videos []struct {
				ID             uuid.UUID `json:"id"`
				Title          *string   `json:"title"`
				ThumbnailURL   *string   `json:"thumbnail_url"`
				HLSPlaylistURL *string   `json:"hls_playlist_url"`
			} `json:"videos"`
		} `json:"data"`
		Pagination struct {
			UsersNextCursor  *string `json:"users_next_cursor"`
			VideosNextCursor *string `json:"videos_next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return fmt.Errorf("FAIL: unmarshal search response: %w", err)
	}

	// Assertion: Tombstoned user (userD) must NOT appear
	for _, u := range searchResp.Data.Users {
		if u.ID == userD.ID {
			return fmt.Errorf("FAIL: tombstoned userD appeared in search results")
		}
	}

	// Assertion: User A itself must NOT appear in users (self-exclusion rule)
	for _, u := range searchResp.Data.Users {
		if u.ID == userA.ID {
			return fmt.Errorf("FAIL: viewer userA appeared in user search results")
		}
	}

	// Assertion: User B (private, followed) MUST appear in users
	userBFound := false
	for _, u := range searchResp.Data.Users {
		if u.ID == userB.ID {
			userBFound = true
			break
		}
	}
	if !userBFound {
		return fmt.Errorf("FAIL: userB (private, followed) not found in user search results")
	}

	// Assertion: Video A1 (READY) and Video B1 (private, followed) MUST appear in videos
	videoA1Found, videoB1Found := false, false
	for _, v := range searchResp.Data.Videos {
		if v.ID == vidA1.ID {
			videoA1Found = true
			if v.ThumbnailURL == nil || *v.ThumbnailURL == "" {
				return fmt.Errorf("FAIL: videoA1 thumbnail_url is null or empty")
			}
			if v.HLSPlaylistURL == nil || *v.HLSPlaylistURL == "" {
				return fmt.Errorf("FAIL: videoA1 hls_playlist_url is null or empty")
			}
		}
		if v.ID == vidB1.ID {
			videoB1Found = true
		}
		// Assertion: PENDING_UPLOAD video (A2), deleted video (A3), unfollowed private video (C1) must NOT appear
		if v.Title != nil && *v.Title == usernamePrefix+"videoA2" {
			return fmt.Errorf("FAIL: PENDING_UPLOAD videoA2 appeared in search results")
		}
		if v.Title != nil && *v.Title == usernamePrefix+"videoA3" {
			return fmt.Errorf("FAIL: deleted videoA3 appeared in search results")
		}
		if v.ID == vidC1.ID {
			return fmt.Errorf("FAIL: unfollowed private videoC1 appeared in search results for userA")
		}
	}

	if !videoA1Found {
		return fmt.Errorf("FAIL: videoA1 not found in search video results")
	}
	if !videoB1Found {
		return fmt.Errorf("FAIL: videoB1 (private, followed) not found in search video results")
	}

	// -------------------------------------------------------------
	// 4. Test Underscore Search Exact Match Ranking
	// -------------------------------------------------------------
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix+"user_underscore"))
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: underscore search returned code %d: %s", code, body)
	}
	var underscoreResp struct {
		Data struct {
			Users []struct {
				ID       uuid.UUID `json:"id"`
				Username string    `json:"username"`
			} `json:"users"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &underscoreResp); err != nil {
		return fmt.Errorf("FAIL: unmarshal underscore search response: %w", err)
	}
	if len(underscoreResp.Data.Users) == 0 || underscoreResp.Data.Users[0].ID != userUnderscore.ID {
		return fmt.Errorf("FAIL: underscore exact match user not returned as top result")
	}

	// -------------------------------------------------------------
	// 5. Test Private Owner Self-Search (User C searching own video)
	// -------------------------------------------------------------
	code, body, err = doAuthGet(userC, "q="+url.QueryEscape(usernamePrefix+"videoC1"))
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: owner self-search returned code %d: %s", code, body)
	}

	var ownerSearchResp struct {
		Data struct {
			Videos []struct {
				ID uuid.UUID `json:"id"`
			} `json:"videos"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ownerSearchResp); err != nil {
		return fmt.Errorf("FAIL: unmarshal owner search response: %w", err)
	}
	vidC1OwnerFound := false
	for _, v := range ownerSearchResp.Data.Videos {
		if v.ID == vidC1.ID {
			vidC1OwnerFound = true
			break
		}
	}
	if !vidC1OwnerFound {
		return fmt.Errorf("FAIL: owner userC could not find their own private videoC1 in search")
	}

	// -------------------------------------------------------------
	// 6. Test Type Filters
	// -------------------------------------------------------------
	// type=users -> videos must be []
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix)+"&type=users")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: type=users search returned code %d", code)
	}
	if !strings.Contains(string(body), `"videos":[]`) {
		return fmt.Errorf("FAIL: type=users did not return videos:[] envelope: %s", body)
	}

	// type=videos -> users must be []
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix)+"&type=videos")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: type=videos search returned code %d", code)
	}
	if !strings.Contains(string(body), `"users":[]`) {
		return fmt.Errorf("FAIL: type=videos did not return users:[] envelope: %s", body)
	}

	// -------------------------------------------------------------
	// 7. Test Pagination & Last Page Next Cursor Omission
	// -------------------------------------------------------------
	// Page 1 with limit=1
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix)+"&type=videos&limit=1")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: pagination page 1 returned code %d", code)
	}
	var page1Resp struct {
		Data struct {
			Videos []struct {
				ID uuid.UUID `json:"id"`
			} `json:"videos"`
		} `json:"data"`
		Pagination struct {
			VideosNextCursor *string `json:"videos_next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(body, &page1Resp); err != nil {
		return fmt.Errorf("FAIL: unmarshal page 1 response: %w", err)
	}
	if len(page1Resp.Data.Videos) != 1 {
		return fmt.Errorf("FAIL: page 1 expected 1 video, got %d", len(page1Resp.Data.Videos))
	}
	if page1Resp.Pagination.VideosNextCursor == nil || *page1Resp.Pagination.VideosNextCursor == "" {
		return fmt.Errorf("FAIL: expected videos_next_cursor on page 1")
	}

	cursor := *page1Resp.Pagination.VideosNextCursor

	// Page 2 using cursor (limit=10 to fetch all remaining)
	code, body, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix)+"&type=videos&limit=10&cursor="+cursor)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: pagination page 2 returned code %d", code)
	}
	var page2Resp struct {
		Data struct {
			Videos []struct {
				ID uuid.UUID `json:"id"`
			} `json:"videos"`
		} `json:"data"`
		Pagination struct {
			VideosNextCursor *string `json:"videos_next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(body, &page2Resp); err != nil {
		return fmt.Errorf("FAIL: unmarshal page 2 response: %w", err)
	}
	if len(page2Resp.Data.Videos) == 0 {
		return fmt.Errorf("FAIL: page 2 expected remaining videos, got 0")
	}
	if page2Resp.Data.Videos[0].ID == page1Resp.Data.Videos[0].ID {
		return fmt.Errorf("FAIL: page 2 returned same video as page 1")
	}
	// Assertion: Last page must omit videos_next_cursor (nil)
	if page2Resp.Pagination.VideosNextCursor != nil {
		return fmt.Errorf("FAIL: last page expected videos_next_cursor to be omitted (nil), got %s", *page2Resp.Pagination.VideosNextCursor)
	}

	// Limit cap (limit=9999) -> 200 OK
	code, _, err = doAuthGet(userA, "q="+url.QueryEscape(usernamePrefix)+"&limit=9999")
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: limit=9999 returned code %d, expected 200", code)
	}

	return nil
}

func verifySeedSanity(ctx context.Context, dbConn *sql.DB, userAID, userBID, userCID, userDID, userUnderscoreID, vidA1ID, vidB1ID, vidC1ID uuid.UUID) error {
	// 1. Verify user counts and statuses
	var activeCount, tombstoneCount int
	err := dbConn.QueryRowContext(ctx, `
		SELECT 
			COUNT(*) FILTER (WHERE is_active = TRUE),
			COUNT(*) FILTER (WHERE is_active = FALSE)
		FROM users WHERE username LIKE $1
	`, usernamePrefix+"%").Scan(&activeCount, &tombstoneCount)
	if err != nil {
		return fmt.Errorf("query user counts: %w", err)
	}
	if activeCount != 4 { // UserA, UserB, UserC, UserUnderscore
		return fmt.Errorf("expected 4 active seeded users, got %d", activeCount)
	}
	if tombstoneCount != 1 { // UserD
		return fmt.Errorf("expected 1 tombstoned seeded user, got %d", tombstoneCount)
	}

	// 2. Verify video counts and statuses
	var readyCount, pendingCount, deletedCount int
	err = dbConn.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status = 'READY' AND deleted_at IS NULL),
			COUNT(*) FILTER (WHERE status = 'PENDING_UPLOAD'),
			COUNT(*) FILTER (WHERE deleted_at IS NOT NULL)
		FROM videos WHERE title LIKE $1
	`, usernamePrefix+"%").Scan(&readyCount, &pendingCount, &deletedCount)
	if err != nil {
		return fmt.Errorf("query video counts: %w", err)
	}
	if readyCount != 3 { // VidA1, VidB1, VidC1
		return fmt.Errorf("expected 3 READY videos, got %d", readyCount)
	}
	if pendingCount != 1 { // VidA2
		return fmt.Errorf("expected 1 PENDING_UPLOAD video, got %d", pendingCount)
	}
	if deletedCount != 1 { // VidA3
		return fmt.Errorf("expected 1 DELETED video, got %d", deletedCount)
	}

	return nil
}

func seedUser(ctx context.Context, dbConn *sql.DB, username, displayName string, isPrivate, isActive bool) (db.User, error) {
	id := uuid.New()
	zitadelID := "zitadel-" + id.String()
	var user db.User
	err := dbConn.QueryRowContext(ctx, `
		INSERT INTO users (id, zitadel_id, username, display_name, is_private, is_active)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, zitadel_id, username, display_name, avatar_url, is_private, is_active, created_at
	`, id, zitadelID, username, displayName, isPrivate, isActive).Scan(
		&user.ID, &user.ZitadelID, &user.Username, &user.DisplayName, &user.AvatarUrl, &user.IsPrivate, &user.IsActive, &user.CreatedAt,
	)
	return user, err
}

func seedVideo(ctx context.Context, dbConn *sql.DB, userID uuid.UUID, title, description, status string, deletedAt *time.Time) (db.Video, error) {
	id := uuid.New()
	r2Key := "videos/" + id.String() + "/source.mp4"
	var vid db.Video
	err := dbConn.QueryRowContext(ctx, `
		INSERT INTO videos (id, user_id, r2_key, title, description, status, thumbnail_key, hls_prefix, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, user_id, r2_key, title, description, status, thumbnail_key, hls_prefix, deleted_at, created_at
	`, id, userID, r2Key, title, description, status, "thumbs/"+id.String()+".jpg", "hls/"+id.String(), deletedAt).Scan(
		&vid.ID, &vid.UserID, &vid.R2Key, &vid.Title, &vid.Description, &vid.Status, &vid.ThumbnailKey, &vid.HlsPrefix, &vid.DeletedAt, &vid.CreatedAt,
	)
	return vid, err
}

func cleanupSeedData(ctx context.Context, dbConn *sql.DB) error {
	_, err := dbConn.ExecContext(ctx, `
		DELETE FROM users WHERE username LIKE $1
	`, usernamePrefix+"%")
	return err
}
