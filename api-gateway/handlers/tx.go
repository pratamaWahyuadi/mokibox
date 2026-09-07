// Package handlers - tx.go defines the transaction runner
// pattern used by every handler that mutates denormalised
// state inside a single *sql.Tx (likes, comments, account
// deletion, upload confirm).
//
// The concrete *sql.Tx a *sql.DB yields cannot be stubbed in
// a unit test, so the runner is an interface the handler
// holds (txRunner[T]) and the production implementation
// (sqlTxRunner) is wired by the handler constructor from the
// real pool. Tests pass a stub runner that invokes the
// callback with a stub tx-store, which keeps the handler
// body - the actual business logic - fully unit-testable.
// The begin/bind/commit/rollback mechanics themselves stay
// thin (no branching beyond error wrapping) and are
// exercised live by the integration suite
// (scripts/integration_test.sh) against the real Postgres.
package handlers

import (
	"context"
	"database/sql"

	"github.com/pratamaWahyuadi/mokibox/shared"
)

// txRunner runs fn exactly once inside a transaction, handing
// it a store bound to that transaction. A non-nil return from
// fn aborts (rolls back) the transaction and the runner
// returns the error unchanged, so classification stays in the
// handler body where the sentinel context lives. Holding the
// runner behind a one-method interface is what makes the tx
// path stubbable in unit tests without a mock library.
type txRunner[T any] interface {
	Run(ctx context.Context, fn func(q T) error) error
}

// sqlTxRunner is the production txRunner: it begins a tx on
// the shared *sql.DB pool, binds the sqlc querier via
// Queries.WithTx, runs fn, and commits.
//
// The bind callback converts the sqlc-bound *db.Queries into
// the consumer-side interface T. Because T is a named
// interface type declared next to the handler (e.g.
// socialTxStore), the conversion inside the callback is
// verified at compile time - a typo'd or missing method fails
// the build rather than panicking at runtime. This avoids the
// fragile `WithTx(tx).(T)` type-assertion-to-type-parameter
// pattern entirely.
type sqlTxRunner[T any] struct {
	db   *sql.DB
	bind func(tx *sql.Tx) T
}

// NewSQLTxRunner builds the production runner from the pool
// and a bind function, e.g.
//
//	NewSQLTxRunner(dbHandle, func(tx *sql.Tx) socialTxStore {
//	    return queries.WithTx(tx)
//	})
//
// It is the constructor's job to pair the runner with the
// same *db.Queries the handler reads through, so reads and
// tx writes hit the single shared pool.
func NewSQLTxRunner[T any](dbHandle *sql.DB, bind func(tx *sql.Tx) T) txRunner[T] {
	return sqlTxRunner[T]{db: dbHandle, bind: bind}
}

// Run implements txRunner. Errors returned by fn are passed
// through verbatim - the handler body already wrapped them
// with the right sentinel - while begin/commit failures
// collapse to ErrInternal. The deferred rollback is a no-op
// after a successful commit.
func (r sqlTxRunner[T]) Run(ctx context.Context, fn func(q T) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return shared.Wrap(shared.ErrInternal, "begin transaction")
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(r.bind(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return shared.Wrap(shared.ErrInternal, "commit transaction")
	}
	return nil
}
