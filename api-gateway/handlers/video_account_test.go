// Package handlers - video_account_test.go covers the
// batch-2 tx-heavy surfaces: VideoHandler.ConfirmUpload /
// DeleteVideo / UploadIntent and the account deletion
// cascade. Same stub patterns as social_test.go — no
// Postgres/Redis/R2 touched.
package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/api-gateway/middleware"
	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

// ---------------------------------------------------------------------------
// Video stubs.
// ---------------------------------------------------------------------------

// stubVideoStore scripts videoStore reads.
type stubVideoStore struct {
	getVideoByID       func(ctx context.Context, id uuid.UUID) (db.Video, error)
	getUserByID        func(ctx context.Context, id uuid.UUID) (db.User, error)
	isFollowing        func(ctx context.Context, arg db.IsFollowingParams) (bool, error)
	getPendingVideo    func(ctx context.Context, userID uuid.UUID) (db.Video, error)
	insertVideo        func(ctx context.Context, arg db.InsertVideoParams) (db.Video, error)
	updateR2Key        func(ctx context.Context, arg db.UpdatePendingVideoR2KeyParams) (db.Video, error)
	updateMetadata     func(ctx context.Context, arg db.UpdatePendingVideoMetadataParams) (db.Video, error)
	getVideoDetail     func(ctx context.Context, arg db.GetVideoDetailParams) (db.GetVideoDetailRow, error)
	markVideoDeleted   func(ctx context.Context, arg db.MarkVideoDeletedParams) (db.Video, error)
}

func (s *stubVideoStore) GetVideoByID(ctx context.Context, id uuid.UUID) (db.Video, error) {
	if s.getVideoByID == nil {
		return db.Video{}, errors.New("stubVideoStore: getVideoByID not scripted")
	}
	return s.getVideoByID(ctx, id)
}
func (s *stubVideoStore) GetUserByID(ctx context.Context, id uuid.UUID) (db.User, error) {
	if s.getUserByID == nil {
		return db.User{}, errors.New("stubVideoStore: getUserByID not scripted")
	}
	return s.getUserByID(ctx, id)
}
func (s *stubVideoStore) IsFollowing(ctx context.Context, arg db.IsFollowingParams) (bool, error) {
	if s.isFollowing == nil {
		return false, errors.New("stubVideoStore: isFollowing not scripted")
	}
	return s.isFollowing(ctx, arg)
}
func (s *stubVideoStore) GetPendingVideoByUser(ctx context.Context, userID uuid.UUID) (db.Video, error) {
	if s.getPendingVideo == nil {
		return db.Video{}, errors.New("stubVideoStore: getPendingVideo not scripted")
	}
	return s.getPendingVideo(ctx, userID)
}
func (s *stubVideoStore) InsertVideo(ctx context.Context, arg db.InsertVideoParams) (db.Video, error) {
	if s.insertVideo == nil {
		return db.Video{}, errors.New("stubVideoStore: insertVideo not scripted")
	}
	return s.insertVideo(ctx, arg)
}
func (s *stubVideoStore) UpdatePendingVideoR2Key(ctx context.Context, arg db.UpdatePendingVideoR2KeyParams) (db.Video, error) {
	if s.updateR2Key == nil {
		return db.Video{}, errors.New("stubVideoStore: updateR2Key not scripted")
	}
	return s.updateR2Key(ctx, arg)
}
func (s *stubVideoStore) UpdatePendingVideoMetadata(ctx context.Context, arg db.UpdatePendingVideoMetadataParams) (db.Video, error) {
	if s.updateMetadata == nil {
		return db.Video{}, errors.New("stubVideoStore: updateMetadata not scripted")
	}
	return s.updateMetadata(ctx, arg)
}
func (s *stubVideoStore) GetVideoDetail(ctx context.Context, arg db.GetVideoDetailParams) (db.GetVideoDetailRow, error) {
	if s.getVideoDetail == nil {
		return db.GetVideoDetailRow{}, errors.New("stubVideoStore: getVideoDetail not scripted")
	}
	return s.getVideoDetail(ctx, arg)
}
func (s *stubVideoStore) MarkVideoDeleted(ctx context.Context, arg db.MarkVideoDeletedParams) (db.Video, error) {
	if s.markVideoDeleted == nil {
		return db.Video{}, errors.New("stubVideoStore: markVideoDeleted not scripted")
	}
	return s.markVideoDeleted(ctx, arg)
}

