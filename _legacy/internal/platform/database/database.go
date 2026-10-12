// Package database opens the MySQL pool and provides transaction, migration and
// error helpers shared by every module.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"

	migrations "siapp/db"
	"siapp/internal/db"
)

const (
	maxOpenConns    = 25
	maxIdleConns    = 25
	connMaxLifetime = 5 * time.Minute
	utcTimeZone     = "'+00:00'"

	mysqlErrDuplicateEntry = 1062
)

// Open validates the DSN, opens a connection pool and verifies it with a ping.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	if err := checkDSN(dsn); err != nil {
		return nil, err
	}
	pool, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	pool.SetMaxOpenConns(maxOpenConns)
	pool.SetMaxIdleConns(maxIdleConns)
	pool.SetConnMaxLifetime(connMaxLifetime)

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return pool, nil
}

// checkDSN refuses DSNs that would make Go and MySQL disagree about time. Without
// parseTime DATETIME columns scan as []byte; without loc=UTC Go reads them in local time;
// without time_zone='+00:00' NOW() uses the server zone. The driver already defaults
// loc to UTC when omitted, so only an explicit non-UTC loc (e.g. loc=Local) is rejected.
func checkDSN(dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parse mysql dsn: %w", err)
	}
	var missing []string
	if !cfg.ParseTime {
		missing = append(missing, "parseTime=true")
	}
	if cfg.Loc != time.UTC {
		missing = append(missing, "loc=UTC")
	}
	if cfg.Params["time_zone"] != utcTimeZone {
		missing = append(missing, "time_zone=%27%2B00%3A00%27")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mysql dsn must include %s", strings.Join(missing, ", "))
	}
	return nil
}

// Migrate applies every pending embedded migration.
func Migrate(ctx context.Context, pool *sql.DB) error {
	fsys, err := fs.Sub(migrations.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectMySQL, pool, fsys)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// WithTx runs fn inside a transaction. It commits when fn returns nil and rolls back when
// fn returns an error or panics; a panic is re-raised after the rollback.
func WithTx(ctx context.Context, pool *sql.DB, fn func(q *db.Queries) error) error {
	return withTx(ctx, pool, func(tx *sql.Tx) error { return fn(db.New(tx)) })
}

func withTx(ctx context.Context, pool *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback tx: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// IsDuplicateKey reports whether err is a MySQL duplicate-entry error (1062).
func IsDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDuplicateEntry
}

// DuplicateKeyName returns the name of the violated key ("PRIMARY", "uq_users_email", ...)
// from a duplicate-entry error, or "" when err is not one. MySQL 8 reports the key as
// "Duplicate entry '<value>' for key '<table>.<key>'"; the table prefix is dropped.
func DuplicateKeyName(err error) string {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != mysqlErrDuplicateEntry {
		return ""
	}
	// The entry value can contain anything, so search for the marker from the end.
	const marker = " for key '"
	i := strings.LastIndex(me.Message, marker)
	if i < 0 {
		return ""
	}
	key := strings.TrimSuffix(me.Message[i+len(marker):], "'")
	if j := strings.LastIndex(key, "."); j >= 0 {
		key = key[j+1:]
	}
	return key
}
