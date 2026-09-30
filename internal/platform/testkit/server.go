package testkit

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"siapp/internal/app"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
)

// TestJWTSecret signs the access tokens of test servers.
const TestJWTSecret = "test-secret-test-secret-test-secret-0123"

const defaultRedisURL = "redis://localhost:6379/0"

// TestPasswordParams keep argon2 fast: tests hash and verify passwords all the time and
// need none of the production cost.
var TestPasswordParams = auth.PasswordParams{MemoryKiB: 8, Time: 1, Threads: 1}

// Passwords is the cheap hasher used by test servers and the user factory.
var Passwords = auth.NewPasswords(TestPasswordParams)

// Deps are what a test can override on the API it starts. Zero values get defaults.
type Deps struct {
	DB *sql.DB
	// Clock defaults to the system clock. A test that fixes it also fixes token expiry.
	Clock clock.Clock
	// ExtraRoutes mounts routes that are not part of the API, behind the real authentication.
	ExtraRoutes func(chi.Router)
}

// NewRouter builds the real API router from the same wiring as production (internal/app),
// on the package test database and a real Redis.
func NewRouter(t *testing.T, deps Deps) *chi.Mux {
	t.Helper()
	if deps.DB == nil {
		deps.DB = DB(t)
	}
	return app.NewRouter(app.Deps{
		Logger:          slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:              deps.DB,
		Redis:           Redis(t),
		Clock:           deps.Clock,
		JWTSecret:       []byte(TestJWTSecret),
		RateLimitPrefix: RandomPrefix(t),
		Passwords:       Passwords,
		ExtraRoutes:     deps.ExtraRoutes,
	})
}

// NewServer starts an httptest.Server running NewRouter and closes it when the test ends.
func NewServer(t *testing.T, deps Deps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewRouter(t, deps))
	t.Cleanup(srv.Close)
	return srv
}

// Redis connects to the test Redis (TEST_REDIS_URL, default localhost:6379/0).
//
// Tests never flush it: the limiter keys carry a random prefix per test server (see
// RandomPrefix), so tests cannot see each other's counters, and the developer's own data
// is left alone.
func Redis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		url = defaultRedisURL
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("testkit: parse TEST_REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(t.Context()).Err(); err != nil {
		t.Fatalf("testkit: cannot reach redis at %s (start it, or set TEST_REDIS_URL): %v", url, err)
	}
	return rdb
}

// RandomPrefix returns a Redis key prefix no other test uses.
func RandomPrefix(t *testing.T) string {
	t.Helper()
	return "test:" + randomHex(t) + ":"
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("testkit: random: %v", err)
	}
	return hex.EncodeToString(b)
}
