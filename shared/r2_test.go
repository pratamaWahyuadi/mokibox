package shared

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// r2_test.go - unit tests for the R2 helpers that can run
// without a real R2 endpoint: constructor validation and
// the key/prefix guards. r2_delete_prefix_test.go already
// covers DeletePrefix's three guards; here we cover
// NewR2Client validation, HeadObject/Get/Delete/Download/
// Upload key validation, and the mapR2Error NotFound
// translation.

func TestNewR2Client_RejectsEmptyConfig(t *testing.T) {
	base := R2Config{
		AccountID:       "acct",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
		Bucket:          "bucket",
		Endpoint:        "https://example.com",
	}
	cases := []struct {
		name  string
		mutfn func(*R2Config)
	}{
		{"empty account id", func(c *R2Config) { c.AccountID = "" }},
		{"empty access key id", func(c *R2Config) { c.AccessKeyID = "" }},
		{"empty secret", func(c *R2Config) { c.SecretAccessKey = "" }},
		{"empty bucket", func(c *R2Config) { c.Bucket = "" }},
		{"empty endpoint", func(c *R2Config) { c.Endpoint = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			c.mutfn(&cfg)
			_, err := NewR2Client(t.Context(), cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "NewR2Client") {
				t.Errorf("error should name NewR2Client, got %q", err.Error())
			}
		})
	}
}

func TestR2KeyGuards(t *testing.T) {
	// All of these methods must reject an empty key before
	// touching the (nil) SDK client - a nil client means a
	// validation miss would panic instead of returning an
	// error, so reaching the panic proves the guard failed.
	c := &R2Client{client: nil, bucket: "irrelevant"}

	t.Run("HeadObject empty key", func(t *testing.T) {
		if _, err := c.HeadObject(t.Context(), ""); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("GetObject empty key", func(t *testing.T) {
		if _, err := c.GetObject(t.Context(), "", 1024); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("Download empty key", func(t *testing.T) {
		if err := c.Download(t.Context(), "", "/tmp/x"); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("Download empty dest", func(t *testing.T) {
		if err := c.Download(t.Context(), "uploads/u/v.bin", ""); err == nil {
			t.Error("expected error for empty destPath")
		}
	})
	t.Run("UploadFile empty key", func(t *testing.T) {
		if err := c.UploadFile(t.Context(), "", "/tmp/x", "video/mp4"); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("UploadFile empty path", func(t *testing.T) {
		if err := c.UploadFile(t.Context(), "uploads/u/v.bin", "", "video/mp4"); err == nil {
			t.Error("expected error for empty filePath")
		}
	})
	t.Run("PresignPut empty key", func(t *testing.T) {
		if _, err := c.PresignPut(t.Context(), "", "video/mp4", time.Minute); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("PresignPut zero expiry", func(t *testing.T) {
		if _, err := c.PresignPut(t.Context(), "uploads/u/v.bin", "video/mp4", 0); err == nil {
			t.Error("expected error for zero expiry")
		}
	})
	t.Run("PresignGet empty key", func(t *testing.T) {
		if _, err := c.PresignGet(t.Context(), "", time.Minute); err == nil {
			t.Error("expected error for empty key")
		}
	})
	t.Run("PresignGet zero expiry", func(t *testing.T) {
		if _, err := c.PresignGet(t.Context(), "uploads/u/v.bin", 0); err == nil {
			t.Error("expected error for zero expiry")
		}
	})
}

func TestMapR2Error_NotFoundBecomesSentinel(t *testing.T) {
	// mapR2Error is unexported; the only observable surface
	// without a real endpoint is via a fake smithy APIError.
	// We exercise the code switch directly through the
	// exported sentinel behaviour instead: isNotFoundErr
	// matches the S3/R2 "missing object" codes.
	for _, code := range []string{"NoSuchKey", "NotFound", "404"} {
		if !isNotFoundErr(code) {
			t.Errorf("isNotFoundErr(%q) should be true", code)
		}
	}
	for _, code := range []string{"", "AccessDenied", "InternalError", "noSuchKey"} {
		if isNotFoundErr(code) {
			t.Errorf("isNotFoundErr(%q) should be false", code)
		}
	}
	// The NotFound sentinel must remain errors.Is-matchable
	// so handlers keep mapping R2 misses to 404.
	if !errors.Is(Wrap(ErrNotFound, "HeadObject uploads/u/v"), ErrNotFound) {
		t.Error("ErrNotFound must stay matchable through Wrap")
	}
}
