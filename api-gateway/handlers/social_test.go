// Package handlers - social_test.go drives the SocialHandler
// endpoints through real Echo requests with stubbed store /
// tx-runner dependencies. This is the unit-test layer the
// testability refactor unlocked: before it, the handler held
// *db.Queries + *sql.DB concretes and no test could run
// without a live Postgres.
//
// Patterns (mirroring middleware/auth_test.go):
//   - stub stores implement the consumer-side interfaces with
//     function fields so each test scripts its own rows /
//     errors / call recording.
//   - the real middleware.Authenticate runs in front of the
//     handler with a stub TokenVerifier + UserStore, so the
//     *db.User resolution path is the production one.
//   - no test touches Postgres/Redis/R2/Zitadel.
//
// The tx begin/bind/commit/rollback mechanics live in
// sqlTxRunner (tx.go) and are exercised live by
// scripts/integration_test.sh; here Run is stubbed so the
// business logic inside the tx body is what gets verified.
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// ---------------------------------------------------------------------------
// Stubs: socialStore (reads) and socialTxStore (tx body).
// ---------------------------------------------------------------------------

// stubSocialStore scripts every read the handler performs.
// Unset function fields fail the test loudly (nil-fn default
// returns a distinct error) instead of silently succeeding.
type stubSocialStore struct {
	getVideoByID       func(ctx context.Context, id uuid.UUID) (db.Video, error)
	getUserByID        func(ctx context.Context, id uuid.UUID) (db.User, error)
	isFollowing        func(ctx context.Context, arg db.IsFollowingParams) (bool, error)
	getCommentByID     func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error)
	listCommentsByVideo func(ctx context.Context, arg db.ListCommentsByVideoParams) ([]db.ListCommentsByVideoRow, error)
	incrementViews     func(ctx context.Context, id uuid.UUID) error
}

func (s *stubSocialStore) GetVideoByID(ctx context.Context, id uuid.UUID) (db.Video, error) {
	if s.getVideoByID == nil {
		return db.Video{}, errors.New("stubSocialStore: getVideoByID not scripted")
	}
	return s.getVideoByID(ctx, id)
}

func (s *stubSocialStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.getUserByID == nil {
		return db.User{}, errors.New("stubSocialStore: getUserByID not scripted")
	}
	return s.getUserByID(ctx, id)
}

func (s *stubSocialStore) IsFollowing(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
	if s.isFollowing == nil {
		return false, errors.New("stubSocialStore: isFollowing not scripted")
	}
	return s.isFollowing(ctx, arg)
}

func (s *stubSocialStore) GetCommentByID(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
	if s.getCommentByID == nil {
		return db.GetCommentByIDRow{}, errors.New("stubSocialStore: getCommentByID not scripted")
	}
	return s.getCommentByID(ctx, id)
}

func (s *stubSocialStore) ListCommentsByVideo(ctx context.Context, arg db.ListCommentsByVideoParams) ([]db.ListCommentsByVideoRow, error) {
	if s.listCommentsByVideo == nil {
		return nil, errors.New("stubSocialStore: listCommentsByVideo not scripted")
	}
	return s.listCommentsByVideo(ctx, arg)
}

func (s *stubSocialStore) IncrementViews(ctx context.Context, id uuid.UUID) error {
	if s.incrementViews == nil {
		return errors.New("stubSocialStore: incrementViews not scripted")
	}
	return s.incrementViews(ctx, id)
}

// stubSocialTx records every tx-body call and can script
// failures per method.
type stubSocialTx struct {
	insertLike          func(ctx context.Context, arg db.InsertLikeParams) (db.Like, error)
	deleteLike          func(ctx context.Context, arg db.DeleteLikeParams) (db.Like, error)
	incrementLikesCount func(ctx context.Context, id uuid.UUID) error
	decrementLikesCount func(ctx context.Context, id uuid.UUID) error
	incrementCommentsCount func(ctx context.Context, id uuid.UUID) error
	insertComment       func(ctx context.Context, arg db.InsertCommentParams) (db.Comment, error)
	deleteCommentByID   func(ctx context.Context, id uuid.UUID) error
	countCommentSubtree func(ctx context.Context, id uuid.UUID) (int32, error)
	decrementCommentsCountBy func(ctx context.Context, arg db.DecrementCommentsCountByParams) error
	insertNotification func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error)

	// records for assertions
	notifications []db.InsertNotificationParams
	comments      []db.InsertCommentParams
}

