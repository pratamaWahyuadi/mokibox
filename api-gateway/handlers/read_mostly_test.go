// Package handlers - read_mostly_test.go covers the
// batch-3 read-mostly handlers: user profile + follow
// endpoints, video detail + status + playlist visibility,
// notification inbox, home feed, and the Zitadel webhook
// (signature verified with the REAL actions helper).
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/zitadel/zitadel-go/v3/pkg/actions"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// ---------------------------------------------------------------------------
// userStore stub (user.go + user_follow.go methods).
// ---------------------------------------------------------------------------

type stubUserStore struct {
	getVideoByID       func(ctx context.Context, id uuid.UUID) (db.Video, error)
	getUserByID        func(ctx context.Context, id uuid.UUID) (db.User, error)
	isFollowing        func(ctx context.Context, arg db.IsFollowingParams) (bool, error)
	getProfileWithStats func(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error)
	updateProfile      func(ctx context.Context, arg db.UpdateUserProfileParams) (db.User, error)
	listVideosByUser   func(ctx context.Context, arg db.ListVideosByUserParams) ([]db.Video, error)
	followUser         func(ctx context.Context, arg db.FollowUserParams) error
	getFollow          func(ctx context.Context, arg db.GetFollowParams) (db.Follow, error)
	deleteFollow       func(ctx context.Context, arg db.DeleteFollowParams) error
	listFollowers      func(ctx context.Context, arg db.ListFollowersParams) ([]db.ListFollowersRow, error)
	listFollowing      func(ctx context.Context, arg db.ListFollowingParams) ([]db.ListFollowingRow, error)
	insertNotification func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error)
	// videoStore surface (satisfied alongside userStore so the
	// video-detail tests can reuse this stub).
	getVideoDetail     func(ctx context.Context, arg db.GetVideoDetailParams) (db.GetVideoDetailRow, error)

	notifCalls []db.InsertNotificationParams
}

// The remaining videoStore methods a read-only test never
// scripts: they fail loudly if hit by mistake.
func (s *stubUserStore) GetPendingVideoByUser(ctx context.Context, userID uuid.UUID) (db.Video, error) {
	return db.Video{}, errors.New("stubUserStore: GetPendingVideoByUser not scripted")
}
func (s *stubUserStore) InsertVideo(ctx context.Context, arg db.InsertVideoParams) (db.Video, error) {
	return db.Video{}, errors.New("stubUserStore: InsertVideo not scripted")
}
func (s *stubUserStore) UpdatePendingVideoR2Key(ctx context.Context, arg db.UpdatePendingVideoR2KeyParams) (db.Video, error) {
	return db.Video{}, errors.New("stubUserStore: UpdatePendingVideoR2Key not scripted")
}
func (s *stubUserStore) UpdatePendingVideoMetadata(ctx context.Context, arg db.UpdatePendingVideoMetadataParams) (db.Video, error) {
	return db.Video{}, errors.New("stubUserStore: UpdatePendingVideoMetadata not scripted")
}
func (s *stubUserStore) MarkVideoDeleted(ctx context.Context, arg db.MarkVideoDeletedParams) (db.Video, error) {
	return db.Video{}, errors.New("stubUserStore: MarkVideoDeleted not scripted")
}
func (s *stubUserStore) GetVideoDetail(ctx context.Context, arg db.GetVideoDetailParams) (db.GetVideoDetailRow, error) {
	if s.getVideoDetail == nil {
		return db.GetVideoDetailRow{}, errors.New("stubUserStore: getVideoDetail not scripted")
	}
	return s.getVideoDetail(ctx, arg)
}

