// Package store contains the PostgreSQL data access layer on pgx/v5.
// Queries are hand-written SQL with explicit row scans — the sync core runs
// inside single transactions whose semantics (ledger + version + change log
// atomicity) are the point of the module, and keeping the SQL visible beats
// indirection for a schema this small. sqlc-style generated code was
// evaluated and deliberately not used (see README §Design choices).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// DB is a thin wrapper so callers depend on an interface-shaped seam while
// keeping pgx transactions first-class.
type DB struct {
	Pool *pgxpool.Pool
}

func NewDB(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() { db.Pool.Close() }

// Tx runs fn inside a serializable-friendly transaction (read committed is
// sufficient: every sync write is keyed on deterministic UUIDs and the
// ledger unique constraint arbitrates retries).
func (db *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// Q is the minimal query interface implemented by both *pgxpool.Pool
// (top-level) and pgx.Tx (inside transactions).
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (commandTag, error)
	Query(ctx context.Context, sql string, args ...any) (rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) row
}

type commandTag interface{ RowsAffected() int64 }
type rows interface {
	Next() bool
	Close()
	Err() error
	Scan(dest ...any) error
}
type row interface{ Scan(dest ...any) error }

// Both *pgxpool.Pool and pgx.Tx satisfy Q via these adapters.
var _ Q = (*poolQ)(nil)

type poolQ struct{ p *pgxpool.Pool }

func (q poolQ) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	return q.p.Exec(ctx, sql, args...)
}
func (q poolQ) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	return q.p.Query(ctx, sql, args...)
}
func (q poolQ) QueryRow(ctx context.Context, sql string, args ...any) row {
	return q.p.QueryRow(ctx, sql, args...)
}

func (db *DB) Q() Q { return poolQ{db.Pool} }

type txQ struct{ t pgx.Tx }

func (q txQ) Exec(ctx context.Context, sql string, args ...any) (commandTag, error) {
	return q.t.Exec(ctx, sql, args...)
}
func (q txQ) Query(ctx context.Context, sql string, args ...any) (rows, error) {
	return q.t.Query(ctx, sql, args...)
}
func (q txQ) QueryRow(ctx context.Context, sql string, args ...any) row {
	return q.t.QueryRow(ctx, sql, args...)
}

func TxQ(t pgx.Tx) Q { return txQ{t} }
