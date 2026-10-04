package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type mockSearchStore struct {
	searchUsersFn  func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error)
	searchVideosFn func(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error)
}

func (m *mockSearchStore) SearchUsers(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
	if m.searchUsersFn != nil {
		return m.searchUsersFn(ctx, arg)
	}
	return nil, nil
}

func (m *mockSearchStore) SearchVideos(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error) {
	if m.searchVideosFn != nil {
		return m.searchVideosFn(ctx, arg)
	}
	return nil, nil
}

type mockR2Store struct{}

func (m *mockR2Store) PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
	return "https://r2.example.com/put/" + key, nil
}
func (m *mockR2Store) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "https://r2.example.com/get/" + key, nil
}
func (m *mockR2Store) HeadObject(ctx context.Context, key string) (int64, error) {
	return 1024, nil
}
func (m *mockR2Store) GetObject(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	return []byte("data"), nil
}

func setupSearchTestContext(method, target string, user *db.User) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set("auth.currentUser", user)
	}
	return c, rec
}

func testConfig() *shared.APIConfig {
	return &shared.APIConfig{
		APIBaseURL:       "http://localhost:8080",
		MediaTokenSecret: "secret-key-1234567890-secret-key-1234567890",
		MediaTokenTTL:    15 * time.Minute,
	}
}

func TestSearch_HappyPath(t *testing.T) {
	viewerID := uuid.New()
	viewer := &db.User{ID: viewerID, Username: "viewer"}

	targetUserID := uuid.New()
	videoID := uuid.New()

	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			if arg.RawQuery != "pratama" {
				return nil, nil
			}
			return []db.SearchUsersRow{
				{
					ID:          targetUserID,
					Username:    "pratama",
					DisplayName: sql.NullString{String: "Pratama Wahyuadi", Valid: true},
					AvatarUrl:   sql.NullString{String: "avatars/1.png", Valid: true},
					IsPrivate:   false,
				},
			}, nil
		},
		searchVideosFn: func(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error) {
			if arg.QueryPattern != "pratama" {
				return nil, nil
			}
			return []db.SearchVideosRow{
				{
					ID:              videoID,
					UserID:          targetUserID,
					Title:           sql.NullString{String: "Pratama Video", Valid: true},
					Description:     sql.NullString{String: "Video description", Valid: true},
					R2Key:           "videos/1/source.mp4",
					HlsPrefix:       sql.NullString{String: "hls/1", Valid: true},
					ThumbnailKey:    sql.NullString{String: "thumbnails/1.jpg", Valid: true},
					DurationSeconds: sql.NullInt32{Int32: 30, Valid: true},
					Status:          "READY",
					RetryCount:      0,
					LikesCount:      10,
					ViewsCount:      100,
					CommentsCount:   5,
					CreatedAt:       time.Now(),
					UserID2:         targetUserID,
					UserUsername:    "pratama",
					UserDisplayName: sql.NullString{String: "Pratama Wahyuadi", Valid: true},
					UserAvatarUrl:   sql.NullString{String: "avatars/1.png", Valid: true},
					UserIsPrivate:   false,
					LikedByMe:       false,
				},
			}, nil
		},
	}

	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=pratama", viewer)

	if err := h.Search(c); err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var resp searchEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}

	if len(resp.Data.Users) != 1 || resp.Data.Users[0].Username != "pratama" {
		t.Errorf("expected 1 user 'pratama', got %+v", resp.Data.Users)
	}
	if len(resp.Data.Videos) != 1 || resp.Data.Videos[0].ID != videoID {
		t.Errorf("expected 1 video, got %+v", resp.Data.Videos)
	}
	if resp.Data.Videos[0].ThumbnailURL == nil || *resp.Data.Videos[0].ThumbnailURL == "" {
		t.Errorf("expected non-null thumbnail_url")
	}
	if resp.Data.Videos[0].HLSPlaylistURL == nil || *resp.Data.Videos[0].HLSPlaylistURL == "" {
		t.Errorf("expected non-null hls_playlist_url")
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	h := NewSearchHandlerForTest(&mockSearchStore{}, &mockR2Store{}, testConfig())

	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=", viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for empty query, got %d", rec.Code)
	}

	var errResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error envelope: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing error object in response")
	}
	if errObj["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected error code VALIDATION_ERROR, got %v", errObj["code"])
	}
}

func TestSearch_QueryTooLong(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	h := NewSearchHandlerForTest(&mockSearchStore{}, &mockR2Store{}, testConfig())

	longQ := strings.Repeat("a", 101)
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q="+longQ, viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for long query, got %d", rec.Code)
	}
}

