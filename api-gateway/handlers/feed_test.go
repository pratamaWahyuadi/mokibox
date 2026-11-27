// feed_test.go - tests for the home feed endpoint with
// OPTIONAL auth (Fitur A, Tahap 3): anonymous requests get
// a 200 public timeline (NOT 401), logged-in viewers keep
// their personal feed, and the Vary header is ADDED (K8).
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type mockFeedStore struct {
	listFeedFn func(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error)
}

func (m *mockFeedStore) ListFeedVideos(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error) {
	if m.listFeedFn != nil {
		return m.listFeedFn(ctx, arg)
	}
	return nil, nil
}

func setupFeedTestContext(method, target string, user *db.User) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set("auth.currentUser", user)
	}
	return c, rec
}

func feedRow(videoID, ownerID uuid.UUID, likedByMe bool, private bool) db.ListFeedVideosRow {
	return db.ListFeedVideosRow{
		ID:              videoID,
		UserID:          ownerID,
		Status:          "READY",
		CreatedAt:       time.Now(),
		UserID2:         ownerID,
		UserUsername:    "owner",
		UserIsPrivate:   private,
		LikedByMe:       likedByMe,
	}
}

func TestFeedHome_AnonymousReturns200(t *testing.T) {
	// Fitur A: no user on the context -> public timeline
	// with 200 (previously 401). ViewerID must be NULL and
	// liked_by_me must be false for every row.
	videoID := uuid.New()
	ownerID := uuid.New()
	var captured db.ListFeedVideosParams
	store := &mockFeedStore{
		listFeedFn: func(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error) {
			captured = arg
			return []db.ListFeedVideosRow{feedRow(videoID, ownerID, false, false)}, nil
		},
	}
	h := newFeedHandlerForTest(store, &mockR2Store{}, testConfig())

	c, rec := setupFeedTestContext(http.MethodGet, "/api/feed/home", nil)
	if err := h.HomeFeed(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous feed must be 200, got %d", rec.Code)
	}
	if captured.ViewerID.Valid || captured.ViewerID.UUID != uuid.Nil {
		t.Errorf("anonymous feed must carry NULL viewer_id, got %+v", captured.ViewerID)
	}

	var resp struct {
		Data []VideoObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("expected 1 video, got %d", len(resp.Data))
	}
	if resp.Data[0].LikedByMe {
		t.Errorf("anonymous liked_by_me must be false")
	}
	if resp.Data[0].IsOwner {
		t.Errorf("anonymous is_owner must be false")
	}
	if vary := rec.Header().Get("Vary"); !strings.Contains(vary, "Authorization") {
		t.Errorf("expected Vary to include Authorization, got %q", vary)
	}
}

func TestFeedHome_ViewerPersonalResults(t *testing.T) {
	// Logged-in viewer: ViewerID set; liked_by_me comes
	// from SQL per row.
	viewerID := uuid.New()
	viewer := &db.User{ID: viewerID, Username: "viewer"}
	videoID := uuid.New()
	ownerID := uuid.New()

	var captured db.ListFeedVideosParams
	store := &mockFeedStore{
		listFeedFn: func(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error) {
			captured = arg
			return []db.ListFeedVideosRow{feedRow(videoID, ownerID, true, false)}, nil
		},
	}

	h := newFeedHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupFeedTestContext(http.MethodGet, "/api/feed/home", viewer)
	if err := h.HomeFeed(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !captured.ViewerID.Valid || captured.ViewerID.UUID != viewerID {
		t.Errorf("feed must receive the viewer id, got %+v", captured.ViewerID)
	}

	var resp struct {
		Data []VideoObject `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Data) != 1 || !resp.Data[0].LikedByMe {
		t.Errorf("viewer liked_by_me must come from SQL, got %+v", resp.Data)
	}
	if resp.Data[0].IsOwner {
		t.Errorf("feed is_owner is always false")
	}
}

func TestFeedHome_VaryHeaderAddPreservesExistingVary(t *testing.T) {
	// K8: Vary must be ADDED so a pre-existing Origin value
	// survives (nginx sets Vary: Origin).
	viewer := &db.User{ID: uuid.New(), Username: "viewer"}
	store := &mockFeedStore{}
	h := newFeedHandlerForTest(store, &mockR2Store{}, testConfig())
	c, rec := setupFeedTestContext(http.MethodGet, "/api/feed/home", viewer)
	c.Response().Header().Add(echo.HeaderVary, "Origin")
	if err := h.HomeFeed(c); err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	joined := strings.Join(rec.Header().Values("Vary"), ", ")
	if !strings.Contains(joined, "Origin") || !strings.Contains(joined, "Authorization") {
		t.Fatalf("Vary must contain both Origin and Authorization, got %q", joined)
	}
}