func (s *stubSocialTx) InsertLike(ctx context.Context, arg db.InsertLikeParams) (db.Like, error) {
	if s.insertLike == nil {
		return db.Like{}, errors.New("stubSocialTx: insertLike not scripted")
	}
	return s.insertLike(ctx, arg)
}

func (s *stubSocialTx) DeleteLike(ctx context.Context, arg db.DeleteLikeParams) (db.Like, error) {
	if s.deleteLike == nil {
		return db.Like{}, errors.New("stubSocialTx: deleteLike not scripted")
	}
	return s.deleteLike(ctx, arg)
}

func (s *stubSocialTx) IncrementLikesCount(ctx context.Context, id uuid.UUID) error {
	if s.incrementLikesCount == nil {
		return errors.New("stubSocialTx: incrementLikesCount not scripted")
	}
	return s.incrementLikesCount(ctx, id)
}

func (s *stubSocialTx) DecrementLikesCount(ctx context.Context, id uuid.UUID) error {
	if s.decrementLikesCount == nil {
		return errors.New("stubSocialTx: decrementLikesCount not scripted")
	}
	return s.decrementLikesCount(ctx, id)
}

func (s *stubSocialTx) IncrementCommentsCount(ctx context.Context, id uuid.UUID) error {
	if s.incrementCommentsCount == nil {
		return errors.New("stubSocialTx: incrementCommentsCount not scripted")
	}
	return s.incrementCommentsCount(ctx, id)
}

func (s *stubSocialTx) InsertComment(ctx context.Context, arg db.InsertCommentParams) (db.Comment, error) {
	if s.insertComment == nil {
		return db.Comment{}, errors.New("stubSocialTx: insertComment not scripted")
	}
	s.comments = append(s.comments, arg)
	return s.insertComment(ctx, arg)
}

func (s *stubSocialTx) DeleteCommentByID(ctx context.Context, id uuid.UUID) error {
	if s.deleteCommentByID == nil {
		return errors.New("stubSocialTx: deleteCommentByID not scripted")
	}
	return s.deleteCommentByID(ctx, id)
}

func (s *stubSocialTx) CountCommentSubtree(ctx context.Context, id uuid.UUID) (int32, error) {
	if s.countCommentSubtree == nil {
		return 0, errors.New("stubSocialTx: countCommentSubtree not scripted")
	}
	return s.countCommentSubtree(ctx, id)
}

func (s *stubSocialTx) DecrementCommentsCountBy(ctx context.Context, arg db.DecrementCommentsCountByParams) error {
	if s.decrementCommentsCountBy == nil {
		return errors.New("stubSocialTx: decrementCommentsCountBy not scripted")
	}
	return s.decrementCommentsCountBy(ctx, arg)
}

func (s *stubSocialTx) InsertNotification(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
	if s.insertNotification == nil {
		return db.Notification{}, errors.New("stubSocialTx: insertNotification not scripted")
	}
	s.notifications = append(s.notifications, arg)
	return s.insertNotification(ctx, arg)
}

// stubTxRunner mirrors sqlTxRunner semantics: fn error =>
// no commit (rollback path), nil => committed. beginErr
// simulates a pool failure before fn runs.
type stubTxRunner struct {
	tx       *stubSocialTx
	beginErr error

	runs      int
	committed bool
	aborted   bool
}

func (r *stubTxRunner) Run(ctx context.Context, fn func(q socialTxStore) error) error {
	r.runs++
	if r.beginErr != nil {
		return r.beginErr
	}
	if err := fn(r.tx); err != nil {
		r.aborted = true
		return err
	}
	r.committed = true
	return nil
}

// ---------------------------------------------------------------------------
// Auth stubs so the REAL middleware.Authenticate resolves the
// user (same production path, no Zitadel).
// ---------------------------------------------------------------------------

type testVerifier struct{ sub string }

func (v testVerifier) CheckToken(ctx context.Context, rawToken string) (string, error) {
	return v.sub, nil
}

type testUserStore struct{ user db.User }

