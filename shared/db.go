package shared

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql "pgx" driver (pgx stdlib adapter)
)

// NewSQLDB opens a *sql.DB pool against the given DSN
// using the pgx stdlib adapter (registered via the
// blank import above).
//
// Why *sql.DB via pgx stdlib (not *pgxpool.Pool):
//
// sqlc (engine "postgresql") generates Queries.WithTx
// with a *sql.Tx signature. *sql.Tx can only be
// produced by *sql.DB.BeginTx - *pgxpool.Pool returns
// pgx.Tx which is NOT a *sql.Tx. So sqlc forces us to
// keep at least one *sql.DB around.
//
// Trade-off (intentional, do not "optimize" back to
// pgxpool without re-reading this comment):
//
//   - We lose pgxpool's granular acquire/release
//     semantics and its named prepared-statement
//     cache.
//   - pgx stdlib adapter still uses pgx type mapping
//     and a per-connection prepared-statement cache,
//     so we keep the pgx wire-level benefits.
//   - For the MokiBox workload (max 10 conn API +
//     max 5 conn worker, single VPS), the difference
//     is negligible.
//
// If a future phase needs pgxpool-only ergonomics,
// rewrite shared/db/*.sql.go (~1900 LOC of generated
// code) to use pgx.Tx instead of *sql.Tx. Do NOT
// silently re-introduce a second pool.
//
// maxConns caps *sql.DB.SetMaxOpenConns; maxIdle caps
// SetMaxIdleConns. The pool is verified with a Ping
// (5s deadline) before returning so a misconfigured
// DSN is surfaced at startup, not on the first
// request.
//
// Callers MUST call (*sql.DB).Close() on shutdown.
func NewSQLDB(ctx context.Context, dsn string, maxConns, maxIdle int32) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("NewSQLDB: dsn is empty")
	}
	if maxConns <= 0 {
		return nil, fmt.Errorf("NewSQLDB: maxConns must be > 0, got %d", maxConns)
	}
	if maxIdle < 0 {
		return nil, fmt.Errorf("NewSQLDB: maxIdle must be >= 0, got %d", maxIdle)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(int(maxConns))
	db.SetMaxIdleConns(int(maxIdle))
	// Conservative lifetime cap: a long-lived process
	// must not accumulate dead connections across
	// Postgres restarts or NAT timeouts.
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}