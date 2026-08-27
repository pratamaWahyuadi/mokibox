package shared

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewDB builds a *pgxpool.Pool against the given database URL.
// maxConns controls the upper bound on connections held open
// by this service. The API gateway sets it to 10, the
// transcoder worker to 5 (see APIPoolMaxConns and
// WorkerPoolMaxConns). The pool is created eagerly and
// verified with a Ping before being returned, so a bad URL
// or missing role is surfaced immediately at startup rather
// than on the first request.
//
// Callers MUST call pool.Close() on shutdown. The pool is
// returned as a concrete struct (not an interface) on
// purpose: pgxpool.Pool is the contract every other layer
// (sqlc, drivers, instrumentation) already speaks, and
// wrapping it in an interface here would just force every
// downstream consumer to define its own interface anyway.
func NewDB(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("NewDB: databaseURL is empty")
	}
	if maxConns <= 0 {
		return nil, fmt.Errorf("NewDB: maxConns must be > 0, got %d", maxConns)
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	// Conservative defaults for a single-VPS deployment.
	// MinConns=0 so a quiet worker doesn't keep an idle
	// connection; the API gateway may want a small MinConns
	// to avoid the first-request handshake cost, but a
	// unified conservative default keeps the code simple.
	cfg.MinConns = 0
	// Per-conn lifetime caps so a long-lived process does
	// not accumulate dead connections across Postgres
	// restarts or NAT timeouts.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pgxpool: %w", err)
	}

	// Verify the pool is actually usable before returning
	// it. A 5s deadline keeps a misconfigured deployment
	// from hanging the caller indefinitely.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
