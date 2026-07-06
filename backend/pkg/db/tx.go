package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// txStarter is the minimal capability (*DB).InTx needs to begin a transaction.
// Both *pgxpool.Pool and pgxmock's pool mock satisfy it, which keeps InTx
// unit-testable without a live database (see db_test.go).
type txStarter interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// InTx runs fn inside a database transaction managed by the pool.
//
// Behavior:
//   - If fn returns nil, the transaction is committed; a commit error is
//     returned wrapped.
//   - If fn returns a non-nil error, the transaction is rolled back and the
//     original error is returned unchanged so callers can errors.Is/As it.
//   - If fn panics, the panic is recovered, the transaction is rolled back,
//     and the panic value is returned as an error (InTx never re-panics).
//
// opts may be the zero value to use the server default isolation level
// (ReadCommitted on a stock PostgreSQL) with read/write access.
func (db *DB) InTx(ctx context.Context, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	return execInTx(ctx, db.pool, opts, fn)
}

// execInTx is the transaction engine behind InTx. It is parameterised over a
// txStarter (rather than *pgxpool.Pool directly) so it can be exercised with a
// pgxmock in tests; the commit/rollback/panic semantics here ARE InTx's
// semantics — InTx is a one-line delegator onto this function.
func execInTx(ctx context.Context, starter txStarter, opts pgx.TxOptions, fn func(tx pgx.Tx) error) (err error) {
	tx, beginErr := starter.BeginTx(ctx, opts)
	if beginErr != nil {
		return fmt.Errorf("db: begin transaction: %w", beginErr)
	}

	defer func() {
		if r := recover(); r != nil {
			// fn panicked: roll back and surface the panic value as an error
			// so callers get a deterministic error instead of a propagating panic.
			_ = tx.Rollback(ctx)
			err = fmt.Errorf("db: panic in transaction: %v", r)
			return
		}
		if err != nil {
			// fn returned an error: roll back and preserve the original error
			// unchanged so errors.Is/As keep working.
			_ = tx.Rollback(ctx)
			return
		}
		// fn succeeded: commit.
		if commitErr := tx.Commit(ctx); commitErr != nil {
			err = fmt.Errorf("db: commit transaction: %w", commitErr)
		}
	}()

	err = fn(tx)
	return err
}
