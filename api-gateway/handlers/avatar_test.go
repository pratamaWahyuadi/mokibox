package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type stubAvatarStore struct {
	setPending func(context.Context, db.SetPendingAvatarUploadParams) (db.SetPendingAvatarUploadRow, error)
	confirm    func(context.Context, db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error)
	clear      func(context.Context, db.ClearPendingAvatarUploadParams) (int64, error)
}

func (s stubAvatarStore) SetPendingAvatarUpload(ctx context.Context, arg db.SetPendingAvatarUploadParams) (db.SetPendingAvatarUploadRow, error) {
	if s.setPending == nil {
		return db.SetPendingAvatarUploadRow{}, errors.New("SetPendingAvatarUpload not scripted")
	}
	return s.setPending(ctx, arg)
}

func (s stubAvatarStore) ConfirmAvatarUpload(ctx context.Context, arg db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error) {
	if s.confirm == nil {
		return db.ConfirmAvatarUploadRow{}, errors.New("ConfirmAvatarUpload not scripted")
	}
	return s.confirm(ctx, arg)
}

func (s stubAvatarStore) ClearPendingAvatarUpload(ctx context.Context, arg db.ClearPendingAvatarUploadParams) (int64, error) {
	if s.clear == nil {
		return 0, errors.New("ClearPendingAvatarUpload not scripted")
	}
	return s.clear(ctx, arg)
}

func avatarTestPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAvatarUploadIntentPresignsUserScopedKey(t *testing.T) {
	expiry := time.Now().UTC().Add(15 * time.Minute)
	store := stubAvatarStore{
		setPending: func(_ context.Context, arg db.SetPendingAvatarUploadParams) (db.SetPendingAvatarUploadRow, error) {
			if arg.ID != testViewerID || !arg.PendingAvatarKey.Valid || !arg.PendingAvatarExpiresAt.Valid {
				t.Fatalf("pending upload params = %+v", arg)
			}
			return db.SetPendingAvatarUploadRow{
				PendingAvatarKey:       arg.PendingAvatarKey,
				PendingAvatarExpiresAt: arg.PendingAvatarExpiresAt,
			}, nil
		},
	}
	r2 := &stubR2{
		presignPut: func(_ context.Context, key, contentType string, gotExpiry time.Duration) (string, error) {
			if key == "" || contentType != "image/png" || gotExpiry != 15*time.Minute {
				t.Fatalf("PresignPut(%q, %q, %s)", key, contentType, gotExpiry)
			}
			if key[:len("avatars/"+testViewerID.String()+"/")] != "avatars/"+testViewerID.String()+"/" {
				t.Fatalf("avatar key is not user scoped: %q", key)
			}
			return "https://r2.example/put", nil
		},
	}
	h := NewAvatarHandlerForTest(store, r2, &stubEnqueuer{}, &shared.APIConfig{
		PresignUploadExpiry: 15 * time.Minute,
	})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/upload-intent", h.UploadIntent)

	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/upload-intent", `{"content_type":"image/png"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		AvatarKey     string            `json:"avatar_key"`
		HTTPMethod    string            `json:"http_method"`
		UploadURL     string            `json:"upload_url"`
		UploadHeaders map[string]string `json:"upload_headers"`
		MaxSizeBytes  int64             `json:"max_size_bytes"`
		ExpiresAt     string            `json:"expires_at"`
	}
	decodeData(t, rec, &got)
	if got.AvatarKey == "" || got.HTTPMethod != http.MethodPut || got.UploadURL != "https://r2.example/put" {
		t.Fatalf("upload intent response = %+v", got)
	}
	if got.UploadHeaders["Content-Type"] != "image/png" || got.MaxSizeBytes != maxAvatarBytes || got.ExpiresAt == "" {
		t.Fatalf("upload policy response = %+v", got)
	}
	if !expiry.After(time.Now()) {
		t.Fatal("test expiry was not in the future")
	}
}

func TestAvatarUploadIntentRejectsUnsupportedContentType(t *testing.T) {
	store := stubAvatarStore{}
	h := NewAvatarHandlerForTest(store, &stubR2{}, &stubEnqueuer{}, &shared.APIConfig{})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/upload-intent", h.UploadIntent)

	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/upload-intent", `{"content_type":"image/gif"}`)
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
}

