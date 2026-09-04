// Package shared contains code used by both api-gateway and
// transcoder-worker. Keep package-level state to a minimum;
// everything is passed via constructor injection.
package shared

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// PoolSize is the maximum number of pgxpool connections.
// Per PRD/NFR the API has more concurrent traffic than the
// worker, so the limits differ between the two services.
const (
	APIPoolMaxConns    = 10
	WorkerPoolMaxConns = 5
)

// APIConfig is the configuration consumed by api-gateway.
// The api-gateway uses DATABASE_URL (role tiktok_api, full
// CRUD) and exposes the public API to clients.
type APIConfig struct {
	// HTTP
	APIBaseURL string // e.g. https://api.example.com

	// Database (role tiktok_api)
	DatabaseURL string

	// Redis (for Asynq producer)
	RedisAddr     string
	RedisPassword string

	// Zitadel (OIDC resource server)
	ZitadelIssuerURL    string
	ZitadelClientID     string // Web/OIDC client (used by login UI)
	ZitadelAPIClientID  string // API client (audience for access tokens)
	// ZitadelTargetSigningKey is the signingKey returned
	// by Zitadel when an Actions V2 Target was created.
	// The api-gateway webhook uses it to verify the
	// ZITADEL-Signature HMAC on every incoming call. It
	// is separate from the OIDC JWKS keys: the signingKey
	// is a shared secret returned exactly once at Target
	// creation time, so it lives in env, not in the
	// OIDC discovery document.
	ZitadelTargetSigningKey string

	// Cloudflare R2
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2Endpoint        string

	// Media token (HMAC for HLS playlist URLs)
	MediaTokenSecret string
	MediaTokenTTL    time.Duration

	// Upload intent (presigned PUT expiry)
	PresignUploadExpiry time.Duration
}

// WorkerConfig is the configuration consumed by transcoder-worker.
// The worker uses WORKER_DATABASE_URL (role tiktok_worker,
// restricted to videos) and consumes jobs from Redis.
type WorkerConfig struct {
	// Database (role tiktok_worker, restricted)
	WorkerDatabaseURL string

	// Redis (Asynq consumer + producer)
	RedisAddr     string
	RedisPassword string

	// Cloudflare R2
	R2AccountID       string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2Endpoint        string

	// Hard timeout per transcode job (worker kills the
	// process if it exceeds this).
	TranscodeTimeout time.Duration

	// Reconcile config (issue #44, phase-10). The worker
	// runs an R2 orphan reconciliation sweep on this
	// cadence, examining up to ReconcileBatch tombstoned
	// users per tick. DryRun logs candidates without
	// enqueueing cleanup.
	ReconcileInterval time.Duration
	ReconcileBatch    int
	ReconcileDryRun   bool
}