func (s *stubUserStore) GetVideoByID(ctx context.Context, id uuid.UUID) (db.Video, error) {
	if s.getVideoByID == nil {
		return db.Video{}, errors.New("stubUserStore: getVideoByID not scripted")
	}
	return s.getVideoByID(ctx, id)
}
func (s *stubUserStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.getUserByID == nil {
		return db.User{}, errors.New("stubUserStore: getUserByID not scripted")
	}
	return s.getUserByID(ctx, id)
}
func (s *stubUserStore) IsFollowing(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
	if s.isFollowing == nil {
		return false, errors.New("stubUserStore: isFollowing not scripted")
	}
	return s.isFollowing(ctx, arg)
}
func (s *stubUserStore) GetUserProfileWithStats(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error) {
	if s.getProfileWithStats == nil {
		return db.GetUserProfileWithStatsRow{}, errors.New("stubUserStore: getProfileWithStats not scripted")
	}
	return s.getProfileWithStats(ctx, arg)
}
func (s *stubUserStore) UpdateUserProfile(ctx context.Context, arg db.UpdateUserProfileParams) (db.User, error) {
	if s.updateProfile == nil {
		return db.User{}, errors.New("stubUserStore: updateProfile not scripted")
	}
	return s.updateProfile(ctx, arg)
}
func (s *stubUserStore) ListVideosByUser(ctx context.Context, arg db.ListVideosByUserParams) ([]db.Video, error) {
	if s.listVideosByUser == nil {
		return nil, errors.New("stubUserStore: listVideosByUser not scripted")
	}
	return s.listVideosByUser(ctx, arg)
}
func (s *stubUserStore) FollowUser(ctx context.Context, arg db.FollowUserParams) error {
	if s.followUser == nil {
		return errors.New("stubUserStore: followUser not scripted")
	}
	return s.followUser(ctx, arg)
}
func (s *stubUserStore) GetFollow(ctx context.Context, arg db.GetFollowParams) (db.Follow, error) {
	if s.getFollow == nil {
		return db.Follow{}, errors.New("stubUserStore: getFollow not scripted")
	}
	return s.getFollow(ctx, arg)
}
func (s *stubUserStore) DeleteFollow(ctx context.Context, arg db.DeleteFollowParams) error {
	if s.deleteFollow == nil {
		return errors.New("stubUserStore: deleteFollow not scripted")
	}
	return s.deleteFollow(ctx, arg)
}
func (s *stubUserStore) ListFollowers(ctx context.Context, arg db.ListFollowersParams) ([]db.ListFollowersRow, error) {
	if s.listFollowers == nil {
		return nil, errors.New("stubUserStore: listFollowers not scripted")
	}
	return s.listFollowers(ctx, arg)
}
func (s *stubUserStore) ListFollowing(ctx context.Context, arg db.ListFollowingParams) ([]db.ListFollowingRow, error) {
	if s.listFollowing == nil {
		return nil, errors.New("stubUserStore: listFollowing not scripted")
	}
	return s.listFollowing(ctx, arg)
}
func (s *stubUserStore) InsertNotification(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
	if s.insertNotification == nil {
		return db.Notification{}, errors.New("stubUserStore: insertNotification not scripted")
	}
	s.notifCalls = append(s.notifCalls, arg)
	return s.insertNotification(ctx, arg)
}

// stubNotificationStore scripts the inbox.
type stubNotificationStore struct {
	list  func(ctx context.Context, arg db.ListNotificationsParams) ([]db.Notification, error)
	mark  func(ctx context.Context, userID uuid.UUID) (int64, error)
}

func (s *stubNotificationStore) ListNotifications(ctx context.Context, arg db.ListNotificationsParams) ([]db.Notification, error) {
	if s.list == nil {
		return nil, errors.New("stubNotificationStore: list not scripted")
	}
	return s.list(ctx, arg)
}
func (s *stubNotificationStore) MarkAllNotificationsRead(ctx context.Context, userID uuid.UUID) (int64, error) {
	if s.mark == nil {
		return 0, errors.New("stubNotificationStore: mark not scripted")
	}
	return s.mark(ctx, userID)
}

// stubFeedStore scripts the feed query.
type stubFeedStore struct {
	listFeed func(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error)
}

func (s *stubFeedStore) ListFeedVideos(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error) {
	if s.listFeed == nil {
		return nil, errors.New("stubFeedStore: listFeed not scripted")
	}
	return s.listFeed(ctx, arg)
}

// stubWebhookStore scripts the webhook store.
type stubWebhookStore struct {
	getByZitadelID func(ctx context.Context, zitadelID string) (db.User, error)
	deactivate     func(ctx context.Context, id uuid.UUID) (db.User, error)
}

func (s *stubWebhookStore) GetUserByZitadelID(ctx context.Context, zitadelID string) (db.User, error) {
	if s.getByZitadelID == nil {
		return db.User{}, errors.New("stubWebhookStore: getByZitadelID not scripted")
	}
	return s.getByZitadelID(ctx, zitadelID)
}
func (s *stubWebhookStore) DeactivateUser(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.deactivate == nil {
		return db.User{}, errors.New("stubWebhookStore: deactivate not scripted")
	}
	return s.deactivate(ctx, id)
}