func (s testUserStore) GetUserByZitadelID(ctx context.Context, zitadelID string) (db.User, error) {
	return s.user, nil
}

func (s testUserStore) CreateUser(ctx context.Context, arg db.CreateUserParams) (db.User, error) {
	return db.User{}, sql.ErrNoRows
}

// testValidator mirrors the production RequestValidator closely
// enough for 400 assertions: a validator/v10 failure is
// translated to the canonical VALIDATION_ERROR APIError.
type testValidator struct{ v *validator.Validate }

func (tv testValidator) Validate(i interface{}) error {
	if err := tv.v.Struct(i); err != nil {
		return shared.NewAPIError(shared.CodeValidationError, "request body failed validation")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Fixture IDs + harness.
// ---------------------------------------------------------------------------

var (
	testOwnerID  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testViewerID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testVideoID  = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	testCommentID = uuid.MustParse("44444444-4444-4444-4444-444444444444")
)

func readyVideo() db.Video {
	return db.Video{
		ID:          testVideoID,
		UserID:      testOwnerID,
		Status:      "READY",
		LikesCount:  5,
		ViewsCount:  10,
		CommentsCount: 2,
	}
}

func activePublicOwner() db.User {
	return db.User{ID: testOwnerID, IsActive: true, IsPrivate: false, Username: "owner"}
}

func viewerUser() db.User {
	return db.User{ID: testViewerID, IsActive: true, Username: "viewer"}
}

// newSocialTestEcho wires the real Authenticate middleware
// (stub verifier/store) in front of the given handler so the
// request path matches production, then returns the app plus
// the runner the handler was built with.
func newSocialTestEcho(store socialStore, runner txRunner[socialTxStore], user db.User, method, path string, h echo.HandlerFunc) *echo.Echo {
	e := echo.New()
	e.Validator = testValidator{v: validator.New()}
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: user},
	}))
	e.Add(method, path, h)
	return e
}

func doJSON(e *echo.Echo, method, target, body string) *httptest.ResponseRecorder {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set(echo.HeaderAuthorization, "Bearer test-token")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, rec.Body.String())
	}
	if err := json.Unmarshal(env.Data, into); err != nil {
		t.Fatalf("decode data: %v; body=%s", err, rec.Body.String())
	}
}

func assertErrCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode shared.ErrorCode) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
	}
	// Decode only the FIRST envelope: some follow-list paths
	// write the 404 envelope and then ALSO return it to Echo,
	// whose HTTPErrorHandler appends a second envelope (same
	// status, same code — pre-existing behaviour, harmless on
	// the wire for status-only clients, kept out of refactor
	// scope). json.Decoder stops at the first value.
	dec := json.NewDecoder(rec.Body)
	var env struct {
		Error struct {
			Code shared.ErrorCode `json:"code"`
		} `json:"error"`
	}
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("code = %s, want %s; body=%s", env.Error.Code, wantCode, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// LikeVideo
// ---------------------------------------------------------------------------

func TestLikeVideo_HappyPathIncrementsAndNotifiesOwner(t *testing.T) {
	calls := 0
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			calls++
			v := readyVideo()
			if calls == 2 {
				v.LikesCount = 6 // post-commit refresh sees the increment
			}
			return v, nil
		},
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return activePublicOwner(), nil
		},
	}
	tx := &stubSocialTx{
		insertLike: func(ctx context.Context, arg db.InsertLikeParams) (db.Like, error) {
			if arg.UserID != testViewerID || arg.VideoID != testVideoID {
				t.Errorf("InsertLike args = %+v", arg)
			}
			return db.Like{UserID: arg.UserID, VideoID: arg.VideoID}, nil
		},
		incrementLikesCount: func(ctx context.Context, id uuid.UUID) error { return nil },
		insertNotification:  func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/like", sh.LikeVideo)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/like", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got LikeObject
	decodeData(t, rec, &got)
	if !got.Liked || got.LikesCount != 6 || got.VideoID != testVideoID {
		t.Errorf("LikeObject = %+v, want {Liked:true, LikesCount:6}", got)
	}
	if !runner.committed || runner.aborted {
		t.Errorf("runner committed=%v aborted=%v, want committed only", runner.committed, runner.aborted)
	}
	if len(tx.notifications) != 1 {
		t.Fatalf("notification count = %d, want 1", len(tx.notifications))
	}
	n := tx.notifications[0]
	if n.UserID != testOwnerID || n.ActorID != testViewerID || n.Type != "like" {
		t.Errorf("notification = %+v, want owner=%s actor=%s type=like", n, testOwnerID, testViewerID)
	}
}

