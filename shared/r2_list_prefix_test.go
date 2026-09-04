package shared

import (
	"strings"
	"testing"
)

// ListObjectsByPrefix guard tests (issue #44). The live
// pagination path runs in the reconcile smoke against real
// R2; here we pin the input validation contract so a
// malformed prefix fails loudly before any SDK call.
func TestListObjectsByPrefix_Guards(t *testing.T) {
	c := &R2Client{client: nil, bucket: "irrelevant"}

	cases := []struct {
		name   string
		prefix string
		wantIn string
	}{
		{"empty prefix", "", "empty"},
		{"no trailing slash", "uploads/user-1", "must end with '/'"},
		{"slash-only prefix", "/", "too broad"},
		{"double-slash prefix", "//", "too broad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListObjectsByPrefix(t.Context(), tc.prefix, 1)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error should mention %q, got %q", tc.wantIn, err.Error())
			}
		})
	}
}
