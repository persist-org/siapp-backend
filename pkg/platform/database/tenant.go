package database

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
)

// WithTenant runs fn in a transaction scoped to one organization. It is the ONLY way to
// read or write tenant data: every tenant table's RLS policy compares organization_id with
// the app.current_org setting, and this function is what sets it.
//
// The setting is transaction-local (set_config's third argument is true). It disappears at
// COMMIT or ROLLBACK, so a pooled connection can never serve the next request with the
// previous tenant's id, even if a release hook were skipped.
//
// The transaction commits when fn returns nil and rolls back when fn returns an error or
// panics; a panic is re-raised after the rollback.
func WithTenant(ctx context.Context, pool *pgxpool.Pool, orgID int64, fn func(q *db.Queries) error) error {
	return WithTenantTx(ctx, pool, orgID, func(tx pgx.Tx) error { return fn(db.New(tx)) })
}

// WithTenantTx is WithTenant for code that needs the transaction itself, for example
// :copyfrom batch inserts or hand-written SQL. It has the same guarantees.
func WithTenantTx(ctx context.Context, pool *pgxpool.Pool, orgID int64, fn func(tx pgx.Tx) error) (err error) {
	if orgID <= 0 {
		return fmt.Errorf("database: invalid organization id %d", orgID)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer finish(ctx, tx, &err)

	// Parameterised, so the id is never spliced into SQL text.
	if _, err = tx.Exec(ctx, "SELECT set_config('app.current_org', $1, true)", strconv.FormatInt(orgID, 10)); err != nil {
		return fmt.Errorf("database: set tenant: %w", err)
	}
	return fn(tx)
}

// WithoutTenant runs fn in a plain transaction with no tenant set. Use it ONLY for the
// organizations registry, the single table that is read outside a tenant (it has no RLS).
// A tenant table touched through it returns no rows and rejects every write, so a
// misuse fails closed, but it is still a bug: use WithTenant.
func WithoutTenant(ctx context.Context, pool *pgxpool.Pool, fn func(q *db.Queries) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer finish(ctx, tx, &err)
	return fn(db.New(tx))
}

// finish commits or rolls back the transaction according to *errp, and on panic rolls back
// and re-panics. The rollback uses a context that outlives a cancelled request context,
// so a cancelled request still releases its locks promptly.
func finish(ctx context.Context, tx pgx.Tx, errp *error) {
	if r := recover(); r != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		panic(r)
	}
	if *errp != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		*errp = fmt.Errorf("database: commit: %w", err)
	}
}
