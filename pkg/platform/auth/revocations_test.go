package auth_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func doGet(t *testing.T, router http.Handler, path, authorization string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	req.Header.Set("Authorization", authorization)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Code
}

func TestRevokedSessionsAreRefusedByRequire(t *testing.T) {
	rdb := testkit.Redis(t)
	prefix := testkit.RandomPrefix(t)
	revocations := auth.NewRevocations(rdb, prefix, quietLogger())
	tokens := newTokens(t, time.Now().UTC())

	router := httpx.NewRouter(httpx.RouterConfig{Logger: quietLogger(), Authenticate: auth.Require(tokens, revocations)})
	router.Get("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) { httpx.OK(w, r, http.StatusOK, nil) })

	token := func(session int64) string {
		s, _, err := tokens.NewAccessToken(15, 7, session)
		require.NoError(t, err)
		return s
	}
	get := func(session int64) int {
		return doGet(t, router, "/api/v1/whoami", "Bearer "+token(session))
	}

	require.Equal(t, http.StatusOK, get(99))
	require.False(t, revocations.IsRevoked(t.Context(), 99))

	revocations.Revoke(t.Context(), 99, 100)
	require.True(t, revocations.IsRevoked(t.Context(), 99))
	require.Equal(t, http.StatusUnauthorized, get(99), "a revoked session is refused at once")
	require.Equal(t, http.StatusUnauthorized, get(100))
	require.Equal(t, http.StatusOK, get(101), "another session is unaffected")

	// The entry lives a little longer than the tokens it cuts off, and then goes: Redis holds
	// the last few minutes of revocations, not a history.
	ttl, err := rdb.TTL(t.Context(), prefix+"revoked-session:99").Result()
	require.NoError(t, err)
	require.Greater(t, ttl, auth.AccessTokenTTL)
	require.LessOrEqual(t, ttl, auth.AccessTokenTTL+time.Minute)
}

func TestRevokeWithNothingToRevokeIsANoOp(t *testing.T) {
	revocations := auth.NewRevocations(testkit.Redis(t), testkit.RandomPrefix(t), quietLogger())
	revocations.Revoke(t.Context())
}

// ⚠ Fail open: with Redis down, a valid token still passes. Failing closed would turn every
// Redis outage into an API outage.
func TestRevocationsFailOpenWhenRedisIsDown(t *testing.T) {
	dead := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1,
	})
	t.Cleanup(func() { _ = dead.Close() })
	revocations := auth.NewRevocations(dead, "test:", quietLogger())

	require.False(t, revocations.IsRevoked(t.Context(), 99), "an unreachable list says not revoked")
	revocations.Revoke(t.Context(), 99) // gives up after logging, does not panic or hang

	tokens := newTokens(t, time.Now().UTC())
	router := httpx.NewRouter(httpx.RouterConfig{Logger: quietLogger(), Authenticate: auth.Require(tokens, revocations)})
	router.Get("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) { httpx.OK(w, r, http.StatusOK, nil) })
	token, _, err := tokens.NewAccessToken(15, 7, 99)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, doGet(t, router, "/api/v1/whoami", "Bearer "+token))
}

func TestRequireNeedsRevocations(t *testing.T) {
	require.Panics(t, func() { auth.Require(newTokens(t, time.Now().UTC()), nil) },
		"a router without a revocation list would keep ended sessions alive")
}
