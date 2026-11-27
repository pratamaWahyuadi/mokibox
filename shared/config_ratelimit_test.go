// config_ratelimit_test.go - Fitur A (K5): the search /
// feed limiter env parsing must be lenient: unset or
// invalid values fall back to the default without an
// error, because a bad limiter value must never prevent
// the api-gateway from starting.
package shared

import "testing"

func TestEnvIntLenient(t *testing.T) {
	t.Setenv("TEST_RATE_LIMIT", "")
	if got := envIntLenient("TEST_RATE_LIMIT", 60); got != 60 {
		t.Fatalf("unset env must yield default, got %d", got)
	}

	t.Setenv("TEST_RATE_LIMIT", "120")
	if got := envIntLenient("TEST_RATE_LIMIT", 60); got != 120 {
		t.Fatalf("valid env must be honoured, got %d", got)
	}

	for _, bad := range []string{"abc", "0", "-5", "1.5"} {
		t.Setenv("TEST_RATE_LIMIT", bad)
		if got := envIntLenient("TEST_RATE_LIMIT", 60); got != 60 {
			t.Fatalf("invalid env %q must yield default, got %d", bad, got)
		}
	}
}
