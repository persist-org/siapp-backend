// Package testkit gives tests a real Postgres (as the app role, so RLS applies), a Redis
// client and the real HTTP router. Only test code imports it.
//
// A package that needs the database adds
//
//	func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }
//
// which creates a private database for the package's test binary, migrates it as the owner
// and drops it afterwards. Packages run in parallel, so each gets its own.
package testkit

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
)

const setupTimeout = 60 * time.Second

var shared struct {
	mu     sync.Mutex
	app    *pgxpool.Pool // connects as siapp_app: RLS applies
	owner  *pgxpool.Pool // connects as the database owner: for DDL and truncation only
	appDSN string
	// ownerDSN is the test database as its owner, for code that runs as the owner role (the
	// seed command's grant-admin) and builds its own pool.
	ownerDSN string
}

var nextOrgID atomic.Int64

// NewDatabase is for TestMain. It returns the exit code to pass to os.Exit.
// With -short it does nothing and database tests skip themselves.
func NewDatabase(m *testing.M) int {
	flag.Parse()
	if testing.Short() {
		return m.Run()
	}
	code, err := runWithDatabase(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testkit:", err)
		return 1
	}
	return code
}

func runWithDatabase(m *testing.M) (int, error) {
	adminDSN := os.Getenv("MIGRATE_DSN")
	if adminDSN == "" {
		return 0, fmt.Errorf("MIGRATE_DSN is not set; run `make up` and `make test`, or use -short to skip database tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("cannot reach Postgres with MIGRATE_DSN (is `make up` running?): %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := databaseName()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return 0, fmt.Errorf("create database %s: %w", name, err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()

	ownerDSN, err := withDatabase(adminDSN, name, "", "")
	if err != nil {
		return 0, err
	}
	if err := database.Migrate(ctx, ownerDSN, "up"); err != nil {
		return 0, err
	}
	appDSN, err := withDatabase(adminDSN, name,
		envOr("TEST_APP_USER", "siapp_app"), envOr("TEST_APP_PASSWORD", "siapp_app"))
	if err != nil {
		return 0, err
	}

	owner, err := database.Open(ctx, ownerDSN)
	if err != nil {
		return 0, err
	}
	defer owner.Close()
	app, err := database.Open(ctx, appDSN)
	if err != nil {
		return 0, err
	}
	defer app.Close()

	shared.mu.Lock()
	shared.owner, shared.app, shared.appDSN, shared.ownerDSN = owner, app, appDSN, ownerDSN
	shared.mu.Unlock()

	return m.Run(), nil
}

// Pool returns the pool that connects as siapp_app, the same role production uses.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	app, _ := pools(t)
	return app
}

// OwnerPool returns a pool that connects as the database owner. It bypasses nothing it
// should not, but it is not subject to the app role's grants: use it only to create test
// tables or to inspect the catalog, never to exercise application code.
func OwnerPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	_, owner := pools(t)
	return owner
}

// AppURL returns the connection URL of the test database as siapp_app, for tests that
// need their own pool (a pool of one connection, a pool they close).
func AppURL(t testing.TB) string {
	t.Helper()
	pools(t)
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return shared.appDSN
}

// OwnerURL returns the connection URL of the test database as its owner. Use it only for
// code that is meant to run as the owner role.
func OwnerURL(t testing.TB) string {
	t.Helper()
	pools(t)
	shared.mu.Lock()
	defer shared.mu.Unlock()
	return shared.ownerDSN
}

func pools(t testing.TB) (app, owner *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set, skipping tests that need Postgres")
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.app == nil {
		t.Fatal("no test database: add `func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }` to this package")
	}
	return shared.app, shared.owner
}

// Reset empties every table except goose's bookkeeping and restarts identities. The table
// list comes from pg_tables, so a table added by a later migration is covered at once.
func Reset(t testing.TB) {
	t.Helper()
	owner := OwnerPool(t)
	rows, err := owner.Query(t.Context(),
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	require.NoError(t, err)
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var name string
		return name, r.Scan(&name)
	})
	require.NoError(t, err)
	if len(tables) == 0 {
		return
	}
	quoted := make([]string, len(tables))
	for i, name := range tables {
		quoted[i] = pgx.Identifier{name}.Sanitize()
	}
	_, err = owner.Exec(t.Context(), "TRUNCATE "+strings.Join(quoted, ", ")+" RESTART IDENTITY CASCADE")
	require.NoError(t, err)
}

// NewOrg registers an organization and returns its id. Most tests create two, to prove
// one cannot see the other.
func NewOrg(t testing.TB) int64 {
	t.Helper()
	id := nextOrgID.Add(1)
	err := database.WithoutTenant(t.Context(), Pool(t), func(q *db.Queries) error {
		_, err := q.UpsertOrganizationSeen(t.Context(), db.UpsertOrganizationSeenParams{
			ID: id, Name: fmt.Sprintf("Org %d", id),
		})
		return err
	})
	require.NoError(t, err)
	return id
}

// Redis returns a client for the test Redis (TEST_REDIS_URL, default localhost:6379).
func Redis(t testing.TB) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set, skipping tests that need Redis")
	}
	url := envOr("TEST_REDIS_URL", "redis://localhost:6379/0")
	opts, err := redis.ParseURL(url)
	require.NoError(t, err, "parse TEST_REDIS_URL")
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(t.Context()).Err(), "cannot reach redis at %s (make up, or set TEST_REDIS_URL)", url)
	return rdb
}

// RandomPrefix returns a Redis key prefix no other test uses.
func RandomPrefix(t testing.TB) string {
	t.Helper()
	return "test:" + randomSuffix() + ":"
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// databaseName is siapp_test_<package>_<random>, within Postgres's 63-byte identifier limit.
func databaseName() string {
	wd, _ := os.Getwd()
	pkg := strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(filepath.Base(wd)), "_"), "_")
	if len(pkg) > 30 {
		pkg = pkg[:30]
	}
	return fmt.Sprintf("siapp_test_%s_%s", pkg, randomSuffix())
}

// randomSuffix is 8 random lowercase letters and digits, safe in identifiers and keys.
func randomSuffix() string { return strings.ToLower(rand.Text()[:8]) }

// withDatabase returns dsn pointing at another database, optionally as another user.
func withDatabase(dsn, name, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("MIGRATE_DSN is not a URL")
	}
	u.Path = "/" + name
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String(), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
