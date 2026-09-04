package shared

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-secret-at-least-32-bytes-long!!"

func TestMediaToken_Valid(t *testing.T) {
	videoID := "123e4567-e89b-12d3-a456-426614174000"
	token, expiry, err := NewMediaToken(videoID, testSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewMediaToken: %v", err)
	}
	if token == "" {
		t.Fatal("NewMediaToken returned empty token")
	}
	if time.Until(expiry) <= 0 {
		t.Errorf("expiry should be in the future, got %v", expiry)
	}
	if err := VerifyMediaToken(token, videoID, testSecret); err != nil {
		t.Fatalf("VerifyMediaToken(valid): %v", err)
	}
}

func TestMediaToken_Expired(t *testing.T) {
	videoID := "123e4567-e89b-12d3-a456-426614174000"
	// SignMediaToken lets us build an already-expired token
	// deterministically instead of sleeping out the TTL.
	expired := time.Now().Add(-time.Minute)
	token, err := SignMediaToken(videoID, testSecret, expired)
	if err != nil {
		t.Fatalf("SignMediaToken: %v", err)
	}
	err = VerifyMediaToken(token, videoID, testSecret)
	if err == nil {
		t.Fatal("expired token must fail verification")
	}
	if !errors.Is(err, ErrMediaTokenInvalid) {
		t.Errorf("expected ErrMediaTokenInvalid, got %v", err)
	}
}

func TestMediaToken_Tampered(t *testing.T) {
	videoID := "123e4567-e89b-12d3-a456-426614174000"
	token, _, err := NewMediaToken(videoID, testSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewMediaToken: %v", err)
	}

	// Tamper with each segment of the raw payload.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	parts := strings.SplitN(string(raw), ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token payload malformed: %q", raw)
	}

	tamperedSig := base64.RawURLEncoding.EncodeToString([]byte(parts[0] + ".deadbeef"))
	t.Run("signature", func(t *testing.T) {
		err := VerifyMediaToken(tamperedSig, videoID, testSecret)
		if !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("tampered signature must fail with ErrMediaTokenInvalid, got %v", err)
		}
	})

	// Rebinding: valid token for video A replayed against video B.
	otherVideo := "00000000-0000-0000-0000-000000000001"
	t.Run("rebound to other video", func(t *testing.T) {
		err := VerifyMediaToken(token, otherVideo, testSecret)
		if !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("token bound to another video must fail, got %v", err)
		}
	})

	// Wrong secret.
	t.Run("wrong secret", func(t *testing.T) {
		err := VerifyMediaToken(token, videoID, "another-secret-value-32-bytes-long")
		if !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("wrong secret must fail, got %v", err)
		}
	})

	// Malformed inputs.
	t.Run("empty token", func(t *testing.T) {
		if err := VerifyMediaToken("", videoID, testSecret); !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("empty token must fail, got %v", err)
		}
	})
	t.Run("not base64", func(t *testing.T) {
		if err := VerifyMediaToken("!!!", videoID, testSecret); !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("non-base64 token must fail, got %v", err)
		}
	})
	t.Run("no dot separator", func(t *testing.T) {
		noDot := base64.RawURLEncoding.EncodeToString([]byte("nodot"))
		if err := VerifyMediaToken(noDot, videoID, testSecret); !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("token without separator must fail, got %v", err)
		}
	})
	t.Run("non-numeric expiry", func(t *testing.T) {
		bad := base64.RawURLEncoding.EncodeToString([]byte("notanumber." + parts[1]))
		if err := VerifyMediaToken(bad, videoID, testSecret); !errors.Is(err, ErrMediaTokenInvalid) {
			t.Errorf("non-numeric expiry must fail, got %v", err)
		}
	})
}

func TestMediaToken_BoundToVideoID(t *testing.T) {
	// The HMAC payload embeds video_id, so a signature
	// computed for one expiry must not verify for a
	// different expiry value even with the right secret.
	videoID := "123e4567-e89b-12d3-a456-426614174000"
	token, _, err := NewMediaToken(videoID, testSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewMediaToken: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	parts := strings.SplitN(string(raw), ".", 2)
	futureExpiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	rewritten := base64.RawURLEncoding.EncodeToString([]byte(futureExpiry + "." + parts[1]))
	if err := VerifyMediaToken(rewritten, videoID, testSecret); !errors.Is(err, ErrMediaTokenInvalid) {
		t.Errorf("expiry-rewritten token must fail (HMAC binds expiry), got %v", err)
	}
}

func TestNewMediaToken_InputValidation(t *testing.T) {
	videoID := "123e4567-e89b-12d3-a456-426614174000"
	if _, _, err := NewMediaToken("", testSecret, time.Minute); !errors.Is(err, ErrMediaTokenInvalid) {
		t.Errorf("empty videoID must fail, got %v", err)
	}
	if _, _, err := NewMediaToken(videoID, "", time.Minute); !errors.Is(err, ErrMediaTokenInvalid) {
		t.Errorf("empty secret must fail, got %v", err)
	}
	if _, _, err := NewMediaToken(videoID, testSecret, 0); !errors.Is(err, ErrMediaTokenInvalid) {
		t.Errorf("zero ttl must fail, got %v", err)
	}
}
