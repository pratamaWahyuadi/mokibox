// r2_delete_prefix_test.go - tests for the DeletePrefix
// bucket-wipe guards. The guards live in shared/r2.go
// but the assertions are intentionally restricted to
// the constructor-side checks (empty / no-trailing-slash
// / slash-only) so we do not need an R2 endpoint to run
// them.
//
// ListObjectsV2 + DeleteObjects pagination over a real
// bucket is exercised in the runtime smoke (issue D).
package shared

import (
	"context"
	"strings"
	"testing"
)

func TestDeletePrefix_RejectsEmptyPrefix(t *testing.T) {
	// We never reach a real R2 client - the constructor
	// validation throws before the SDK is touched.
	c := &R2Client{client: nil, bucket: "irrelevant"}
	err := c.DeletePrefix(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty prefix, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should mention empty prefix, got %q", err.Error())
	}
}

func TestDeletePrefix_RejectsNoTrailingSlash(t *testing.T) {
	c := &R2Client{client: nil, bucket: "irrelevant"}
	err := c.DeletePrefix(context.Background(), "hls/user/vid")
	if err == nil {
		t.Fatal("expected error for prefix without trailing slash, got nil")
	}
	if !strings.Contains(err.Error(), "must end with '/'") {
		t.Errorf("error should mention trailing slash requirement, got %q", err.Error())
	}
}

func TestDeletePrefix_RejectsBucketRootSlash(t *testing.T) {
	cases := []string{
		"/",       // single slash - would match every key
		"//",      // double slash
		"///",     // triple slash
		"/////",   // many slashes
	}
	for _, p := range cases {
		err := (&R2Client{client: nil, bucket: "irrelevant"}).DeletePrefix(context.Background(), p)
		if err == nil {
			t.Errorf("prefix %q must be rejected (would match bucket root)", p)
			continue
		}
		if !strings.Contains(err.Error(), "too broad") {
			t.Errorf("prefix %q: error should mention 'too broad', got %q", p, err.Error())
		}
	}
}