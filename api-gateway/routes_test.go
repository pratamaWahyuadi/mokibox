package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/labstack/echo/v4"

	"github.com/pratamaWahyuadi/mokibox/shared"
	"github.com/pratamaWahyuadi/mokibox/shared/db"
)

type routeTestVerifier struct{}

func (routeTestVerifier) CheckToken(context.Context, string) (string, error) {
	return "route-test-sub", nil
}

func newRouteTestRouter(t *testing.T, dmEnabled bool) *echo.Echo {
	t.Helper()
	r2, err := shared.NewR2Client(context.Background(), shared.R2Config{
		AccountID:       "test-account",
		AccessKeyID:     "test-access",
		SecretAccessKey: "test-secret",
		Bucket:          "test-bucket",
		Endpoint:        "https://example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	queue := asynq.NewClient(asynq.RedisClientOpt{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { _ = queue.Close() })
	sqlDB := &sql.DB{}
	cfg := &shared.APIConfig{
		MediaTokenSecret:    "route-test-secret",
		MediaTokenTTL:       time.Hour,
		PresignUploadExpiry: time.Minute,
		DME2EEEnabled:       dmEnabled,
		DMInboxTTL:          168 * time.Hour,
	}
	return NewRouter(RouterDeps{
		Queries:           db.New(sqlDB),
		DB:                sqlDB,
		R2:                r2,
		Queue:             queue,
		Cfg:               cfg,
		AuthVerifier:      routeTestVerifier{},
		WebhookSigningKey: "route-test-signing-key",
	})
}

func TestDMRoutesFollowFeatureFlag(t *testing.T) {
	wantRoutes := []struct {
		method string
		path   string
	}{
		{"POST", "/api/dm/device"},
		{"POST", "/api/dm/device/one-time-keys"},
		{"GET", "/api/dm/users/:id/key-bundle"},
		{"POST", "/api/dm/messages"},
		{"GET", "/api/dm/inbox"},
		{"POST", "/api/dm/inbox/:message_id/ack"},
	}
	for _, enabled := range []bool{false, true} {
		router := newRouteTestRouter(t, enabled)
		routes := make(map[string]bool)
		for _, route := range router.Routes() {
			routes[route.Method+" "+route.Path] = true
			if strings.HasPrefix(route.Path, "/api") && strings.Contains(route.Path, "*") &&
				route.Method != echo.RouteNotFound {
				t.Fatalf("unexpected catch-all API route %s %s", route.Method, route.Path)
			}
		}
		for _, want := range wantRoutes {
			key := want.method + " " + want.path
			if routes[key] != enabled {
				t.Fatalf("enabled=%v route %s present=%v", enabled, key, routes[key])
			}
		}
	}
}

func TestUnknownAPIRouteSkipsAuthAndRateLimit(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		router := newRouteTestRouter(t, enabled)
		for _, path := range []string{"/api/does-not-exist", "/api/dm/does-not-exist"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("enabled=%v unknown route %s returned %d: %s", enabled, path, rec.Code, rec.Body.String())
			}
		}
	}
}
