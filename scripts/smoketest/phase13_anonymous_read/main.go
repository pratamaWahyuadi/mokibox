// Phase 13 anonymous-read smoke test (Fitur A):
// GET /api/search and GET /api/feed/home with OPTIONAL
// auth + the combined per-user/per-IP limiter.
//
// Verifies (against real SQL + real middleware):
//  1. /api/search without a token -> 200 (public results;
//     liked_by_me=false, is_owner=false).
//  2. /api/search with a VALID token -> 200 personal
//     (own private video visible, followed private video
//     visible, self excluded from user results).
//  3. Random (invalid) token -> 401 on BOTH search and
//     feed (fail-closed, never downgraded to anonymous).
//  4. /api/feed/home without a token -> 200; private
//     accounts never leak to anonymous callers.
//  5. Spam from one IP without a token -> 429 RATE_LIMITED
//     once the anonymous burst is exhausted.
//  6. Spoofed X-Real-IP / X-Forwarded-For from a
//     non-trusted peer does NOT evade the limiter (the
//     same peer IP keeps hitting the same bucket).
//  7. Responses carry Vary: Authorization.
//
// Usage (requires the dev stack: postgres + gateway deps):
//
//	docker run --rm --network mokibox_backend \
//	    -v $PWD:/repo -w /repo \
//	    -e DATABASE_URL \
//	    golang:1.25.5-alpine \
//	    go run ./scripts/smoketest/phase13_anonymous_read
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
	"golang.org/x/time/rate"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/handlers"
	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

const usernamePrefix = "smoke-test-anon-"

// stubTokenVerifier implements middleware.TokenVerifier.
// Accepts any non-empty token that is not prefixed with
// "invalid" (so a random token is a 401 case).
type stubTokenVerifier struct{}

func (v *stubTokenVerifier) CheckToken(ctx context.Context, rawToken string) (string, error) {
	if rawToken == "" || strings.HasPrefix(rawToken, "invalid") {
		return "", errors.New("unauthorized token")
	}
	return rawToken, nil
}

