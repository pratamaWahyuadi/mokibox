package shared

import (
	"testing"
	"time"
)

func setRequiredAPIEnv(t *testing.T) {
	t.Helper()
	values := map[string]string{
		"API_BASE_URL":               "https://api.example.test",
		"DATABASE_URL":               "postgres://test",
		"REDIS_ADDR":                 "localhost:6379",
		"REDIS_PASSWORD":             "test",
		"ZITADEL_ISSUER_URL":         "https://auth.example.test",
		"ZITADEL_CLIENT_ID":          "web",
		"ZITADEL_API_CLIENT_ID":      "api",
		"ZITADEL_TARGET_SIGNING_KEY": "test",
		"R2_ACCOUNT_ID":              "test",
		"R2_ACCESS_KEY_ID":           "test",
		"R2_SECRET_ACCESS_KEY":       "test",
		"R2_BUCKET":                  "test",
		"R2_ENDPOINT":                "https://r2.example.test",
		"MEDIA_TOKEN_SECRET":         "test",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func TestLoadAPI_DME2EEIsOffByDefaultWithFixedRetention(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("DM_E2EE_ENABLED", "")
	t.Setenv("DM_INBOX_TTL", "")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if cfg.DME2EEEnabled {
		t.Fatal("DM E2EE must default off")
	}
	if cfg.DMInboxTTL != 168*time.Hour {
		t.Fatalf("DM inbox TTL = %s, want 168h", cfg.DMInboxTTL)
	}
}

func TestLoadAPI_DME2EERequires168HourTTL(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("DM_E2EE_ENABLED", "true")
	t.Setenv("DM_INBOX_TTL", "24h")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("LoadAPI() must reject non-168h DM TTL when enabled")
	}
}

func TestLoadAPI_DME2EERejectsInvalidFlag(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("DM_E2EE_ENABLED", "sometimes")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("LoadAPI() must reject invalid DM_E2EE_ENABLED")
	}
}