// stubUserStore satisfies BOTH userStore and videoStore so
// the video-detail tests can reuse it: the video-side write
// methods fail loudly if a read-only test hits them.
func harnessAuthEcho(user db.User, method, path string, h echo.HandlerFunc) *echo.Echo {
	e := echo.New()
	e.Validator = testValidator{v: validator.New()}
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries: testUserStore{user: user},
	}))
	e.Add(method, path, h)
	return e
}

// ---------------------------------------------------------------------------
// User profile endpoints.
// ---------------------------------------------------------------------------

func TestGetUserProfile_HappyPath(t *testing.T) {
	store := &stubUserStore{
		getProfileWithStats: func(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error) {
			return db.GetUserProfileWithStatsRow{
				ID: arg.ID, Username: "target", IsActive: true,
				IsFollowing: true, FollowerCount: 3, FollowingCount: 1,
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/users/:id", uh.GetUserProfile)

	rec := doJSON(e, http.MethodGet, "/api/users/"+testOwnerID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Username       string `json:"username"`
		IsFollowing    bool   `json:"is_following"`
		FollowerCount  int    `json:"follower_count"`
		FollowingCount int    `json:"following_count"`
	}
	decodeData(t, rec, &got)
	if got.Username != "target" || !got.IsFollowing || got.FollowerCount != 3 || got.FollowingCount != 1 {
		t.Errorf("profile = %+v", got)
	}
}

func TestGetUserProfile_InactiveTarget404(t *testing.T) {
	store := &stubUserStore{
		getProfileWithStats: func(ctx context.Context, arg db.GetUserProfileWithStatsParams) (db.GetUserProfileWithStatsRow, error) {
			return db.GetUserProfileWithStatsRow{ID: arg.ID, IsActive: false}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/users/:id", uh.GetUserProfile)

	rec := doJSON(e, http.MethodGet, "/api/users/"+testOwnerID.String(), "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestUpdateMe_PartialFieldsApplied(t *testing.T) {
	store := &stubUserStore{
		updateProfile: func(ctx context.Context, arg db.UpdateUserProfileParams) (db.User, error) {
			if !arg.DisplayName.Valid || arg.DisplayName.String != "New Name" {
				t.Errorf("display_name = %+v, want New Name", arg.DisplayName)
			}
			if arg.Bio.Valid {
				t.Errorf("bio must stay unchanged when omitted, got %+v", arg.Bio)
			}
			return db.User{ID: arg.ID, Username: "viewer", IsActive: true, DisplayName: arg.DisplayName}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPut, "/api/users/me", uh.UpdateMe)

	rec := doJSON(e, http.MethodPut, "/api/users/me", `{"display_name":"New Name"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		DisplayName *string `json:"display_name"`
	}
	decodeData(t, rec, &got)
	if got.DisplayName == nil || *got.DisplayName != "New Name" {
		t.Errorf("display_name = %v", got.DisplayName)
	}
}

// ---------------------------------------------------------------------------
// Follow endpoints.
// ---------------------------------------------------------------------------

func TestFollowUser_HappyPathBestEffortNotification(t *testing.T) {
	store := &stubUserStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true, IsPrivate: false}, nil
		},
		followUser: func(ctx context.Context, arg db.FollowUserParams) error { return nil },
		getFollow: func(ctx context.Context, arg db.GetFollowParams) (db.Follow, error) {
			return db.Follow{FollowerID: arg.FollowerID, FolloweeID: arg.FolloweeID,
				CreatedAt: time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)}, nil
		},
		insertNotification: func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/:id/follow", uh.FollowUser)

	rec := doJSON(e, http.MethodPost, "/api/users/"+testOwnerID.String()+"/follow", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(store.notifCalls) != 1 {
		t.Fatalf("follow must insert 1 best-effort notification, got %d", len(store.notifCalls))
	}
	n := store.notifCalls[0]
	if n.UserID != testOwnerID || n.ActorID != testViewerID || n.Type != "follow" {
		t.Errorf("notification = %+v", n)
	}
}

func TestFollowUser_SelfFollow400(t *testing.T) {
	store := &stubUserStore{}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/:id/follow", uh.FollowUser)

	rec := doJSON(e, http.MethodPost, "/api/users/"+testViewerID.String()+"/follow", "")
	// Explicit denial with the dedicated code, NOT 404.
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeSelfFollowNotAllowed)
}

func TestFollowUser_NotificationFailureStillSucceeds(t *testing.T) {
	// Out-of-tx notification (fase-6 decision): failure is
	// logged warn, the follow still succeeds.
	store := &stubUserStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true}, nil
		},
		followUser: func(ctx context.Context, arg db.FollowUserParams) error { return nil },
		getFollow: func(ctx context.Context, arg db.GetFollowParams) (db.Follow, error) {
			return db.Follow{FollowerID: arg.FollowerID, FolloweeID: arg.FolloweeID}, nil
		},
		insertNotification: func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, errors.New("notification insert failed")
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/:id/follow", uh.FollowUser)

	rec := doJSON(e, http.MethodPost, "/api/users/"+testOwnerID.String()+"/follow", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (best-effort notification)", rec.Code)
	}
}

func TestListFollowers_PrivateTargetNonFollower404(t *testing.T) {
	store := &stubUserStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true, IsPrivate: true}, nil
		},
		isFollowing: func(ctx context.Context, arg db.IsFollowingParams) (bool, error) { return false, nil },
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/users/:id/followers", uh.ListFollowers)

	rec := doJSON(e, http.MethodGet, "/api/users/"+testOwnerID.String()+"/followers", "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestListFollowers_PrivateTargetFollowerAllowed(t *testing.T) {
	store := &stubUserStore{
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true, IsPrivate: true}, nil
		},
		isFollowing: func(ctx context.Context, arg db.IsFollowingParams) (bool, error) { return true, nil },
		listFollowers: func(ctx context.Context, arg db.ListFollowersParams) ([]db.ListFollowersRow, error) {
			return []db.ListFollowersRow{{ID: testViewerID, Username: "viewer", CreatedAt: time.Now()}}, nil
		},
	}
	uh := newUserHandlerForTest(store)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/users/:id/followers", uh.ListFollowers)

	rec := doJSON(e, http.MethodGet, "/api/users/"+testOwnerID.String()+"/followers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Video detail / status / playlist visibility (incl. the TRAP).
// ---------------------------------------------------------------------------

func TestGetVideoStatus_OwnerSeesNonReady(t *testing.T) {
	store := &stubUserStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "PROCESSING", RetryCount: 1}, nil
		},
	}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, &stubEnqueuer{}, testConfirmCfg)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/videos/:id/status", vh.GetVideoStatus)

	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got videoStatusResponse
	decodeData(t, rec, &got)
	if got.Status != "PROCESSING" || got.RetryCount != 1 {
		t.Errorf("statusResponse = %+v", got)
	}
}

func TestGetVideoStatus_NonOwner404(t *testing.T) {
	store := &stubUserStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testOwnerID, Status: "READY"}, nil
		},
	}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, &stubEnqueuer{}, testConfirmCfg)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/videos/:id/status", vh.GetVideoStatus)

	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/status", "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestGetVideoDetail_OwnerBypassVsNonOwnerReadyOnly(t *testing.T) {
	// Owner sees their PROCESSING video; a non-owner gets
	// 404 for the same row (READY-only rule).
	detailRow := func() db.GetVideoDetailRow {
		return db.GetVideoDetailRow{
			ID: testVideoID, UserID: testOwnerID, Status: "PROCESSING",
			UserID2: testOwnerID, UserUsername: "owner", UserIsPrivate: false,
			CreatedAt: time.Now(),
		}
	}
	store := &stubUserStore{
		getVideoDetail: func(ctx context.Context, arg db.GetVideoDetailParams) (db.GetVideoDetailRow, error) {
			r := detailRow()
			r.LikedByMe = false
			return r, nil
		},
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return activePublicOwner(), nil
		},
	}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, &stubEnqueuer{}, testConfirmCfg)

	// Owner path: viewer == owner.
	ownerEcho := harnessAuthEcho(db.User{ID: testOwnerID, IsActive: true, Username: "owner"},
		http.MethodGet, "/api/videos/:id", vh.GetVideoDetail)
	rec := doJSON(ownerEcho, http.MethodGet, "/api/videos/"+testVideoID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200 for PROCESSING own video; body=%s", rec.Code, rec.Body.String())
	}

	// Non-owner path: PROCESSING -> 404.
	viewerEcho := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/videos/:id", vh.GetVideoDetail)
	rec = doJSON(viewerEcho, http.MethodGet, "/api/videos/"+testVideoID.String(), "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

// THE TRAP PIN: the playlist mode deliberately differs from
// the social mode on two axes. (1) A non-READY video is 404
// for EVERYONE, owner included — no owner bypass on the
// READY gate. (2) On the JWT path there is no owner bypass
// on the private-following sub-step either: a private owner
// viewing via JWT fails IsFollowing(self, self) and gets 404;
// the owner is expected to use the ?token= path. Both
// behaviours are pre-existing (fase 6) and are preserved
// verbatim by the shared helper migration.
func TestVisibility_PlaylistMode_PrivateOwnerJWTNoBypass(t *testing.T) {
	store := &stubUserStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{
				ID: id, UserID: testOwnerID, Status: "READY",
				HlsPrefix: sql.NullString{String: "hls/u/v", Valid: true},
			}, nil
		},
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: id, IsActive: true, IsPrivate: true}, nil // owner is PRIVATE
		},
		// IsFollowing(self, self) can only return false — there
		// is no self-edge row. The handler must NOT special-case
		// the owner here.
		isFollowing: func(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
			if arg.FollowerID != testOwnerID || arg.FolloweeID != testOwnerID {
				t.Errorf("IsFollowing args = %+v, want (owner, owner)", arg)
			}
			return false, nil
		},
	}
	r2 := &stubR2{
		getObject: func(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
			return nil, shared.Wrap(shared.ErrNotFound, "no playlist") // not reached
		},
	}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, r2, &stubEnqueuer{}, &shared.APIConfig{
		MediaTokenSecret: "secret", APIBaseURL: "https://api.example.com",
	})

	// Viewer == owner, via JWT (Authorization header present,
	// so tokenPath = false).
	e := harnessAuthEcho(db.User{ID: testOwnerID, IsActive: true, Username: "owner"},
		http.MethodGet, "/api/videos/:id/playlist.m3u8", vh.GetPlaylist)
	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/playlist.m3u8", "")
	// THE PIN: 404, not 200. A naive "owner bypasses everything"
	// helper would return the playlist here and silently break
	// anti-enumeration.
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestGetPlaylist_NonReady404EvenForOwner(t *testing.T) {
	// Trap axis (1): the READY gate has no owner bypass.
	store := &stubUserStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testOwnerID, Status: "PROCESSING"}, nil
		},
	}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, &stubEnqueuer{}, &shared.APIConfig{
		MediaTokenSecret: "secret", APIBaseURL: "https://api.example.com",
	})
	e := harnessAuthEcho(db.User{ID: testOwnerID, IsActive: true, Username: "owner"},
		http.MethodGet, "/api/videos/:id/playlist.m3u8", vh.GetPlaylist)
	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/playlist.m3u8", "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

func TestGetPlaylist_MediaTokenPathSkipsVisibility(t *testing.T) {
	// Anonymous + valid ?token= for a private owner's video:
	// the token IS the authorisation; the follow-graph check
	// must not run (no IsFollowing call).
	isFollowingCalled := false
	store := &stubUserStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{
				ID: id, UserID: testOwnerID, Status: "READY",
				HlsPrefix: sql.NullString{String: "hls/u/v", Valid: true},
			}, nil
		},
		isFollowing: func(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
			isFollowingCalled = true
			return false, nil
		},
	}
	r2 := &stubR2{
		getObject: func(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
			if key != "hls/u/v/master.m3u8" {
				t.Errorf("master key = %s, want hls/u/v/master.m3u8", key)
			}
			return []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000\n480p/index.m3u8\n"), nil
		},
	}
	cfg := &shared.APIConfig{MediaTokenSecret: "secret", APIBaseURL: "https://api.example.com"}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, r2, &stubEnqueuer{}, cfg)

	// Build a REAL media token (same helper production uses).
	token, _, err := shared.NewMediaToken(testVideoID.String(), cfg.MediaTokenSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewMediaToken: %v", err)
	}

	// No Authenticate middleware here — the route is mounted
	// with AuthenticateOptional in production; anonymous request.
	e := echo.New()
	e.GET("/api/videos/:id/playlist.m3u8", vh.GetPlaylist)
	req := httptest.NewRequest(http.MethodGet, "/api/videos/"+testVideoID.String()+"/playlist.m3u8?token="+token, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid token path); body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.Contains(ct, "mpegurl") {
		t.Errorf("content-type = %s", ct)
	}
	if isFollowingCalled {
		t.Error("token path must NOT run the follow-graph visibility check")
	}
}

func TestGetPlaylist_AnonymousMissingToken401(t *testing.T) {
	store := &stubUserStore{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, &stubEnqueuer{}, &shared.APIConfig{
		MediaTokenSecret: "secret", APIBaseURL: "https://api.example.com",
	})
	e := echo.New()
	e.GET("/api/videos/:id/playlist.m3u8", vh.GetPlaylist)
	req := httptest.NewRequest(http.MethodGet, "/api/videos/"+testVideoID.String()+"/playlist.m3u8", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assertErrCode(t, rec, http.StatusUnauthorized, shared.CodeUnauthorized)
}

// ---------------------------------------------------------------------------
// Notification inbox.
// ---------------------------------------------------------------------------

func TestNotificationListAndMarkAllRead(t *testing.T) {
	nid := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	store := &stubNotificationStore{
		list: func(ctx context.Context, arg db.ListNotificationsParams) ([]db.Notification, error) {
			if arg.UserID != testViewerID {
				t.Errorf("list target = %s", arg.UserID)
			}
			return []db.Notification{{ID: nid, UserID: testViewerID, Type: "like", IsRead: false, CreatedAt: now}}, nil
		},
		mark: func(ctx context.Context, userID uuid.UUID) (int64, error) {
			return 2, nil
		},
	}
	nh := NewNotificationHandler(nil)
	nh.Queries = store
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/notifications", nh.List)

	rec := doJSON(e, http.MethodGet, "/api/notifications", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []NotificationObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 || env.Data[0].Type != "like" || env.Data[0].IsRead {
		t.Errorf("notifications = %+v", env.Data)
	}

	// Mark-all-read.
	e2 := harnessAuthEcho(viewerUser(), http.MethodPut, "/api/notifications/read-all", nh.MarkAllRead)
	rec2 := doJSON(e2, http.MethodPut, "/api/notifications/read-all", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("mark status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	var got struct {
		UpdatedCount int `json:"updated_count"`
	}
	decodeData(t, rec2, &got)
	if got.UpdatedCount != 2 {
		t.Errorf("updated_count = %d, want 2", got.UpdatedCount)
	}
}

// ---------------------------------------------------------------------------
// Home feed.
// ---------------------------------------------------------------------------

func TestHomeFeed_ExcludesSelfPresignedURLs(t *testing.T) {
	thumbKey := sql.NullString{String: "thumbs/1.jpg", Valid: true}
	store := &stubFeedStore{
		listFeed: func(ctx context.Context, arg db.ListFeedVideosParams) ([]db.ListFeedVideosRow, error) {
			if arg.UserID != testViewerID {
				t.Errorf("feed viewer = %s", arg.UserID)
			}
			return []db.ListFeedVideosRow{{
				ID: testVideoID, UserID: testOwnerID, Status: "READY",
				ThumbnailKey: thumbKey, CreatedAt: time.Now(),
				UserID2: testOwnerID, UserUsername: "owner", UserIsPrivate: false,
				LikedByMe: true,
			}}, nil
		},
	}
	r2 := &stubR2{
		presignGet: func(ctx context.Context, key string, expiry time.Duration) (string, error) {
			if key != "thumbs/1.jpg" {
				t.Errorf("presign key = %s", key)
			}
			return "https://r2.example.com/signed", nil
		},
	}
	cfg := &shared.APIConfig{MediaTokenSecret: "secret", APIBaseURL: "https://api.example.com"}
	fh := newFeedHandlerForTest(store, r2, cfg)
	e := harnessAuthEcho(viewerUser(), http.MethodGet, "/api/feed/home", fh.HomeFeed)

	rec := doJSON(e, http.MethodGet, "/api/feed/home", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data []VideoObject `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 1 {
		t.Fatalf("feed items = %d, want 1", len(env.Data))
	}
	item := env.Data[0]
	if item.IsOwner {
		t.Error("feed must never mark is_owner (self excluded upstream)")
	}
	if !item.LikedByMe {
		t.Error("liked_by_me must flow from the SQL EXISTS join")
	}
	if item.ThumbnailURL == nil || *item.ThumbnailURL != "https://r2.example.com/signed" {
		t.Errorf("thumbnail_url = %v", item.ThumbnailURL)
	}
	if item.HLSPlaylistURL == nil || !strings.Contains(*item.HLSPlaylistURL, "token=") {
		t.Errorf("hls_playlist_url = %v (must embed a media token)", item.HLSPlaylistURL)
	}
}

// ---------------------------------------------------------------------------
// Webhook (real actions.ValidateRequestPayload signature).
// ---------------------------------------------------------------------------

const testSigningKey = "test-signing-key"

func signWebhook(t *testing.T, payload []byte) string {
	t.Helper()
	return actions.ComputeSignatureHeader(time.Now(), payload, testSigningKey)
}

func postWebhook(e *echo.Echo, body []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/zitadel", strings.NewReader(string(body)))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if sig != "" {
		req.Header.Set(actions.SigningHeader, sig)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestWebhook_InvalidSignature401(t *testing.T) {
	store := &stubWebhookStore{}
	wh := newWebhookHandlerForTest(store, testSigningKey)
	e := echo.New()
	e.POST("/api/webhooks/zitadel", wh.Handle)

	payload := []byte(`{"event_type":"user.deactivated","aggregateID":"sub-1"}`)
	rec := postWebhook(e, payload, "t=1,v1=deadbeef")
	assertErrCode(t, rec, http.StatusUnauthorized, shared.CodeWebhookInvalidSignature)
}

func TestWebhook_ValidSignatureDeactivates(t *testing.T) {
	deactivated := uuid.Nil
	store := &stubWebhookStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			if zitadelID != "sub-1" {
				t.Errorf("lookup sub = %s", zitadelID)
			}
			return db.User{ID: testViewerID, ZitadelID: zitadelID, IsActive: true}, nil
		},
		deactivate: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			deactivated = id
			return db.User{ID: id, IsActive: false}, nil
		},
	}
	wh := newWebhookHandlerForTest(store, testSigningKey)
	e := echo.New()
	e.POST("/api/webhooks/zitadel", wh.Handle)

	payload := []byte(`{"event_type":"user.deactivated","aggregateID":"sub-1","userID":"admin-1"}`)
	rec := postWebhook(e, payload, signWebhook(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if deactivated != testViewerID {
		t.Errorf("deactivated = %s, want %s (aggregateID = affected user, NOT the admin actor)", deactivated, testViewerID)
	}
}

func TestWebhook_MissingLocalUserAcked(t *testing.T) {
	store := &stubWebhookStore{
		getByZitadelID: func(ctx context.Context, zitadelID string) (db.User, error) {
			return db.User{}, sql.ErrNoRows
		},
	}
	wh := newWebhookHandlerForTest(store, testSigningKey)
	e := echo.New()
	e.POST("/api/webhooks/zitadel", wh.Handle)

	payload := []byte(`{"event_type":"user.deactivated","aggregateID":"never-logged-in"}`)
	rec := postWebhook(e, payload, signWebhook(t, payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (missing local user acked so Zitadel stops retrying)", rec.Code)
	}
}

func TestWebhook_UnsupportedEvent400(t *testing.T) {
	store := &stubWebhookStore{}
	wh := newWebhookHandlerForTest(store, testSigningKey)
	e := echo.New()
	e.POST("/api/webhooks/zitadel", wh.Handle)

	payload := []byte(fmt.Sprintf(`{"event_type":"user.locked","aggregateID":"sub-1"}`))
	rec := postWebhook(e, payload, signWebhook(t, payload))
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeWebhookEventUnsupported)
}

// Compile-time checks.
var (
	_ userStore          = (*stubUserStore)(nil)
	_ notificationStore  = (*stubNotificationStore)(nil)
	_ feedStore          = (*stubFeedStore)(nil)
	_ webhookStore       = (*stubWebhookStore)(nil)
	_ visibilityStore    = (*stubUserStore)(nil)
)
