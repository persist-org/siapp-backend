package httpx_test

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/httpx"
)

var quiet = slog.New(slog.NewJSONHandler(io.Discard, nil))

// testRedis and randomPrefix stand in for the testkit helpers until Phase 00.2 rebuilds
// testkit; then these tests switch back to testkit.Redis and testkit.RandomPrefix.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set, skipping tests that need Redis")
	}
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opts, err := redis.ParseURL(url)
	require.NoError(t, err, "parse TEST_REDIS_URL")
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(t.Context()).Err(), "cannot reach redis at %s (make up, or set TEST_REDIS_URL)", url)
	return rdb
}

// randomPrefix returns a Redis key prefix no other test uses.
func randomPrefix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return "test:" + hex.EncodeToString(b) + ":"
}

func TestFailureLimiter(t *testing.T) {
	rdb := testRedis(t)
	newLimiter := func() *httpx.FailureLimiter {
		return httpx.NewFailureLimiter(rdb, randomPrefix(t), quiet)
	}
	rule := func(key string, limit int, window time.Duration) httpx.FailureRule {
		return httpx.FailureRule{Name: "test", Key: key, Max: limit, Window: window}
	}

	t.Run("blocks after Max failures and not before", func(t *testing.T) {
		l := newLimiter()
		r := rule("a", 3, time.Minute)

		for i := range 3 {
			_, blocked := l.Check(t.Context(), r)
			require.False(t, blocked, "before failure %d", i+1)
			l.Record(t.Context(), r)
		}
		retryAfter, blocked := l.Check(t.Context(), r)
		require.True(t, blocked)
		require.Positive(t, retryAfter)
		require.LessOrEqual(t, retryAfter, time.Minute)
	})

	t.Run("keys are independent", func(t *testing.T) {
		l := newLimiter()
		for range 3 {
			l.Record(t.Context(), rule("a", 3, time.Minute))
		}
		_, blocked := l.Check(t.Context(), rule("b", 3, time.Minute))
		require.False(t, blocked)
	})

	t.Run("several rules: any exhausted rule blocks, retry-after is the longest", func(t *testing.T) {
		l := newLimiter()
		short := rule("k1", 1, time.Minute)
		long := rule("k2", 1, 10*time.Minute)
		l.Record(t.Context(), short, long)

		retryAfter, blocked := l.Check(t.Context(), short, long)
		require.True(t, blocked)
		require.Greater(t, retryAfter, time.Minute, "the longer window decides")

		// A client that is fresh on one rule is still blocked by another that is exhausted.
		_, blocked = l.Check(t.Context(), rule("fresh", 1, time.Minute), long)
		require.True(t, blocked)
	})

	t.Run("the block ends when the window does", func(t *testing.T) {
		l := newLimiter()
		r := rule("a", 2, 300*time.Millisecond)
		l.Record(t.Context(), r)
		l.Record(t.Context(), r)
		_, blocked := l.Check(t.Context(), r)
		require.True(t, blocked)

		time.Sleep(400 * time.Millisecond)
		_, blocked = l.Check(t.Context(), r)
		require.False(t, blocked)

		l.Record(t.Context(), r)
		_, blocked = l.Check(t.Context(), r)
		require.False(t, blocked, "a new window starts from zero")
	})

	t.Run("the counter always has a TTL, so a client can never be blocked forever", func(t *testing.T) {
		prefix := randomPrefix(t)
		l := httpx.NewFailureLimiter(rdb, prefix, quiet)
		l.Record(t.Context(), rule("a", 3, time.Minute))

		ttl, err := rdb.PTTL(t.Context(), prefix+"fail:test:a").Result()
		require.NoError(t, err)
		require.Positive(t, ttl)
		require.LessOrEqual(t, ttl, time.Minute)

		// A second failure must not extend the window.
		time.Sleep(50 * time.Millisecond)
		l.Record(t.Context(), rule("a", 3, time.Minute))
		ttl2, err := rdb.PTTL(t.Context(), prefix+"fail:test:a").Result()
		require.NoError(t, err)
		require.Less(t, ttl2, ttl)
	})

	t.Run("fails open when Redis is unreachable", func(t *testing.T) {
		dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
		t.Cleanup(func() { _ = dead.Close() })
		l := httpx.NewFailureLimiter(dead, "x:", quiet)

		_, blocked := l.Check(t.Context(), rule("a", 1, time.Minute))
		require.False(t, blocked)
		require.NotPanics(t, func() { l.Record(t.Context(), rule("a", 1, time.Minute)) })
	})
}

func TestRateLimiterPerMinute(t *testing.T) {
	limiter := httpx.NewRateLimiter(testRedis(t), randomPrefix(t), quiet)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	byHeader := func(r *http.Request) string { return r.Header.Get("X-Client") }
	srv := httptest.NewServer(limiter.PerMinute("test", 3, byHeader)(ok))
	t.Cleanup(srv.Close)

	call := func(client string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		req.Header.Set("X-Client", client)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res
	}

	for i := 1; i <= 3; i++ {
		require.Equal(t, http.StatusNoContent, call("a").StatusCode, "request %d", i)
	}
	res := call("a")
	require.Equal(t, http.StatusTooManyRequests, res.StatusCode)
	seconds, err := strconv.Atoi(res.Header.Get("Retry-After"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, seconds, 1)
	require.LessOrEqual(t, seconds, 60)

	require.Equal(t, http.StatusNoContent, call("b").StatusCode, "another client is unaffected")
}

func TestTooManyRequestsRetryAfter(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "1"},
		{time.Millisecond, "1"},
		{time.Second, "1"},
		{1500 * time.Millisecond, "2"},
		{time.Minute, "60"},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		httpx.TooManyRequests(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), tc.in)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.Equal(t, tc.want, rec.Header().Get("Retry-After"), tc.in.String())
	}
}