func TestSearch_TypeFilter(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	userID := uuid.New()
	videoID := uuid.New()

	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			return []db.SearchUsersRow{{ID: userID, Username: "testuser"}}, nil
		},
		searchVideosFn: func(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error) {
			return []db.SearchVideosRow{{ID: videoID, Status: "READY"}}, nil
		},
	}

	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())

	// type=users -> videos must be []
	c1, rec1 := setupSearchTestContext(http.MethodGet, "/api/search?q=test&type=users", viewer)
	if err := h.Search(c1); err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	var resp1 searchEnvelope
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)
	if len(resp1.Data.Users) != 1 {
		t.Errorf("expected 1 user, got %d", len(resp1.Data.Users))
	}
	if len(resp1.Data.Videos) != 0 {
		t.Errorf("expected 0 videos for type=users, got %d", len(resp1.Data.Videos))
	}

	// type=videos -> users must be []
	c2, rec2 := setupSearchTestContext(http.MethodGet, "/api/search?q=test&type=videos", viewer)
	if err := h.Search(c2); err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	var resp2 searchEnvelope
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if len(resp2.Data.Users) != 0 {
		t.Errorf("expected 0 users for type=videos, got %d", len(resp2.Data.Users))
	}
	if len(resp2.Data.Videos) != 1 {
		t.Errorf("expected 1 video, got %d", len(resp2.Data.Videos))
	}
}

func TestSearch_TombstoneFilter(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			// SQL query filters out is_active = FALSE
			return []db.SearchUsersRow{}, nil
		},
		searchVideosFn: func(ctx context.Context, arg db.SearchVideosParams) ([]db.SearchVideosRow, error) {
			return []db.SearchVideosRow{}, nil
		},
	}

	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=tombstoned", viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	var resp searchEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data.Users) != 0 || len(resp.Data.Videos) != 0 {
		t.Errorf("expected empty search results for tombstoned user query")
	}
}

func TestSearch_Pagination(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	u1 := db.SearchUsersRow{ID: uuid.New(), Username: "user1"}
	u2 := db.SearchUsersRow{ID: uuid.New(), Username: "user2"}

	allUsers := []db.SearchUsersRow{u1, u2}
	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			start := int(arg.PageOffset)
			if start >= len(allUsers) {
				return []db.SearchUsersRow{}, nil
			}
			end := start + int(arg.PageLimit)
			if end > len(allUsers) {
				end = len(allUsers)
			}
			return allUsers[start:end], nil
		},
	}

	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())

	// Page 1 with limit=1
	c1, rec1 := setupSearchTestContext(http.MethodGet, "/api/search?q=user&type=users&limit=1", viewer)
	if err := h.Search(c1); err != nil {
		t.Fatalf("Search page 1 failed: %v", err)
	}
	var resp1 searchEnvelope
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)

	if len(resp1.Data.Users) != 1 || resp1.Data.Users[0].Username != "user1" {
		t.Fatalf("page 1 expected user1, got %+v", resp1.Data.Users)
	}
	if resp1.Pagination.UsersNextCursor == nil || *resp1.Pagination.UsersNextCursor == "" {
		t.Fatalf("expected users_next_cursor to be set")
	}

	cursor := *resp1.Pagination.UsersNextCursor

	// Page 2 with cursor
	c2, rec2 := setupSearchTestContext(http.MethodGet, "/api/search?q=user&type=users&limit=1&cursor="+cursor, viewer)
	if err := h.Search(c2); err != nil {
		t.Fatalf("Search page 2 failed: %v", err)
	}
	var resp2 searchEnvelope
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)

	if len(resp2.Data.Users) != 1 || resp2.Data.Users[0].Username != "user2" {
		t.Fatalf("page 2 expected user2, got %+v", resp2.Data.Users)
	}
}

func TestSearch_Unauthorized(t *testing.T) {
	h := NewSearchHandlerForTest(&mockSearchStore{}, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=test", nil)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rec.Code)
	}
}

func TestSearch_LimitZero(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	h := NewSearchHandlerForTest(&mockSearchStore{}, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=test&limit=0", viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for limit=0, got %d", rec.Code)
	}
}

func TestSearch_LimitCap(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	var receivedLimit int32
	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			receivedLimit = arg.PageLimit
			return nil, nil
		},
	}
	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=test&type=users&limit=9999", viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	// parseLimit caps at videoListMaxLimit (50) -> pageLimit should be limit + 1 = 51
	if receivedLimit != 51 {
		t.Errorf("expected limit capped at max 50 (plus 1 for lookahead = 51), got %d", receivedLimit)
	}
}

func TestSearch_RuneCountLimit(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	h := NewSearchHandlerForTest(&mockSearchStore{}, &mockR2Store{}, testConfig())

	// 101 runes (e.g. 101 Japanese characters, each 3 bytes)
	longRuneStr := strings.Repeat("あ", 101)
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q="+longRuneStr, viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400 for >100 runes query, got %d", rec.Code)
	}
}

func TestSearch_UnderscoreExactMatch(t *testing.T) {
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	var capturedPattern, capturedRaw string

	store := &mockSearchStore{
		searchUsersFn: func(ctx context.Context, arg db.SearchUsersParams) ([]db.SearchUsersRow, error) {
			capturedPattern = arg.QueryPattern
			capturedRaw = arg.RawQuery
			return nil, nil
		},
	}

	h := NewSearchHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupSearchTestContext(http.MethodGet, "/api/search?q=pratama_dev&type=users", viewer)
	if err := h.Search(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if capturedPattern != `pratama\_dev` {
		t.Errorf("expected QueryPattern to escape underscore to 'pratama\\_dev', got %q", capturedPattern)
	}
	if capturedRaw != "pratama_dev" {
		t.Errorf("expected RawQuery to remain unescaped 'pratama_dev', got %q", capturedRaw)
	}
}

