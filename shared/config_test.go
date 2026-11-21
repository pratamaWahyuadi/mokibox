package shared

import (
	"testing"
	"time"
)

func setRequiredAPIEnv(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"API_BASE_URL":               "https://api.example.test",
		"DATABASE_URL":               "postgres://db",
		"REDIS_ADDR":                 "redis:6379",
		"REDIS_PASSWORD":             "password",
		"ZITADEL_ISSUER_URL":         "https://auth.example.test",
		"ZITADEL_CLIENT_ID":          "web-client",
		"ZITADEL_API_CLIENT_ID":      "api-client",
		"ZITADEL_TARGET_SIGNING_KEY": "signing-key",
		"R2_ACCOUNT_ID":              "account",
		"R2_ACCESS_KEY_ID":           "access",
		"R2_SECRET_ACCESS_KEY":       "secret",
		"R2_BUCKET":                  "bucket",
		"R2_ENDPOINT":                "https://r2.example.test",
		"MEDIA_TOKEN_SECRET":         "media-secret",
	} {
		t.Setenv(name, value)
	}
	t.Setenv("DM_E2EE_ENABLED", "")
	t.Setenv("DM_INBOX_TTL", "")
}

func TestLoadAPIDMFeatureFlagDefaultsOff(t *testing.T) {
	setRequiredAPIEnv(t)
	cfg, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DMEnabled {
		t.Fatal("DM relay enabled without an explicit opt-in")
	}
	if cfg.DMInboxTTL != 0 {
		t.Fatalf("DM inbox TTL = %s, want unset while disabled", cfg.DMInboxTTL)
	}
}

func TestLoadAPIRequiresInboxTTLWhenDMEnabled(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("DM_E2EE_ENABLED", "true")
	if _, err := LoadAPI(); err == nil {
		t.Fatal("enabled DM relay accepted missing TTL")
	}
}

func TestLoadAPIParsesDMInboxTTLWhenEnabled(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("DM_E2EE_ENABLED", "true")
	t.Setenv("DM_INBOX_TTL", "168h")
	cfg, err := LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DMEnabled || cfg.DMInboxTTL != 7*24*time.Hour {
		t.Fatalf("DM config = enabled:%t ttl:%s", cfg.DMEnabled, cfg.DMInboxTTL)
	}
}

func TestLoadAPIRejectsInvalidDMConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name    string
		enabled string
		ttl     string
	}{
		{name: "invalid flag", enabled: "yes", ttl: "168h"},
		{name: "invalid ttl", enabled: "true", ttl: "seven days"},
		{name: "non-positive ttl", enabled: "true", ttl: "0s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredAPIEnv(t)
			t.Setenv("DM_E2EE_ENABLED", tt.enabled)
			t.Setenv("DM_INBOX_TTL", tt.ttl)
			if _, err := LoadAPI(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