func TestConfirmAvatarUploadValidatesImageAndCleansPreviousKey(t *testing.T) {
	key := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".png"
	oldKey := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".jpg"
	var gotArg db.ConfirmAvatarUploadParams
	store := stubAvatarStore{
		confirm: func(_ context.Context, arg db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error) {
			gotArg = arg
			return db.ConfirmAvatarUploadRow{
				ID:                testViewerID,
				AvatarKey:         sql.NullString{String: key, Valid: true},
				PreviousAvatarKey: sql.NullString{String: oldKey, Valid: true},
			}, nil
		},
	}
	r2 := &stubR2{
		headObject: func(_ context.Context, gotKey string) (int64, error) {
			if gotKey != key {
				t.Fatalf("HeadObject key = %q, want %q", gotKey, key)
			}
			return int64(len(avatarTestPNG(t))), nil
		},
		getObject: func(_ context.Context, gotKey string, maxBytes int64) ([]byte, error) {
			if gotKey != key || maxBytes != maxAvatarBytes {
				t.Fatalf("GetObject(%q, %d)", gotKey, maxBytes)
			}
			return avatarTestPNG(t), nil
		},
		presignGet: func(_ context.Context, gotKey string, _ time.Duration) (string, error) {
			if gotKey != key {
				t.Fatalf("PresignGet key = %q, want %q", gotKey, key)
			}
			return "https://r2.example/avatar", nil
		},
	}
	queue := &stubEnqueuer{}
	h := NewAvatarHandlerForTest(store, r2, queue, &shared.APIConfig{})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/confirm", h.ConfirmUpload)

	body, _ := json.Marshal(map[string]string{"avatar_key": key})
	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/confirm", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotArg.ID != testViewerID || !gotArg.PendingAvatarKey.Valid || gotArg.PendingAvatarKey.String != key {
		t.Fatalf("confirm params = %+v", gotArg)
	}
	if len(queue.cleanupObjectsKeys) != 1 || len(queue.cleanupObjectsKeys[0]) != 1 || queue.cleanupObjectsKeys[0][0] != oldKey {
		t.Fatalf("cleanup keys = %+v", queue.cleanupObjectsKeys)
	}
	var got struct {
		AvatarURL string `json:"avatar_url"`
	}
	decodeData(t, rec, &got)
	if got.AvatarURL != "https://r2.example/avatar" {
		t.Fatalf("avatar_url = %q", got.AvatarURL)
	}
}

func TestConfirmAvatarUploadEnforcesFiveMiBBoundary(t *testing.T) {
	key := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".png"
	for _, tc := range []struct {
		name       string
		size       int64
		wantStatus int
	}{
		{name: "maximum accepted", size: maxAvatarBytes, wantStatus: http.StatusOK},
		{name: "over maximum rejected", size: maxAvatarBytes + 1, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := &stubEnqueuer{}
			store := stubAvatarStore{
				confirm: func(_ context.Context, arg db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error) {
					return db.ConfirmAvatarUploadRow{
						ID:        arg.ID,
						AvatarKey: arg.PendingAvatarKey,
					}, nil
				},
				clear: func(_ context.Context, arg db.ClearPendingAvatarUploadParams) (int64, error) {
					return 1, nil
				},
			}
			r2 := &stubR2{
				headObject: func(context.Context, string) (int64, error) {
					return tc.size, nil
				},
				getObject: func(context.Context, string, int64) ([]byte, error) {
					if tc.size > maxAvatarBytes {
						t.Fatal("GetObject must not run for an oversized object")
					}
					return avatarTestPNG(t), nil
				},
				presignGet: func(context.Context, string, time.Duration) (string, error) {
					return "https://r2.example/avatar", nil
				},
			}
			h := NewAvatarHandlerForTest(store, r2, queue, &shared.APIConfig{})
			e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/confirm", h.ConfirmUpload)

			body, _ := json.Marshal(map[string]string{"avatar_key": key})
			rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/confirm", string(body))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.size == maxAvatarBytes && len(queue.cleanupObjectsKeys) != 0 {
				t.Fatalf("valid boundary upload unexpectedly queued cleanup: %v", queue.cleanupObjectsKeys)
			}
			if tc.size > maxAvatarBytes {
				assertErrCode(t, rec, http.StatusBadRequest, shared.CodeUploadSizeInvalid)
				if len(queue.cleanupObjectsKeys) != 1 || queue.cleanupObjectsKeys[0][0] != key {
					t.Fatalf("oversized object cleanup = %v", queue.cleanupObjectsKeys)
				}
			}
		})
	}
}

