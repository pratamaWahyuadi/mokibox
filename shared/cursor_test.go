package shared

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEncodeCursor_EmptyInputs(t *testing.T) {
	if got := EncodeCursor(time.Time{}, uuid.New()); got != "" {
		t.Errorf("EncodeCursor(zero time) = %q, want empty", got)
	}
	if got := EncodeCursor(time.Now(), uuid.Nil); got != "" {
		t.Errorf("EncodeCursor(nil uuid) = %q, want empty", got)
	}
}

func TestCursorRoundtrip(t *testing.T) {
	created := time.Date(2026, 9, 3, 12, 34, 56, 123456789, time.UTC)
	id := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")

	cur := EncodeCursor(created, id)
	if cur == "" {
		t.Fatal("EncodeCursor returned empty for valid inputs")
	}

	gotTime, gotID, err := DecodeCursor(cur)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if gotID != id {
		t.Errorf("id roundtrip: got %v, want %v", gotID, id)
	}
	// RFC3339Nano preserves nanosecond precision.
	if !gotTime.Equal(created) {
		t.Errorf("time roundtrip: got %v, want %v", gotTime, created)
	}
}

func TestDecodeCursor_EmptyIsFirstPage(t *testing.T) {
	ts, id, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("empty cursor must be first-page, got error %v", err)
	}
	if !ts.IsZero() || id != uuid.Nil {
		t.Errorf("empty cursor should yield zero values, got ts=%v id=%v", ts, id)
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	enc := base64.RawURLEncoding
	cases := []struct {
		name   string
		cursor string
	}{
		{"not base64url", "!!!not-base64!!!"},
		{"valid base64, no separator", enc.EncodeToString([]byte("noseparator"))},
		{"valid base64, bad timestamp", enc.EncodeToString([]byte("not-a-time|123e4567-e89b-12d3-a456-426614174000"))},
		{"valid base64, bad uuid", enc.EncodeToString([]byte("2026-09-03T12:34:56Z|not-a-uuid"))},
		{"empty timestamp part", enc.EncodeToString([]byte("|123e4567-e89b-12d3-a456-426614174000"))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := DecodeCursor(c.cursor)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrInvalidCursor) {
				t.Errorf("expected ErrInvalidCursor, got %v", err)
			}
		})
	}
}