// stubVideoTx scripts the confirm-tx body.
type stubVideoTx struct {
	getForUpdate   func(ctx context.Context, id uuid.UUID) (db.Video, error)
	confirmFlip    func(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error)
}

func (s *stubVideoTx) GetVideoByIDForUpdate(ctx context.Context, id uuid.UUID) (db.Video, error) {
	if s.getForUpdate == nil {
		return db.Video{}, errors.New("stubVideoTx: getForUpdate not scripted")
	}
	return s.getForUpdate(ctx, id)
}
func (s *stubVideoTx) ConfirmVideoProcessing(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error) {
	if s.confirmFlip == nil {
		return db.Video{}, errors.New("stubVideoTx: confirmFlip not scripted")
	}
	return s.confirmFlip(ctx, arg)
}

// stubVideoTxRunner mirrors sqlTxRunner for videoTxStore.
type stubVideoTxRunner struct {
	tx       *stubVideoTx
	beginErr error
	runs     int
	committed bool
	aborted   bool
}

func (r *stubVideoTxRunner) Run(ctx context.Context, fn func(q videoTxStore) error) error {
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

// stubR2 scripts the R2 surface.
type stubR2 struct {
	presignPut  func(ctx context.Context, key, contentType string, expiry time.Duration) (string, error)
	presignGet  func(ctx context.Context, key string, expiry time.Duration) (string, error)
	headObject  func(ctx context.Context, key string) (int64, error)
	getObject   func(ctx context.Context, key string, maxBytes int64) ([]byte, error)
}

func (s *stubR2) PresignPut(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
	if s.presignPut == nil {
		return "", errors.New("stubR2: presignPut not scripted")
	}
	return s.presignPut(ctx, key, contentType, expiry)
}
func (s *stubR2) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	if s.presignGet == nil {
		return "", errors.New("stubR2: presignGet not scripted")
	}
	return s.presignGet(ctx, key, expiry)
}
func (s *stubR2) HeadObject(ctx context.Context, key string) (int64, error) {
	if s.headObject == nil {
		return 0, errors.New("stubR2: headObject not scripted")
	}
	return s.headObject(ctx, key)
}
func (s *stubR2) GetObject(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	if s.getObject == nil {
		return nil, errors.New("stubR2: getObject not scripted")
	}
	return s.getObject(ctx, key, maxBytes)
}

// stubEnqueuer records every enqueue call.
type stubEnqueuer struct {
	transcodeErr       error
	cleanupObjectsErr  error
	cleanupVideoErr    error
	transcodePayloads  []shared.TranscodeVideoPayload
	cleanupObjectsKeys [][]string
	cleanupVideoIDs    []string
}

func (s *stubEnqueuer) EnqueueTranscode(payload shared.TranscodeVideoPayload) (*asynq.TaskInfo, error) {
	if s.transcodeErr != nil {
		return nil, s.transcodeErr
	}
	s.transcodePayloads = append(s.transcodePayloads, payload)
	return nil, nil
}
func (s *stubEnqueuer) EnqueueCleanupObjects(payload shared.CleanupObjectsPayload) (*asynq.TaskInfo, error) {
	if s.cleanupObjectsErr != nil {
		return nil, s.cleanupObjectsErr
	}
	s.cleanupObjectsKeys = append(s.cleanupObjectsKeys, payload.Keys)
	return nil, nil
}
func (s *stubEnqueuer) EnqueueCleanupVideo(payload shared.CleanupVideoPayload, delay time.Duration) (*asynq.TaskInfo, error) {
	if s.cleanupVideoErr != nil {
		return nil, s.cleanupVideoErr
	}
	s.cleanupVideoIDs = append(s.cleanupVideoIDs, payload.VideoID)
	return nil, nil
}

