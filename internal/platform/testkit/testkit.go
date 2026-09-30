// Package testkit sets up real MySQL-backed HTTP tests: one database per test package,
// table resets between tests, and a server built from the real router.
//
// Usage in a package:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }
//
//	func TestSomething(t *testing.T) {
//		pool := testkit.DB(t)
//		testkit.Reset(t, pool)
//		...
//	}
//
// Tests that share the package database must not call t.Parallel, because Reset truncates
// the tables every test sees.
package testkit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"siapp/internal/platform/database"
	"siapp/internal/platform/httpx"
)

// defaultAdminDSN matches the root account of docker-compose.yml. The database name is
// left empty because each test package creates its own.
const defaultAdminDSN = "root:root@tcp(localhost:3306)/?parseTime=true&loc=UTC&time_zone=%27%2B00%3A00%27"

const (
	setupTimeout     = 60 * time.Second
	maxPkgNameLen    = 30
	randSuffixBytes  = 4
	shortModeMessage = "testkit: -short set, skipping tests that need MySQL"
)

// pool is the package-wide test database. Each test package compiles into its own binary,
// so this is per package.
var pool *sql.DB

// NewDatabase is meant for TestMain. It creates siapp_test_<pkg>_<rand>, runs the
// migrations, runs the tests and drops the database. It returns the exit code for os.Exit.
// With -short it skips the database entirely and DB(t) skips each test that needs it.
//
// TEST_MYSQL_DSN overrides the admin connection; it needs CREATE and DROP privileges.
func NewDatabase(m *testing.M) int {
	flag.Parse()
	if testing.Short() {
		fmt.Println(shortModeMessage)
		return m.Run()
	}

	adminDSN := os.Getenv("TEST_MYSQL_DSN")
	if adminDSN == "" {
		adminDSN = defaultAdminDSN
	}
	name, dsn, err := databaseDSN(adminDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testkit:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := sql.Open("mysql", adminDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testkit: open admin connection:", err)
		return 1
	}
	defer func() { _ = admin.Close() }()

	// DDL cannot take placeholders. The name is built from [a-z0-9_] only (see
	// databaseDSN), so quoting it with backticks is safe.
	create := "CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci" //nolint:gosec // sanitized name
	if _, err := admin.ExecContext(ctx, create); err != nil {                                   //nolint:gosec // sanitized name
		fmt.Fprintf(os.Stderr, "testkit: create database (is `make up` running?): %v\n", err)
		return 1
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE `"+name+"`"); err != nil { //nolint:gosec // sanitized name
			fmt.Fprintln(os.Stderr, "testkit: drop database:", err)
		}
	}()

	pool, err = database.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testkit:", err)
		return 1
	}
	defer func() { _ = pool.Close() }()

	if err := database.Migrate(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, "testkit:", err)
		return 1
	}
	return m.Run()
}

// databaseDSN picks a unique database name for this test binary and returns the DSN for it.
func databaseDSN(adminDSN string) (name, dsn string, err error) {
	cfg, err := mysql.ParseDSN(adminDSN)
	if err != nil {
		return "", "", fmt.Errorf("parse TEST_MYSQL_DSN: %w", err)
	}
	suffix := make([]byte, randSuffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		return "", "", fmt.Errorf("random suffix: %w", err)
	}
	cfg.DBName = "siapp_test_" + packageName() + "_" + hex.EncodeToString(suffix)
	return cfg.DBName, cfg.FormatDSN(), nil
}

// packageName derives a safe identifier from the test binary name ("httpx.test").
func packageName() string {
	base := strings.ToLower(filepath.Base(os.Args[0]))
	base, _, _ = strings.Cut(base, ".")
	var b strings.Builder
	for _, c := range base {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if len(name) > maxPkgNameLen {
		name = name[:maxPkgNameLen]
	}
	return name
}

// DB returns the package test database. It skips the test in -short mode.
func DB(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip(shortModeMessage)
	}
	if pool == nil {
		t.Fatal("testkit: no database; call os.Exit(testkit.NewDatabase(m)) in TestMain")
	}
	return pool
}

// Reset empties every table except goose_db_version.
//
// ⚠ The table list comes from information_schema, so tables added by later migrations are
// covered without touching this code. FOREIGN_KEY_CHECKS is a session variable, so all
// statements run on one dedicated connection; on a pooled *sql.DB each Exec could land on
// a different connection and TRUNCATE would fail on FK-referenced tables. TRUNCATE is used
// instead of DELETE because it is faster on small tables and resets AUTO_INCREMENT.
func Reset(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	ctx := context.Background()

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("testkit reset: get connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' AND table_name <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("testkit reset: list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("testkit reset: scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("testkit reset: list tables: %v", err)
	}
	_ = rows.Close()

	if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("testkit reset: disable FK checks: %v", err)
	}
	// Re-enable before the connection goes back to the pool, even if a TRUNCATE fails.
	defer func() {
		if _, err := conn.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
			t.Errorf("testkit reset: enable FK checks: %v", err)
		}
	}()
	for _, table := range tables {
		// Table names come from information_schema, not from user input.
		if _, err := conn.ExecContext(ctx, "TRUNCATE TABLE `"+table+"`"); err != nil {
			t.Fatalf("testkit reset: truncate %s: %v", table, err)
		}
	}
}

// Deps are the dependencies NewServer wires into the router. Zero values get defaults.
type Deps struct {
	DB *sql.DB
}

// NewServer starts an httptest.Server running the real router and closes it when the
// test ends.
func NewServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	if deps.DB == nil {
		deps.DB = DB(t)
	}
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:     deps.DB,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}