func TestConfirmAvatarUploadExpiredIntentIsConflictAndCleaned(t *testing.T) {
	key := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".png"
	queue := &stubEnqueuer{}
	store := stubAvatarStore{
		confirm: func(context.Context, db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error) {
			return db.ConfirmAvatarUploadRow{}, sql.ErrNoRows
		},
		clear: func(_ context.Context, arg db.ClearPendingAvatarUploadParams) (int64, error) {
			if arg.ID != testViewerID || arg.PendingAvatarKey.String != key {
				t.Fatalf("clear pending params = %+v", arg)
			}
			return 1, nil
		},
	}
	r2 := &stubR2{
		headObject: func(context.Context, string) (int64, error) { return 100, nil },
		getObject:  func(context.Context, string, int64) ([]byte, error) { return avatarTestPNG(t), nil },
		presignGet: func(context.Context, string, time.Duration) (string, error) {
			return "https://r2.example/avatar", nil
		},
	}
	h := NewAvatarHandlerForTest(store, r2, queue, &shared.APIConfig{})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/confirm", h.ConfirmUpload)

	body, _ := json.Marshal(map[string]string{"avatar_key": key})
	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/confirm", string(body))
	assertErrCode(t, rec, http.StatusConflict, shared.CodeAvatarUploadConflict)
	if len(queue.cleanupObjectsKeys) != 1 || queue.cleanupObjectsKeys[0][0] != key {
		t.Fatalf("expired intent cleanup = %v", queue.cleanupObjectsKeys)
	}
}

func TestConfirmAvatarUploadRejectsInvalidImageAndClearsIntent(t *testing.T) {
	key := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".png"
	cleared := false
	store := stubAvatarStore{
		clear: func(_ context.Context, arg db.ClearPendingAvatarUploadParams) (int64, error) {
			cleared = arg.ID == testViewerID && arg.PendingAvatarKey.String == key
			return 1, nil
		},
		confirm: func(context.Context, db.ConfirmAvatarUploadParams) (db.ConfirmAvatarUploadRow, error) {
			t.Fatal("ConfirmAvatarUpload must not run for invalid bytes")
			return db.ConfirmAvatarUploadRow{}, nil
		},
	}
	r2 := &stubR2{
		headObject: func(context.Context, string) (int64, error) { return 12, nil },
		getObject:  func(context.Context, string, int64) ([]byte, error) { return []byte("not an image"), nil },
	}
	queue := &stubEnqueuer{}
	h := NewAvatarHandlerForTest(store, r2, queue, &shared.APIConfig{})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/confirm", h.ConfirmUpload)

	body, _ := json.Marshal(map[string]string{"avatar_key": key})
	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/confirm", string(body))
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
	if !cleared {
		t.Fatal("invalid upload intent was not cleared")
	}
	if len(queue.cleanupObjectsKeys) != 1 || queue.cleanupObjectsKeys[0][0] != key {
		t.Fatalf("invalid object cleanup = %+v", queue.cleanupObjectsKeys)
	}
}

func TestConfirmAvatarUploadRejectsAnotherUsersKey(t *testing.T) {
	key := "avatars/" + uuid.NewString() + "/" + uuid.NewString() + ".png"
	store := stubAvatarStore{}
	r2 := &stubR2{}
	h := NewAvatarHandlerForTest(store, r2, &stubEnqueuer{}, &shared.APIConfig{})
	e := harnessAuthEcho(viewerUser(), http.MethodPost, "/api/users/me/avatar/confirm", h.ConfirmUpload)

	body, _ := json.Marshal(map[string]string{"avatar_key": key})
	rec := doJSON(e, http.MethodPost, "/api/users/me/avatar/confirm", string(body))
	assertErrCode(t, rec, http.StatusBadRequest, shared.CodeValidationError)
}

func TestAvatarURLMappingPresignsPrivateAvatarForProfileAndSummary(t *testing.T) {
	key := "avatars/" + testViewerID.String() + "/" + uuid.NewString() + ".png"
	const signedURL = "https://r2.example/signed-avatar"
	var presignCalls int
	r2 := &stubR2{
		presignGet: func(_ context.Context, gotKey string, _ time.Duration) (string, error) {
			presignCalls++
			if gotKey != key {
				t.Fatalf("PresignGet key = %q, want %q", gotKey, key)
			}
			return signedURL, nil
		},
	}
	cfg := &shared.APIConfig{MediaTokenTTL: time.Minute}
	user := db.User{
		ID:        testViewerID,
		Username:  "viewer",
		IsActive:  true,
		AvatarKey: sql.NullString{String: key, Valid: true},
	}

	profile, err := userProfileFromDB(context.Background(), r2, cfg, user)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := userSummaryFromRow(context.Background(), r2, cfg,
		sql.NullString{}, sql.NullString{}, user.AvatarKey, user.ID, user.Username, false)
	if err != nil {
		t.Fatal(err)
	}
	if profile.AvatarURL == nil || *profile.AvatarURL != signedURL {
		t.Errorf("profile avatar_url = %v, want %q", profile.AvatarURL, signedURL)
	}
	if summary.AvatarURL == nil || *summary.AvatarURL != signedURL {
		t.Errorf("summary avatar_url = %v, want %q", summary.AvatarURL, signedURL)
	}
	if presignCalls != 2 {
		t.Errorf("PresignGet calls = %d, want 2", presignCalls)
	}
}