// ---------------------------------------------------------------------------
// ConfirmUpload tests.
// ---------------------------------------------------------------------------

var testConfirmCfg = &shared.APIConfig{
	PresignUploadExpiry: 15 * time.Minute,
	APIBaseURL:         "https://api.example.com",
	MediaTokenSecret:   "test-secret",
}

const testR2Key = "uploads/22222222-2222-2222-2222-222222222222/33333333-3333-3333-3333-333333333333/source.mp4"

func newVideoEcho(vh *VideoHandler, method, path string, h echo.HandlerFunc) *echo.Echo {
	e := echo.New()
	e.Validator = testValidator{v: validator.New()}
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	e.Add(method, path, h)
	return e
}

func TestConfirmUpload_HappyPath(t *testing.T) {
	store := &stubVideoStore{}
	tx := &stubVideoTx{
		getForUpdate: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "PENDING_UPLOAD", R2Key: testR2Key}, nil
		},
		confirmFlip: func(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error) {
			return db.Video{ID: arg.ID, UserID: arg.UserID, Status: "PROCESSING", RetryCount: 0}, nil
		},
	}
	runner := &stubVideoTxRunner{tx: tx}
	r2 := &stubR2{
		headObject: func(ctx context.Context, key string) (int64, error) {
			if key != testR2Key {
				t.Errorf("HeadObject key = %s, want %s", key, testR2Key)
			}
			return 5 * 1024 * 1024, nil // 5 MB, in range
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, runner, r2, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/confirm", vh.ConfirmUpload)

	body := fmt.Sprintf(`{"video_id":%q,"r2_key":%q}`, testVideoID.String(), testR2Key)
	rec := doJSON(e, http.MethodPost, "/api/videos/confirm", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got confirmResponse
	decodeData(t, rec, &got)
	if got.VideoID != testVideoID.String() || got.Status != "PROCESSING" || got.RetryCount != 0 {
		t.Errorf("confirmResponse = %+v", got)
	}
	if !runner.committed {
		t.Error("confirm must commit on success")
	}
	if len(enq.transcodePayloads) != 1 || enq.transcodePayloads[0].VideoID != testVideoID.String() {
		t.Errorf("transcode enqueues = %+v, want 1 with the confirmed id", enq.transcodePayloads)
	}
}

func TestConfirmUpload_MissingR2Object409(t *testing.T) {
	tx := &stubVideoTx{
		getForUpdate: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "PENDING_UPLOAD", R2Key: testR2Key}, nil
		},
		confirmFlip: func(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error) {
			t.Error("state must not flip when the R2 object is missing")
			return db.Video{}, nil
		},
	}
	runner := &stubVideoTxRunner{tx: tx}
	r2 := &stubR2{
		headObject: func(ctx context.Context, key string) (int64, error) {
			return 0, shared.Wrap(shared.ErrNotFound, "no such object") // R2 miss
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(&stubVideoStore{}, runner, r2, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/confirm", vh.ConfirmUpload)

	body := fmt.Sprintf(`{"video_id":%q,"r2_key":%q}`, testVideoID.String(), testR2Key)
	rec := doJSON(e, http.MethodPost, "/api/videos/confirm", body)
	assertErrCode(t, rec, http.StatusConflict, shared.CodeUploadMissing)
	if runner.committed {
		t.Error("missing object must not commit (row must stay PENDING_UPLOAD)")
	}
}

func TestConfirmUpload_SizeInvalid400(t *testing.T) {
	tx := &stubVideoTx{
		getForUpdate: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "PENDING_UPLOAD", R2Key: testR2Key}, nil
		},
		confirmFlip: func(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error) {
			t.Error("state must not flip for an invalid-size object")
			return db.Video{}, nil
		},
	}
	runner := &stubVideoTxRunner{tx: tx}
	r2 := &stubR2{
		headObject: func(ctx context.Context, key string) (int64, error) {
			return 100, nil // below MinUploadBytes (1 KB)
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(&stubVideoStore{}, runner, r2, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/confirm", vh.ConfirmUpload)

	body := fmt.Sprintf(`{"video_id":%q,"r2_key":%q}`, testVideoID.String(), testR2Key)
	rec := doJSON(e, http.MethodPost, "/api/videos/confirm", body)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeUploadSizeInvalid)
	if len(enq.cleanupObjectsKeys) != 1 {
		t.Errorf("invalid-size upload must enqueue best-effort cleanup, got %+v", enq.cleanupObjectsKeys)
	}
}

func TestConfirmUpload_StatusConflict409(t *testing.T) {
	tx := &stubVideoTx{
		getForUpdate: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			// Already processing: concurrent confirm.
			return db.Video{ID: id, UserID: testViewerID, Status: "PROCESSING", R2Key: testR2Key}, nil
		},
	}
	runner := &stubVideoTxRunner{tx: tx}
	vh := newVideoHandlerForTest(&stubVideoStore{}, runner, &stubR2{}, &stubEnqueuer{}, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/confirm", vh.ConfirmUpload)

	body := fmt.Sprintf(`{"video_id":%q,"r2_key":%q}`, testVideoID.String(), testR2Key)
	rec := doJSON(e, http.MethodPost, "/api/videos/confirm", body)
	assertErrCode(t, rec, http.StatusConflict, shared.CodeVideoStatusConflict)
}

func TestConfirmUpload_EnqueueFailureRollsBack(t *testing.T) {
	// Pre-refactor semantics preserved: the transcode
	// enqueue runs INSIDE the tx body; on failure the tx
	// rolls back and the row stays PENDING_UPLOAD.
	tx := &stubVideoTx{
		getForUpdate: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "PENDING_UPLOAD", R2Key: testR2Key}, nil
		},
		confirmFlip: func(ctx context.Context, arg db.ConfirmVideoProcessingParams) (db.Video, error) {
			return db.Video{ID: arg.ID, Status: "PROCESSING"}, nil
		},
	}
	runner := &stubVideoTxRunner{tx: tx}
	r2 := &stubR2{
		headObject: func(ctx context.Context, key string) (int64, error) { return 5 * 1024 * 1024, nil },
	}
	enq := &stubEnqueuer{transcodeErr: errors.New("redis down")}
	vh := newVideoHandlerForTest(&stubVideoStore{}, runner, r2, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/confirm", vh.ConfirmUpload)

	body := fmt.Sprintf(`{"video_id":%q,"r2_key":%q}`, testVideoID.String(), testR2Key)
	rec := doJSON(e, http.MethodPost, "/api/videos/confirm", body)
	assertErrCode(t, rec, http.StatusInternalServerError, shared.CodeInternalError)
	if runner.committed {
		t.Error("enqueue failure must roll back (row stays PENDING_UPLOAD)")
	}
}

// ---------------------------------------------------------------------------
// DeleteVideo tests.
// ---------------------------------------------------------------------------

func TestDeleteVideo_TombstonedIdempotentEarly204(t *testing.T) {
	store := &stubVideoStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "DELETED"}, nil
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodDelete, "/api/videos/:id", vh.DeleteVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (idempotent tombstone)", rec.Code)
	}
	if len(enq.cleanupVideoIDs) != 0 {
		t.Errorf("already-DELETED video must not re-enqueue cleanup, got %v", enq.cleanupVideoIDs)
	}
}