// LoadAPI reads the environment variables required by
// api-gateway from the process environment. It returns a
// wrapped error naming the missing variable when something
// required is absent; never panics.
func LoadAPI() (*APIConfig, error) {
	c := &APIConfig{
		APIBaseURL:          os.Getenv("API_BASE_URL"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		RedisAddr:           os.Getenv("REDIS_ADDR"),
		RedisPassword:       os.Getenv("REDIS_PASSWORD"),
		ZitadelIssuerURL:    os.Getenv("ZITADEL_ISSUER_URL"),
		ZitadelClientID:     os.Getenv("ZITADEL_CLIENT_ID"),
		ZitadelAPIClientID:  os.Getenv("ZITADEL_API_CLIENT_ID"),
		ZitadelTargetSigningKey: os.Getenv("ZITADEL_TARGET_SIGNING_KEY"),
		R2AccountID:         os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:       os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey:   os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:            os.Getenv("R2_BUCKET"),
		R2Endpoint:          os.Getenv("R2_ENDPOINT"),
		MediaTokenSecret:    os.Getenv("MEDIA_TOKEN_SECRET"),
	}

	ttl, err := envDuration("MEDIA_TOKEN_TTL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	c.MediaTokenTTL = ttl

	expiry, err := envDuration("PRESIGN_UPLOAD_EXPIRY", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	c.PresignUploadExpiry = expiry

	if err := requireFields(
		"API_BASE_URL", c.APIBaseURL,
		"DATABASE_URL", c.DatabaseURL,
		"REDIS_ADDR", c.RedisAddr,
		"REDIS_PASSWORD", c.RedisPassword,
		"ZITADEL_ISSUER_URL", c.ZitadelIssuerURL,
		"ZITADEL_CLIENT_ID", c.ZitadelClientID,
		"ZITADEL_API_CLIENT_ID", c.ZitadelAPIClientID,
		"ZITADEL_TARGET_SIGNING_KEY", c.ZitadelTargetSigningKey,
		"R2_ACCOUNT_ID", c.R2AccountID,
		"R2_ACCESS_KEY_ID", c.R2AccessKeyID,
		"R2_SECRET_ACCESS_KEY", c.R2SecretAccessKey,
		"R2_BUCKET", c.R2Bucket,
		"R2_ENDPOINT", c.R2Endpoint,
		"MEDIA_TOKEN_SECRET", c.MediaTokenSecret,
	); err != nil {
		return nil, err
	}

	return c, nil
}

// LoadWorker reads the environment variables required by
// transcoder-worker.
func LoadWorker() (*WorkerConfig, error) {
	c := &WorkerConfig{
		WorkerDatabaseURL:  os.Getenv("WORKER_DATABASE_URL"),
		RedisAddr:          os.Getenv("REDIS_ADDR"),
		RedisPassword:      os.Getenv("REDIS_PASSWORD"),
		R2AccountID:        os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:      os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey:  os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:           os.Getenv("R2_BUCKET"),
		R2Endpoint:         os.Getenv("R2_ENDPOINT"),
	}

	timeout, err := envDuration("TRANSCODE_TIMEOUT", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	c.TranscodeTimeout = timeout

	// Issue #44 reconciliation cadence. Default 1h per the
	// issue's Action criteria; batch 100 users per tick to
	// avoid a thundering herd on a large backlog.
	ri, err := envDuration("RECONCILE_INTERVAL", time.Hour)
	if err != nil {
		return nil, err
	}
	c.ReconcileInterval = ri
	c.ReconcileBatch, err = envInt("RECONCILE_BATCH", 100)
	if err != nil {
		return nil, err
	}
	c.ReconcileDryRun = os.Getenv("RECONCILE_DRY_RUN") == "true"

	if err := requireFields(
		"WORKER_DATABASE_URL", c.WorkerDatabaseURL,
		"REDIS_ADDR", c.RedisAddr,
		"REDIS_PASSWORD", c.RedisPassword,
		"R2_ACCOUNT_ID", c.R2AccountID,
		"R2_ACCESS_KEY_ID", c.R2AccessKeyID,
		"R2_SECRET_ACCESS_KEY", c.R2SecretAccessKey,
		"R2_BUCKET", c.R2Bucket,
		"R2_ENDPOINT", c.R2Endpoint,
	); err != nil {
		return nil, err
	}

	return c, nil
}

// envDuration parses a duration env var, falling back to def
// when unset. A present-but-invalid value is reported as an
// error so a typo does not silently degrade behaviour.
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s=%q: %w", name, v, err)
	}
	return d, nil
}

// envInt parses an int env var, falling back to def when
// unset. Same fail-loud policy as envDuration: a
// present-but-invalid value is an error, and a
// non-positive value is rejected because every current
// caller uses it as a size/budget (never 0 or negative).
func envInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid int for %s=%q: %w", name, v, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid int for %s=%q: must be > 0", name, v)
	}
	return n, nil
}

// requireFields returns the first missing field as a wrapped
// error. Pair-form: name, value, name, value, ... (variadic
// for clarity at call site).
func requireFields(kv ...string) error {
	if len(kv)%2 != 0 {
		return fmt.Errorf("requireFields: odd number of arguments (%d)", len(kv))
	}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			return fmt.Errorf("missing required env var: %s", kv[i])
		}
	}
	return nil
}

// ParseIntFromEnv is a small helper exported for tests and
// future handlers. It returns def when unset and an error
// for malformed values.
func ParseIntFromEnv(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid int for %s=%q: %w", name, v, err)
	}
	return n, nil
}