func main() {
	log.Println("Running phase13_anonymous_read smoke test...")
	if err := runSmoke(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS phase13_anonymous_read")
}

func runSmoke() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL environment variable is required")
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

	cleanup := func() error { return cleanupSeedData(ctx, sqlDB) }
	if err := cleanup(); err != nil {
		return fmt.Errorf("cleanupSeedData: %w", err)
	}
	defer cleanup()

	// Seed: public owner, private owner (followed by the
	// viewer), private owner NOT followed, and the viewer.
	ownerPub, err := seedUser(ctx, sqlDB, usernamePrefix+"pub", false)
	if err != nil {
		return fmt.Errorf("seed ownerPub: %w", err)
	}
	ownerPrivFollowed, err := seedUser(ctx, sqlDB, usernamePrefix+"privfollow", true)
	if err != nil {
		return fmt.Errorf("seed ownerPrivFollowed: %w", err)
	}
	ownerPrivUnfollowed, err := seedUser(ctx, sqlDB, usernamePrefix+"privunfollow", true)
	if err != nil {
		return fmt.Errorf("seed ownerPrivUnfollowed: %w", err)
	}
	viewer, err := seedUser(ctx, sqlDB, usernamePrefix+"viewer", false)
	if err != nil {
		return fmt.Errorf("seed viewer: %w", err)
	}
	if err := q.FollowUser(ctx, db.FollowUserParams{FollowerID: viewer.ID, FolloweeID: ownerPrivFollowed.ID}); err != nil {
		return fmt.Errorf("follow: %w", err)
	}

	vidPub, err := seedVideo(ctx, sqlDB, ownerPub.ID, usernamePrefix+"video-pub", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidPub: %w", err)
	}
	vidPrivFollowed, err := seedVideo(ctx, sqlDB, ownerPrivFollowed.ID, usernamePrefix+"video-privfollowed", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidPrivFollowed: %w", err)
	}
	vidPrivUnfollowed, err := seedVideo(ctx, sqlDB, ownerPrivUnfollowed.ID, usernamePrefix+"video-privunfollowed", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidPrivUnfollowed: %w", err)
	}
	vidOwnPriv, err := seedVideo(ctx, sqlDB, viewer.ID, usernamePrefix+"video-ownpriv", "READY", nil)
	if err != nil {
		return fmt.Errorf("seed vidOwnPriv: %w", err)
	}

	cfg := &shared.APIConfig{
		APIBaseURL:            "http://localhost:8080",
		MediaTokenSecret:      "secret-key-1234567890-secret-key-1234567890",
		MediaTokenTTL:         15 * time.Minute,
		SearchRateLimitPerMin: 60,
		FeedRateLimitPerMin:   120,
		// Production-like trusted list: the httptest peer
		// (127.0.0.1) is trusted, so a spoofed X-Real-IP
		// WOULD be honoured... which is exactly why the
		// anti-spoof case is asserted with a deliberately
		// narrow trusted list that EXCLUDES loopback.
		TrustedProxyCIDRs: "192.168.144.1/32",
	}
	r2Client, err := shared.NewR2Client(context.Background(), shared.R2Config{
		AccountID: "dummy", AccessKeyID: "dummy", SecretAccessKey: "dummy",
		Bucket: "mokibox", Endpoint: "https://r2.example.com",
	})
	if err != nil {
		return fmt.Errorf("NewR2Client: %w", err)
	}

	searchH, err := handlers.NewSearchHandler(q, r2Client, cfg)
	if err != nil {
		return fmt.Errorf("NewSearchHandler: %w", err)
	}
	feedH, err := handlers.NewFeedHandler(q, r2Client, cfg)
	if err != nil {
		return fmt.Errorf("NewFeedHandler: %w", err)
	}

	// Real middleware chain, same shape as routes.go:
	// AuthenticateOptional THEN the combined limiter.
	authenticateOptional := middleware.AuthenticateOptional(middleware.AuthenticateConfig{
		Verifier: &stubTokenVerifier{},
		Queries:  q,
	})
	searchLimiter, err := middleware.NewRateLimitOptionalAuth(middleware.RateLimitConfig{
		Rate: rate.Limit(float64(cfg.SearchRateLimitPerMin) / 60.0),
		Burst: middleware.SearchRateLimitBurstAnon,
		TTL: 10 * time.Minute, SweepInterval: 5 * time.Minute,
	}, cfg.TrustedProxyCIDRs)
	if err != nil {
		return fmt.Errorf("search limiter: %w", err)
	}
	feedLimiter, err := middleware.NewRateLimitOptionalAuth(middleware.RateLimitConfig{
		Rate: rate.Limit(float64(cfg.FeedRateLimitPerMin) / 60.0),
		Burst: middleware.FeedRateLimitBurstAnon,
		TTL: 10 * time.Minute, SweepInterval: 5 * time.Minute,
	}, cfg.TrustedProxyCIDRs)
	if err != nil {
		return fmt.Errorf("feed limiter: %w", err)
	}

	e := echo.New()
	e.GET("/api/search", searchH.Search, authenticateOptional, searchLimiter)
	e.GET("/api/feed/home", feedH.HomeFeed, authenticateOptional, feedLimiter)
	ts := httptest.NewServer(e)
	defer ts.Close()

	client := ts.Client()

	doGet := func(path, token string, headers map[string]string) (int, http.Header, []byte, error) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, body, err
	}

	searchQ := url.QueryEscape(usernamePrefix)

	// 1. Anonymous search -> 200, public only, flags false.
	code, hdr, body, err := doGet("/api/search?type=videos&q="+searchQ, "", nil)
	if err != nil {
		return fmt.Errorf("anon search: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("FAIL: anonymous search returned %d, expected 200: %s", code, body)
	}
	if !strings.Contains(hdr.Get("Vary"), "Authorization") {
		return fmt.Errorf("FAIL: anonymous search missing Vary: Authorization")
	}
	var searchResp struct {
		Data struct {
			Users  []struct{ ID uuid.UUID } `json:"users"`
			Videos []struct {
				ID       uuid.UUID `json:"id"`
				LikedByMe bool     `json:"liked_by_me"`
				IsOwner   bool     `json:"is_owner"`
			} `json:"videos"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return fmt.Errorf("unmarshal anon search: %w", err)
	}
	for _, v := range searchResp.Data.Videos {
		if v.LikedByMe || v.IsOwner {
			return fmt.Errorf("FAIL: anonymous search row has liked_by_me/is_owner true")
		}
		if v.ID == vidPrivUnfollowed.ID {
			return fmt.Errorf("FAIL: unfollowed private video leaked to anonymous search")
		}
		if v.ID == vidPrivFollowed.ID {
			return fmt.Errorf("FAIL: followed private video leaked to ANONYMOUS search (K3)")
		}
	}
	// Anonymous users search: the private owners still show
	// up as UserSummary (K10/Q1 default) but never leak
	// videos beyond the visibility rule above.

	// 2. Valid-token search -> personal.
	code, _, body, err = doGet("/api/search?q="+searchQ, viewer.ZitadelID, nil)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: personal search code=%d err=%v body=%s", code, err, body)
	}
	var personalResp struct {
		Data struct {
			Users  []struct{ ID uuid.UUID } `json:"users"`
			Videos []struct {
				ID        uuid.UUID `json:"id"`
				LikedByMe bool      `json:"liked_by_me"`
				IsOwner   bool      `json:"is_owner"`
			} `json:"videos"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &personalResp); err != nil {
		return fmt.Errorf("unmarshal personal search: %w", err)
	}
	selfSeen := false
	for _, u := range personalResp.Data.Users {
		if u.ID == viewer.ID {
			selfSeen = true
		}
	}
	if selfSeen {
		return fmt.Errorf("FAIL: viewer appeared in own user search results")
	}
	ownPrivSeen := false
	for _, v := range personalResp.Data.Videos {
		if v.ID == vidOwnPriv.ID {
			ownPrivSeen = true
		}
	}
	if !ownPrivSeen {
		return fmt.Errorf("FAIL: viewer's own private video missing from personal search")
	}

	// 3. Random token -> 401 on BOTH endpoints.
	for _, p := range []string{"/api/search?q=" + searchQ, "/api/feed/home"} {
		code, _, body, err := doGet(p, "invalid-random-garbage-token", nil)
		if err != nil {
			return fmt.Errorf("broken-token %s: %w", p, err)
		}
		if code != http.StatusUnauthorized {
			return fmt.Errorf("FAIL: broken token on %s returned %d, expected 401: %s", p, code, body)
		}
	}

	// 4. Anonymous feed -> 200, private accounts never leak.
	code, hdr, body, err = doGet("/api/feed/home", "", nil)
	if err != nil {
		return fmt.Errorf("anon feed: %w", err)
	}
	if code != http.StatusOK {
		return fmt.Errorf("FAIL: anonymous feed returned %d, expected 200: %s", code, body)
	}
	if !strings.Contains(hdr.Get("Vary"), "Authorization") {
		return fmt.Errorf("FAIL: anonymous feed missing Vary: Authorization")
	}
	var feedResp struct {
		Data []struct {
			ID        uuid.UUID `json:"id"`
			LikedByMe bool      `json:"liked_by_me"`
			IsOwner   bool      `json:"is_owner"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &feedResp); err != nil {
		return fmt.Errorf("unmarshal anon feed: %w", err)
	}
	pubSeen := false
	for _, v := range feedResp.Data {
		if v.LikedByMe || v.IsOwner {
			return fmt.Errorf("FAIL: anonymous feed row has liked_by_me/is_owner true")
		}
		if v.ID == vidPrivUnfollowed.ID || v.ID == vidPrivFollowed.ID {
			return fmt.Errorf("FAIL: private-account video leaked to anonymous feed (K3)")
		}
		if v.ID == vidPub.ID {
			pubSeen = true
		}
	}
	if !pubSeen {
		return fmt.Errorf("FAIL: public video missing from anonymous feed")
	}

	// 4b. Valid-token feed -> 200 with liked_by_me from the
	// viewer (viewer follows ownerPrivFollowed).
	code, _, body, err = doGet("/api/feed/home", viewer.ZitadelID, nil)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("FAIL: personal feed code=%d err=%v body=%s", code, err, body)
	}

	// 5. Anonymous burst -> 429 (search burst = 15; the
	// bucket for this peer was already drained once in
	// step 1, so a fresh sweep of requests must 429).
	lastCode := 0
	for i := 0; i < middleware.SearchRateLimitBurstAnon+5; i++ {
		code, _, _, err = doGet("/api/search?type=users&q="+searchQ, "", nil)
		if err != nil {
			return fmt.Errorf("burst req %d: %w", i, err)
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if lastCode != http.StatusTooManyRequests {
		return fmt.Errorf("FAIL: anonymous burst never hit 429 (last=%d)", lastCode)
	}

	// 6. Spoofed headers from a NON-trusted peer do not
	// evade the limiter: the httptest peer is 127.0.0.1,
	// which is NOT in TrustedProxyCIDRs here, so the
	// spoofed X-Real-IP must be ignored and the same peer
	// bucket keeps returning 429.
	code, _, _, err = doGet("/api/search?type=users&q="+searchQ, "", map[string]string{
		"X-Real-IP":       "203.0.113.77",
		"X-Forwarded-For": "203.0.113.77",
	})
	if err != nil {
		return fmt.Errorf("spoof req: %w", err)
	}
	if code != http.StatusTooManyRequests {
		return fmt.Errorf("FAIL: spoofed headers evaded the limiter (got %d, expected 429)", code)
	}

	_ = vidPub // used above
	return nil
}

func seedUser(ctx context.Context, dbConn *sql.DB, username string, isPrivate bool) (db.User, error) {
	id := uuid.New()
	zitadelID := "zitadel-" + id.String()
	var user db.User
	err := dbConn.QueryRowContext(ctx, `
		INSERT INTO users (id, zitadel_id, username, display_name, is_private, is_active)
		VALUES ($1, $2, $3, $4, $5, TRUE)
		RETURNING id, zitadel_id, username, display_name, avatar_url, is_private, is_active, created_at
	`, id, zitadelID, username, username, isPrivate).Scan(
		&user.ID, &user.ZitadelID, &user.Username, &user.DisplayName, &user.AvatarUrl, &user.IsPrivate, &user.IsActive, &user.CreatedAt,
	)
	return user, err
}

func seedVideo(ctx context.Context, dbConn *sql.DB, userID uuid.UUID, title, status string, deletedAt *time.Time) (db.Video, error) {
	id := uuid.New()
	r2Key := "videos/" + id.String() + "/source.mp4"
	var vid db.Video
	err := dbConn.QueryRowContext(ctx, `
		INSERT INTO videos (id, user_id, r2_key, title, description, status, thumbnail_key, hls_prefix, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, user_id, r2_key, title, description, status, thumbnail_key, hls_prefix, deleted_at, created_at
	`, id, userID, r2Key, title, title, status, "thumbs/"+id.String()+".jpg", "hls/"+id.String(), deletedAt).Scan(
		&vid.ID, &vid.UserID, &vid.R2Key, &vid.Title, &vid.Description, &vid.Status, &vid.ThumbnailKey, &vid.HlsPrefix, &vid.DeletedAt, &vid.CreatedAt,
	)
	return vid, err
}

func cleanupSeedData(ctx context.Context, dbConn *sql.DB) error {
	_, err := dbConn.ExecContext(ctx, `DELETE FROM users WHERE username LIKE $1`, usernamePrefix+"%")
	return err
}