func TestDeleteVideo_HappyPathEnqueues24hCleanup(t *testing.T) {
	store := &stubVideoStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "READY"}, nil
		},
		markVideoDeleted: func(ctx context.Context, arg db.MarkVideoDeletedParams) (db.Video, error) {
			return db.Video{ID: arg.ID, UserID: arg.UserID, Status: "DELETED"}, nil
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodDelete, "/api/videos/:id", vh.DeleteVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if len(enq.cleanupVideoIDs) != 1 || enq.cleanupVideoIDs[0] != testVideoID.String() {
		t.Errorf("cleanup:video enqueues = %v, want [%s]", enq.cleanupVideoIDs, testVideoID)
	}
}

func TestDeleteVideo_NonOwner404(t *testing.T) {
	store := &stubVideoStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testOwnerID, Status: "READY"}, nil // someone else's
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodDelete, "/api/videos/:id", vh.DeleteVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String(), "")
	assertErrCode(t, rec, http.StatusNotFound, shared.CodeNotFound)
	if len(enq.cleanupVideoIDs) != 0 {
		t.Errorf("non-owner delete must not enqueue cleanup")
	}
}

func TestDeleteVideo_MarkRaceTreatedAs204(t *testing.T) {
	// Two concurrent deletes: the second MarkVideoDeleted
	// returns sql.ErrNoRows -> treated as success (204).
	store := &stubVideoStore{
		getVideoByID: func(ctx context.Context, id uuid.UUID) (db.Video, error) {
			return db.Video{ID: id, UserID: testViewerID, Status: "READY"}, nil
		},
		markVideoDeleted: func(ctx context.Context, arg db.MarkVideoDeletedParams) (db.Video, error) {
			return db.Video{}, sql.ErrNoRows
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, &stubR2{}, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodDelete, "/api/videos/:id", vh.DeleteVideo)

	rec := doJSON(e, http.MethodDelete, "/api/videos/"+testVideoID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (race treated as success)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// UploadIntent (single read path worth pinning: reuse + key rotation).
// ---------------------------------------------------------------------------

func TestUploadIntent_ReusesPendingRowAndRotatesKey(t *testing.T) {
	existingID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	store := &stubVideoStore{
		getPendingVideo: func(ctx context.Context, userID uuid.UUID) (db.Video, error) {
			return db.Video{ID: existingID, UserID: userID, Status: "PENDING_UPLOAD", R2Key: "uploads/x/old/source.mp4"}, nil
		},
		updateR2Key: func(ctx context.Context, arg db.UpdatePendingVideoR2KeyParams) (db.Video, error) {
			if arg.ID != existingID {
				t.Errorf("rotate target = %s, want existing %s", arg.ID, existingID)
			}
			return db.Video{ID: arg.ID, R2Key: arg.R2Key}, nil
		},
		updateMetadata: func(ctx context.Context, arg db.UpdatePendingVideoMetadataParams) (db.Video, error) {
			return db.Video{ID: arg.ID}, nil
		},
	}
	r2 := &stubR2{
		presignPut: func(ctx context.Context, key, contentType string, expiry time.Duration) (string, error) {
			return "https://r2.example.com/put", nil
		},
	}
	enq := &stubEnqueuer{}
	vh := newVideoHandlerForTest(store, &stubVideoTxRunner{}, r2, enq, testConfirmCfg)
	e := newVideoEcho(vh, http.MethodPost, "/api/videos/upload-intent", vh.UploadIntent)

	rec := doJSON(e, http.MethodPost, "/api/videos/upload-intent", `{"title":"my clip"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (reuse); body=%s", rec.Code, rec.Body.String())
	}
	var got uploadIntentResponse
	decodeData(t, rec, &got)
	if got.VideoID != existingID.String() {
		t.Errorf("reused videoID = %s, want %s", got.VideoID, existingID)
	}
	if len(enq.cleanupObjectsKeys) != 1 {
		t.Errorf("old key must be enqueued for best-effort cleanup, got %+v", enq.cleanupObjectsKeys)
	}
}

// ---------------------------------------------------------------------------
// Account delete cascade.
// ---------------------------------------------------------------------------

// stubAccountTx records the delete cascade call order.
type stubAccountTx struct {
	listVideoKeys      func(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error)
	tombstoneErr       error
	calls              []string
}

func (s *stubAccountTx) ListVideoKeysByUser(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error) {
	s.calls = append(s.calls, "ListVideoKeysByUser")
	if s.listVideoKeys == nil {
		return nil, errors.New("stubAccountTx: listVideoKeys not scripted")
	}
	return s.listVideoKeys(ctx, userID)
}
func (s *stubAccountTx) DecrementLikesForUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DecrementLikesForUser")
	return nil
}
func (s *stubAccountTx) DecrementCommentsForUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DecrementCommentsForUser")
	return nil
}
func (s *stubAccountTx) DeleteVideosByUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteVideosByUser")
	return nil
}
func (s *stubAccountTx) DeleteFollowsByFollower(ctx context.Context, followerID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteFollowsByFollower")
	return nil
}
func (s *stubAccountTx) DeleteFollowsByFollowee(ctx context.Context, followeeID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteFollowsByFollowee")
	return nil
}
func (s *stubAccountTx) DeleteLikesByUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteLikesByUser")
	return nil
}
func (s *stubAccountTx) DeleteCommentsByUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteCommentsByUser")
	return nil
}
func (s *stubAccountTx) DeleteNotificationsForUser(ctx context.Context, userID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteNotificationsForUser")
	return nil
}
func (s *stubAccountTx) DeleteNotificationsByActor(ctx context.Context, actorID uuid.UUID) error {
	s.calls = append(s.calls, "DeleteNotificationsByActor")
	return nil
}
func (s *stubAccountTx) TombstoneUser(ctx context.Context, id uuid.UUID) (db.User, error) {
	s.calls = append(s.calls, "TombstoneUser")
	if s.tombstoneErr != nil {
		return db.User{}, s.tombstoneErr
	}
	return db.User{ID: id, IsActive: false}, nil
}

// stubAccountRunner mirrors sqlTxRunner for accountTxStore.
type stubAccountRunner struct {
	tx         *stubAccountTx
	committed  bool
	aborted    bool
}

func (r *stubAccountRunner) Run(ctx context.Context, fn func(q accountTxStore) error) error {
	if err := fn(r.tx); err != nil {
		r.aborted = true
		return err
	}
	r.committed = true
	return nil
}

func TestDeleteMe_HappyPathCascadeAndEnqueue(t *testing.T) {
	tx := &stubAccountTx{
		listVideoKeys: func(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error) {
			return []db.ListVideoKeysByUserRow{
				{R2Key: "uploads/x/1/source.mp4", ThumbnailKey: sql.NullString{String: "thumbs/1.jpg", Valid: true}},
				{R2Key: "uploads/x/2/source.mp4"},
			}, nil
		},
	}
	runner := &stubAccountRunner{tx: tx}
	enq := &stubEnqueuer{}
	ah := newAccountHandlerForTest(runner, enq)

	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	e.DELETE("/api/users/me", ah.DeleteMe)

	req := httptest.NewRequest(http.MethodDelete, "/api/users/me", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer t")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	// Full cascade order (LLD section 11).
	wantOrder := []string{
		"ListVideoKeysByUser", "DecrementLikesForUser", "DecrementCommentsForUser",
		"DeleteVideosByUser", "DeleteFollowsByFollower", "DeleteFollowsByFollowee",
		"DeleteLikesByUser", "DeleteCommentsByUser", "DeleteNotificationsForUser",
		"DeleteNotificationsByActor", "TombstoneUser",
	}
	if len(tx.calls) != len(wantOrder) {
		t.Fatalf("cascade calls = %v, want %v", tx.calls, wantOrder)
	}
	for i, want := range wantOrder {
		if tx.calls[i] != want {
			t.Fatalf("cascade step %d = %s, want %s (full: %v)", i, tx.calls[i], want, tx.calls)
		}
	}
	if !runner.committed {
		t.Error("successful delete must commit")
	}
	// r2_key + valid thumbnail key per video = 3 keys.
	if len(enq.cleanupObjectsKeys) != 1 || len(enq.cleanupObjectsKeys[0]) != 3 {
		t.Errorf("cleanup keys = %+v, want 1 batch with 3 keys", enq.cleanupObjectsKeys)
	}
}

func TestDeleteMe_TombstoneMissingUserRollsBackAndAcks(t *testing.T) {
	// Webhook path: Zitadel notifies about a user with no
	// local row. TombstoneUser -> sql.ErrNoRows -> rollback
	// + nil (ack), and NO cleanup enqueue.
	tx := &stubAccountTx{
		listVideoKeys: func(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error) {
			return nil, nil
		},
		tombstoneErr: sql.ErrNoRows,
	}
	runner := &stubAccountRunner{tx: tx}
	enq := &stubEnqueuer{}
	ah := newAccountHandlerForTest(runner, enq)

	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	e.DELETE("/api/users/me", ah.DeleteMe)

	req := httptest.NewRequest(http.MethodDelete, "/api/users/me", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer t")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (missing user acked)", rec.Code)
	}
	if runner.committed {
		t.Error("missing user must NOT commit the cascade")
	}
	if len(enq.cleanupObjectsKeys) != 0 {
		t.Errorf("missing user must not enqueue cleanup, got %+v", enq.cleanupObjectsKeys)
	}
}

func TestDeleteMe_CascadeFailureIs500(t *testing.T) {
	tx := &stubAccountTx{
		listVideoKeys: func(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error) {
			return nil, errors.New("db down")
		},
	}
	runner := &stubAccountRunner{tx: tx}
	ah := newAccountHandlerForTest(runner, &stubEnqueuer{})

	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	e.DELETE("/api/users/me", ah.DeleteMe)

	req := httptest.NewRequest(http.MethodDelete, "/api/users/me", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer t")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	assertErrCode(t, rec, http.StatusInternalServerError, shared.CodeInternalError)
	if runner.committed {
		t.Error("cascade failure must not commit")
	}
}

// deleteUserData enqueue-failure path: post-commit warn,
// request still succeeds (dual-write trap documentation).
func TestDeleteMe_EnqueueFailureStillSucceeds(t *testing.T) {
	tx := &stubAccountTx{
		listVideoKeys: func(ctx context.Context, userID uuid.UUID) ([]db.ListVideoKeysByUserRow, error) {
			return []db.ListVideoKeysByUserRow{{R2Key: "uploads/x/1/source.mp4"}}, nil
		},
	}
	runner := &stubAccountRunner{tx: tx}
	enq := &stubEnqueuer{cleanupObjectsErr: errors.New("redis down")}
	ah := newAccountHandlerForTest(runner, enq)

	e := echo.New()
	e.Use(middleware.Authenticate(middleware.AuthenticateConfig{
		Verifier: testVerifier{sub: "test-sub"},
		Queries:  testUserStore{user: viewerUser()},
	}))
	e.DELETE("/api/users/me", ah.DeleteMe)

	req := httptest.NewRequest(http.MethodDelete, "/api/users/me", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer t")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// Tombstone committed; enqueue failure is best-effort
	// (dual-write trap: warn, not fail).
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (enqueue failure must not fail the tombstone)", rec.Code)
	}
	if !runner.committed {
		t.Error("cascade must be committed before the enqueue attempt")
	}
}

// Compile-time interface satisfaction checks for the
// production adapters.
var (
	_ taskEnqueuer   = asynqEnqueuer{}
	_ r2ObjectStore  = (*stubR2)(nil)
	_ videoStore     = (*stubVideoStore)(nil)
	_ videoTxStore   = (*stubVideoTx)(nil)
	_ accountTxStore = (*stubAccountTx)(nil)
)
