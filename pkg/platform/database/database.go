// Package database owns the Postgres connection pool and the tenant-isolation rules around it.
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const discardTimeout = 5 * time.Second

// SQLSTATE codes the application reacts to.
const (
	codeUniqueViolation    = "23505"
	codeExclusionViolation = "23P01"
	codeInsufficientPriv   = "42501" // also what a failed RLS WITH CHECK reports
)

// Option adjusts the pool configuration before the pool is created.
type Option func(*pgxpool.Config)

// WithMaxConns caps the pool size. Tests use 1 to force two tenants onto one connection.
func WithMaxConns(n int32) Option {
	return func(c *pgxpool.Config) { c.MaxConns = n }
}

// Open builds a pool and checks that the database answers.
//
// Two settings exist only as defence in depth against one tenant's session state reaching
// another tenant's request (DL-56). WithTenant already scopes its setting to a transaction,
// but a pooled connection must never be able to carry anything over, so every release runs
// DISCARD ALL (and drops the connection if that fails). Statements go through
// QueryExecModeExec, which uses no cached prepared statements, because DISCARD ALL
// deallocates them and a cache would then point at statements that no longer exist.
func Open(ctx context.Context, url string, opts ...Option) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx includes the URL in some errors, and it holds a password: do not wrap it.
		return nil, errors.New("database: invalid connection URL")
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.AfterRelease = func(conn *pgx.Conn) bool {
		ctx, cancel := context.WithTimeout(context.Background(), discardTimeout)
		defer cancel()
		_, err := conn.Exec(ctx, "DISCARD ALL")
		return err == nil
	}
	for _, o := range opts {
		o(cfg)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}
	return pool, nil
}

// IsUniqueViolation reports a unique constraint or index violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool { return hasCode(err, codeUniqueViolation) }

// IsExclusionViolation reports an exclusion constraint violation (SQLSTATE 23P01).
func IsExclusionViolation(err error) bool { return hasCode(err, codeExclusionViolation) }

// IsRLSViolation reports a row-level security rejection (SQLSTATE 42501).
func IsRLSViolation(err error) bool { return hasCode(err, codeInsufficientPriv) }

// ConstraintName returns the violated constraint or index of a Postgres error, or "".
func ConstraintName(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.ConstraintName
	}
	return ""
}

func hasCode(err error, code string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == code
}
