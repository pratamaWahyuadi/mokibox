package shared

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testWSSecret = "unit-test-secret-unit-test-secret-32b"

func TestWSTicket_RoundTrip(t *testing.T) {
	uid := uuid.New()
	ticket, exp, err := NewWSTicket(uid, testWSSecret, 30*time.Second)
	if err != nil {
		t.Fatalf("NewWSTicket: %v", err)
	}
	if !exp.After(time.Now()) {
		t.Errorf("expiry %v should be in the future", exp)
	}
	got, err := VerifyWSTicket(ticket, testWSSecret)
	if err != nil {
		t.Fatalf("VerifyWSTicket: %v", err)
	}
	if got != uid {
		t.Errorf("got user %s, want %s", got, uid)
	}
}

func TestWSTicket_Rejections(t *testing.T) {
	uid := uuid.New()
	valid, _, _ := NewWSTicket(uid, testWSSecret, time.Minute)
	expired, _ := SignWSTicket(uid, testWSSecret, time.Now().Add(-time.Minute))

	// Same signature, different user id: must not verify.
	raw, _ := base64.RawURLEncoding.DecodeString(valid)
	parts := strings.SplitN(string(raw), ".", 3)
	swapped := base64.RawURLEncoding.EncodeToString([]byte(parts[0] + "." + uuid.New().String() + "." + parts[2]))

	// A media token signed with the same secret must not pass as a WS ticket.
	media, _ := SignMediaToken(uid.String(), testWSSecret, time.Now().Add(time.Minute))

	cases := map[string]struct{ ticket, secret string }{
		"empty":           {"", testWSSecret},
		"garbage":         {"not-a-ticket", testWSSecret},
		"not base64":      {"!!!", testWSSecret},
		"expired":         {expired, testWSSecret},
		"wrong secret":    {valid, "another-secret-another-secret-32by"},
		"swapped user id": {swapped, testWSSecret},
		"media token":     {media, testWSSecret},
		"empty secret":    {valid, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyWSTicket(tc.ticket, tc.secret); !errors.Is(err, ErrWSTicketInvalid) {
				t.Fatalf("expected ErrWSTicketInvalid, got %v", err)
			}
		})
	}
}

func TestWSTicket_InvalidIssueArgs(t *testing.T) {
	if _, _, err := NewWSTicket(uuid.Nil, testWSSecret, time.Second); !errors.Is(err, ErrWSTicketInvalid) {
		t.Errorf("nil user: got %v", err)
	}
	if _, _, err := NewWSTicket(uuid.New(), "", time.Second); !errors.Is(err, ErrWSTicketInvalid) {
		t.Errorf("empty secret: got %v", err)
	}
	if _, _, err := NewWSTicket(uuid.New(), testWSSecret, 0); !errors.Is(err, ErrWSTicketInvalid) {
		t.Errorf("zero ttl: got %v", err)
	}
}