func TestLikeVideo_SelfLikeSkipsNotification(t *testing.T) {
	own := readyVideo()
	own.UserID = testViewerID // viewer owns the video
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return own, nil },
	}
	tx := &stubSocialTx{
		insertLike:          func(ctx context.Context, arg db.InsertLikeParams) (db.Like, error) { return db.Like{}, nil },
		incrementLikesCount: func(ctx context.Context, id uuid.UUID) error { return nil },
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/like", sh.LikeVideo)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/like", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(tx.notifications) != 0 {
		t.Errorf("self-like must not notify, got %d notifications", len(tx.notifications))
	}
}

func TestLikeVideo_IdempotentReLikeDoesNotIncrement(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	increments := 0
	tx := &stubSocialTx{
		insertLike: func(ctx context.Context, arg db.InsertLikeParams) (db.Like, error) {
			return db.Like{}, sql.ErrNoRows // ON CONFLICT DO NOTHING
		},
		incrementLikesCount: func(ctx context.Context, id uuid.UUID) error {
			increments++
			return nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/like", sh.LikeVideo)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/like", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if increments != 0 {
		t.Errorf("idempotent re-like must not bump the counter, got %d increments", increments)
	}
	if len(tx.notifications) != 0 {
		t.Errorf("idempotent re-like must not notify, got %d", len(tx.notifications))
	}
}

func TestLikeVideo_TxBodyFailureAbortsNoCommit(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	tx := &stubSocialTx{
		insertLike: func(ctx context.Context, arg db.InsertLikeParams) (db.Like, error) {
			return db.Like{}, nil
		},
		incrementLikesCount: func(ctx context.Context, id uuid.UUID) error {
			return errors.New("counter check constraint violated")
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/like", sh.LikeVideo)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/like", "")
	assertErrCode(t, rec, http.StatusInternalServerError, shared.CodeInternalError)
	if runner.committed {
		t.Error("tx body failure must not commit")
	}
	if !runner.aborted {
		t.Error("tx body failure must abort (rollback)")
	}
}

func TestLikeVideo_PrivateOwnerNotFollowing404(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID: func(ctx context.Context, id uuid.UUID) (db.User, error) {
			return db.User{ID: testOwnerID, IsActive: true, IsPrivate: true}, nil
		},
		isFollowing: func(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
			if arg.FollowerID != testViewerID || arg.FolloweeID != testOwnerID {
				t.Errorf("IsFollowing args = %+v", arg)
			}
			return false, nil
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/like", sh.LikeVideo)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/like", "")
	// Anti-enumeration: a private video the viewer does not
	// follow collapses to 404, never 403.
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
	if runner.runs != 0 {
		t.Errorf("visibility rejection must short-circuit before the tx, got %d runs", runner.runs)
	}
}

// ---------------------------------------------------------------------------
// UnlikeVideo
// ---------------------------------------------------------------------------

func TestUnlikeVideo_HappyPathDecrements(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	decrements := 0
	tx := &stubSocialTx{
		deleteLike: func(ctx context.Context, arg db.DeleteLikeParams) (db.Like, error) {
			return db.Like{UserID: arg.UserID, VideoID: arg.VideoID}, nil
		},
		decrementLikesCount: func(ctx context.Context, id uuid.UUID) error {
			decrements++
			return nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodDelete, "/api/videos/:id/like", sh.UnlikeVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String()+"/like", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got LikeObject
	decodeData(t, rec, &got)
	if got.Liked || got.LikesCount != 5 {
		t.Errorf("LikeObject = %+v, want {Liked:false, LikesCount:5}", got)
	}
	if decrements != 1 {
		t.Errorf("decrements = %d, want 1", decrements)
	}
	if !runner.committed {
		t.Error("successful unlike must commit")
	}
}

func TestUnlikeVideo_IdempotentNoRowDoesNotDecrement(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	decrements := 0
	tx := &stubSocialTx{
		deleteLike: func(ctx context.Context, arg db.DeleteLikeParams) (db.Like, error) {
			return db.Like{}, sql.ErrNoRows // like did not exist
		},
		decrementLikesCount: func(ctx context.Context, id uuid.UUID) error {
			decrements++
			return nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodDelete, "/api/videos/:id/like", sh.UnlikeVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String()+"/like", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if decrements != 0 {
		t.Errorf("idempotent unlike must not decrement, got %d", decrements)
	}
}

// ---------------------------------------------------------------------------
// TrackView
// ---------------------------------------------------------------------------

func TestTrackView_HappyPathIncrements(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
		incrementViews: func(ctx context.Context, id uuid.UUID) error { return nil },
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}} // TrackView takes no tx
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/view", sh.TrackView)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/view", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got viewObject
	decodeData(t, rec, &got)
	if got.VideoID != testVideoID || got.ViewsCount != 10 {
		t.Errorf("viewObject = %+v, want views_count=10", got)
	}
}

// ---------------------------------------------------------------------------
// CreateComment
// ---------------------------------------------------------------------------

func TestCreateComment_HappyPathNotifiesOwner(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			return db.GetCommentByIDRow{
				ID: id, VideoID: testVideoID, UserID: testViewerID,
				Content: "nice video", Username: "viewer", IsPrivate: false,
			}, nil
		},
	}
	tx := &stubSocialTx{
		insertComment: func(ctx context.Context, arg db.InsertCommentParams) (db.Comment, error) {
			if arg.ParentID.Valid {
				t.Errorf("top-level comment must have NULL parent, got %+v", arg.ParentID)
			}
			return db.Comment{ID: testCommentID, VideoID: arg.VideoID, UserID: arg.UserID, Content: arg.Content}, nil
		},
		incrementCommentsCount: func(ctx context.Context, id uuid.UUID) error { return nil },
		insertNotification: func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/comments", sh.CreateComment)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/comments", `{"content":"nice video"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got CommentObject
	decodeData(t, rec, &got)
	if got.ID != testCommentID || got.Content != "nice video" || got.ParentID != nil {
		t.Errorf("CommentObject = %+v", got)
	}
	if len(tx.notifications) != 1 || tx.notifications[0].UserID != testOwnerID {
		t.Errorf("notifications = %+v, want one to owner", tx.notifications)
	}
	if !runner.committed {
		t.Error("successful comment must commit")
	}
}

func TestCreateComment_NotificationFailureFailsWholeTx(t *testing.T) {
	// In-tx notification atomicity (LLD section 10): if the
	// notification insert fails, the comment + counter must
	// roll back too - the request is a 500, not a partial
	// success.
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	tx := &stubSocialTx{
		insertComment: func(ctx context.Context, arg db.InsertCommentParams) (db.Comment, error) {
			return db.Comment{ID: testCommentID}, nil
		},
		incrementCommentsCount: func(ctx context.Context, id uuid.UUID) error { return nil },
		insertNotification: func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, errors.New("notification insert failed")
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/comments", sh.CreateComment)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/comments", `{"content":"hi"}`)
	assertErrCode(t, rec, http.StatusInternalServerError, shared.CodeInternalError)
	if runner.committed {
		t.Error("notification failure must abort the whole tx (no commit)")
	}
}

func TestCreateComment_WhitespaceOnlyContentRejected(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/videos/:id/comments", sh.CreateComment)

	rec := doJSON(e, http.MethodPost, "/api/videos/"+testVideoID.String()+"/comments", `{"content":"   "}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
	if runner.runs != 0 {
		t.Errorf("invalid body must short-circuit before the tx, got %d runs", runner.runs)
	}
}

// ---------------------------------------------------------------------------
// ListComments
// ---------------------------------------------------------------------------

func TestListComments_CursorPagination(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	c1 := db.ListCommentsByVideoRow{ID: testCommentID, VideoID: testVideoID, UserID: testViewerID, Content: "first", CreatedAt: now}
	c2 := db.ListCommentsByVideoRow{ID: testCommentID, VideoID: testVideoID, UserID: testViewerID, Content: "second", CreatedAt: now.Add(-time.Minute)}
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
		listCommentsByVideo: func(ctx context.Context, arg db.ListCommentsByVideoParams) ([]db.ListCommentsByVideoRow, error) {
			if arg.PageLimit != 2 {
				t.Errorf("PageLimit = %d, want 2", arg.PageLimit)
			}
			return []db.ListCommentsByVideoRow{c1, c2}, nil
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodGet, "/api/videos/:id/comments", sh.ListComments)

	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/comments?limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data       []CommentObject `json:"data"`
		Pagination *struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data) != 2 {
		t.Fatalf("items = %d, want 2", len(env.Data))
	}
	if env.Pagination == nil || env.Pagination.NextCursor == nil || *env.Pagination.NextCursor == "" {
		t.Errorf("full page must carry next_cursor, got %+v", env.Pagination)
	}
}

func TestListComments_LastPageNullCursor(t *testing.T) {
	store := &stubSocialStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
		listCommentsByVideo: func(ctx context.Context, arg db.ListCommentsByVideoParams) ([]db.ListCommentsByVideoRow, error) {
			return []db.ListCommentsByVideoRow{
				{ID: testCommentID, VideoID: testVideoID, UserID: testViewerID, Content: "only"},
			}, nil
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodGet, "/api/videos/:id/comments", sh.ListComments)

	rec := doJSON(e, http.MethodGet, "/api/videos/"+testVideoID.String()+"/comments?limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data       []CommentObject `json:"data"`
		Pagination *struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Pagination == nil || env.Pagination.NextCursor != nil {
		t.Errorf("partial page must have next_cursor=null, got %+v", env.Pagination)
	}
}

// ---------------------------------------------------------------------------
// DeleteComment
// ---------------------------------------------------------------------------

func TestDeleteComment_OwnerDeletesSubtree(t *testing.T) {
	store := &stubSocialStore{
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			return db.GetCommentByIDRow{ID: id, VideoID: testVideoID, UserID: testViewerID, Content: "mine"}, nil
		},
	}
	var decrementedBy int32
	tx := &stubSocialTx{
		countCommentSubtree: func(ctx context.Context, id uuid.UUID) (int32, error) { return 2, nil },
		deleteCommentByID:   func(ctx context.Context, id uuid.UUID) error { return nil },
		decrementCommentsCountBy: func(ctx context.Context, arg db.DecrementCommentsCountByParams) error {
			decrementedBy = arg.CommentsCount
			if arg.ID != testVideoID {
				t.Errorf("decrement target video = %s, want %s", arg.ID, testVideoID)
			}
			return nil
		},
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodDelete, "/api/comments/:id", sh.DeleteComment)

	rec := doJSON(e, http.MethodDelete, "/api/comments/"+testCommentID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if decrementedBy != 2 {
		t.Errorf("comments_count must drop by subtree size 2, got %d", decrementedBy)
	}
	if !runner.committed {
		t.Error("successful delete must commit")
	}
}

func TestDeleteComment_NonOwnerGets404(t *testing.T) {
	store := &stubSocialStore{
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			// Comment owned by someone else.
			return db.GetCommentByIDRow{ID: id, VideoID: testVideoID, UserID: testOwnerID}, nil
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodDelete, "/api/comments/:id", sh.DeleteComment)

	rec := doJSON(e, http.MethodDelete, "/api/comments/"+testCommentID.String(), "")
	// Anti-enumeration: same 404 as a missing comment.
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
	if runner.runs != 0 {
		t.Errorf("non-owner delete must short-circuit before the tx")
	}
}

func TestDeleteComment_MissingComment404(t *testing.T) {
	store := &stubSocialStore{
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			return db.GetCommentByIDRow{}, sql.ErrNoRows
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodDelete, "/api/comments/:id", sh.DeleteComment)

	rec := doJSON(e, http.MethodDelete, "/api/comments/"+testCommentID.String(), "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

// ---------------------------------------------------------------------------
// ReplyComment
// ---------------------------------------------------------------------------

func replyFixture() (*stubSocialStore, *stubSocialTx) {
	store := &stubSocialStore{
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			return db.GetCommentByIDRow{ID: id, VideoID: testVideoID, UserID: testOwnerID, Content: "parent"}, nil
		},
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) { return readyVideo(), nil },
		getUserByID:  func(ctx context.Context, id uuid.UUID) (db.User, error) { return activePublicOwner(), nil },
	}
	tx := &stubSocialTx{
		insertComment: func(ctx context.Context, arg db.InsertCommentParams) (db.Comment, error) {
			return db.Comment{ID: testCommentID, VideoID: arg.VideoID, UserID: arg.UserID, ParentID: arg.ParentID, Content: arg.Content}, nil
		},
		incrementCommentsCount: func(ctx context.Context, id uuid.UUID) error { return nil },
		insertNotification: func(ctx context.Context, arg db.InsertNotificationParams) (db.Notification, error) {
			return db.Notification{}, nil
		},
	}
	return store, tx
}

func TestReplyComment_OwnerIsParentAuthorDedupsToOneRecipient(t *testing.T) {
	// owner == parent author -> dedup collapses the fan-out to
	// a single notification.
	store, tx := replyFixture()
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/comments/:id/reply", sh.ReplyComment)

	rec := doJSON(e, http.MethodPost, "/api/comments/"+testCommentID.String()+"/reply", `{"content":"a reply"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(tx.notifications) != 1 {
		t.Fatalf("notifications = %d, want 1 (owner == parent author dedup)", len(tx.notifications))
	}
	if tx.notifications[0].UserID != testOwnerID {
		t.Errorf("notification recipient = %s, want owner %s", tx.notifications[0].UserID, testOwnerID)
	}
}

func TestReplyComment_DistinctOwnerAndParentAuthorTwoNotifications(t *testing.T) {
	// parent author is a third user -> owner + parent author
	// are distinct recipients.
	parentAuthor := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	store, tx := replyFixture()
	store.getCommentByID = func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
		return db.GetCommentByIDRow{ID: id, VideoID: testVideoID, UserID: parentAuthor, Content: "parent"}, nil
	}
	runner := &stubTxRunner{tx: tx}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/comments/:id/reply", sh.ReplyComment)

	rec := doJSON(e, http.MethodPost, "/api/comments/"+testCommentID.String()+"/reply", `{"content":"a reply"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(tx.notifications) != 2 {
		t.Fatalf("notifications = %d, want 2", len(tx.notifications))
	}
	got := map[uuid.UUID]bool{}
	for _, n := range tx.notifications {
		got[n.UserID] = true
	}
	if !got[testOwnerID] || !got[parentAuthor] {
		t.Errorf("must notify owner %s and parent author %s, got %v", testOwnerID, parentAuthor, got)
	}
}

func TestReplyComment_MissingParent404(t *testing.T) {
	store := &stubSocialStore{
		getCommentByID: func(ctx context.Context, id uuid.UUID) (db.GetCommentByIDRow, error) {
			return db.GetCommentByIDRow{}, sql.ErrNoRows
		},
	}
	runner := &stubTxRunner{tx: &stubSocialTx{}}
	sh := newSocialHandlerForTest(store, runner)
	e := newSocialTestEcho(store, runner, viewerUser(), http.MethodPost, "/api/comments/:id/reply", sh.ReplyComment)

	rec := doJSON(e, http.MethodPost, "/api/comments/"+testCommentID.String()+"/reply", `{"content":"hi"}`)
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
}

// ---------------------------------------------------------------------------
// parseAuthVideoParam (pure unit)
// ---------------------------------------------------------------------------

func TestParseAuthVideoParam_NoUser(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/videos/x/like", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// No Authenticate middleware ran -> no user on context.
	_, _, err := parseAuthVideoParam(c)
	if !errors.Is(err, shared.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestParseAuthVideoParam_InvalidUUID(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/videos/not-a-uuid/like", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/videos/:id/like")
	c.SetParamNames("id")
	c.SetParamValues("not-a-uuid")
	// Same literal key middleware.Authenticate sets; it is
	// unexported there, and this is the pure-unit path.
	c.Set("auth.currentUser", &db.User{ID: testViewerID, IsActive: true})
	_, _, err := parseAuthVideoParam(c)
	if !errors.Is(err, shared.ErrValidation) {
		t.Errorf("err = %v, want ErrValidation", err)
	}
}
