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

	"siapp/internal/db"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/testkit"
)

const whoamiPath = "/api/v1/test/whoami"

// newRevocationEnv starts a server with the whoami route, for checking what an access token
// can still do after its session ended. It returns the Redis key prefix of the server.
func newRevocationEnv(t *testing.T, clk clock.Clock) (*httptest.Server, *testkit.Client, string) {
	t.Helper()
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	prefix := testkit.RandomPrefix(t)
	srv := testkit.NewServer(t, testkit.Deps{DB: pool, Clock: clk, ExtraRoutes: whoami, RedisPrefix: prefix})
	return srv, testkit.NewClient(t, srv), prefix
}

func statusOf(t *testing.T, srv *httptest.Server, accessToken string) int {
	t.Helper()
	return get(t, srv, whoamiPath, "Bearer "+accessToken).StatusCode
}

func TestLogoutCutsOffTheAccessTokenAtOnce(t *testing.T) {
	srv, c, _ := newRevocationEnv(t, nil)
	pool := testkit.DB(t)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})
	phone := loginAs(t, c, user, false)
	tablet := loginAs(t, c, user, false)

	require.Equal(t, http.StatusOK, statusOf(t, srv, phone.AccessToken), "works before logout")

	res := c.WithToken(phone.AccessToken).Do(http.MethodPost, logoutPath, nil)
	require.Equal(t, http.StatusOK, res.Status)

	t.Run("the same token is dead on every protected route, not only on logout", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, statusOf(t, srv, phone.AccessToken))
	})
	t.Run("a second logout with it is 401, not another success", func(t *testing.T) {
		requireError(t, c.WithToken(phone.AccessToken).Do(http.MethodPost, logoutPath, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	})
	t.Run("another device of the same user is not affected", func(t *testing.T) {
		require.Equal(t, http.StatusOK, statusOf(t, srv, tablet.AccessToken))
	})
}

func TestLogoutAllCutsOffEveryAccessToken(t *testing.T) {
	srv, c, _ := newRevocationEnv(t, nil)
	pool := testkit.DB(t)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})
	other := testkit.CreateUser(t, pool, testkit.UserOpts{})

	phone := loginAs(t, c, user, false)
	tablet := loginAs(t, c, user, false)
	otherTokens := loginAs(t, c, other, false)

	// The phone refreshes, so its first access token belongs to a session that has since
	// been rotated. That token still works: rotation alone does not cut it off, because a
	// client may have requests in flight that still carry it.
	refreshed := refreshOK(t, c, phone.RefreshToken)
	require.Equal(t, http.StatusOK, statusOf(t, srv, phone.AccessToken), "the pre-rotation token still works")

	c.WithToken(refreshed.AccessToken).Do(http.MethodPost, logoutAllPath, nil)

	// But logout-all ends everything, including tokens of sessions that were rotated a
	// moment ago.
	for name, token := range map[string]string{
		"the phone's first token":     phone.AccessToken,
		"the phone's refreshed token": refreshed.AccessToken,
		"the tablet's token":          tablet.AccessToken,
	} {
		require.Equal(t, http.StatusUnauthorized, statusOf(t, srv, token), name)
	}
	require.Equal(t, http.StatusOK, statusOf(t, srv, otherTokens.AccessToken), "other users are not affected")
}

func TestReuseDetectionCutsOffTheStolenAccessTokens(t *testing.T) {
	srv, c, _ := newRevocationEnv(t, nil)
	pool := testkit.DB(t)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	phone := loginAs(t, c, user, false)
	tablet := loginAs(t, c, user, false)

	// Chain on the phone: login -> refresh -> refresh.
	second := refreshOK(t, c, phone.RefreshToken)
	third := refreshOK(t, c, second.RefreshToken)
	for name, token := range map[string]string{"first": phone.AccessToken, "second": second.AccessToken, "third": third.AccessToken} {
		require.Equal(t, http.StatusOK, statusOf(t, srv, token), "%s token works before the replay", name)
	}

	// Someone replays the first refresh token.
	requireError(t, doRefresh(c, phone.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)

	for name, token := range map[string]string{"first": phone.AccessToken, "second": second.AccessToken, "third": third.AccessToken} {
		require.Equal(t, http.StatusUnauthorized, statusOf(t, srv, token), "%s access token is cut off with its chain", name)
	}
	require.Equal(t, http.StatusOK, statusOf(t, srv, tablet.AccessToken), "another device is not affected")
}

func TestRevocationOnlyCoversTokensThatCanStillBeAlive(t *testing.T) {
	// Sessions older than an access token's lifetime cannot have a live token, so they are
	// not listed. This keeps a logout-all on an account with months of history cheap.
	clk := &movableClock{now: time.Now().UTC().Truncate(time.Second)}
	_, c, prefix := newRevocationEnv(t, clk)
	pool := testkit.DB(t)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	loginAs(t, c, user, true)
	clk.Advance(20 * time.Minute) // the first access token has expired by now
	fresh := loginAs(t, c, user, true)

	c.WithToken(fresh.AccessToken).WithClock(clk).Do(http.MethodPost, logoutAllPath, nil)

	rdb := testkit.Redis(t)
	keys, err := rdb.Keys(t.Context(), prefix+"revoked-session:*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1, "only the session created within the last 15 minutes is listed")
}

func TestRevocationEntriesExpire(t *testing.T) {
	_, c, prefix := newRevocationEnv(t, nil)
	pool := testkit.DB(t)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})
	tokens := loginAs(t, c, user, false)
	c.WithToken(tokens.AccessToken).Do(http.MethodPost, logoutPath, nil)

	rdb := testkit.Redis(t)
	keys, err := rdb.Keys(t.Context(), prefix+"revoked-session:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys)

	// An entry has to outlive the 15 minute access token, and must not stay forever.
	ttl, err := rdb.PTTL(t.Context(), keys[0]).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 15*time.Minute)
	require.LessOrEqual(t, ttl, 16*time.Minute)
}

// TestRevocationFailsOpen is the decision behind the revocation list: when Redis is down the
// API keeps serving requests instead of rejecting them all.
func TestRevocationFailsOpen(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = dead.Close() })
	revocations := auth.NewRevocations(dead, "x:", slog.New(slog.NewJSONHandler(io.Discard, nil)))

	t.Run("IsRevoked reports not revoked, and Revoke does not panic", func(t *testing.T) {
		require.False(t, revocations.IsRevoked(t.Context(), "some-session"))
		require.NotPanics(t, func() { revocations.Revoke(t.Context(), "some-session") })
	})

	t.Run("a valid token still gets through the middleware", func(t *testing.T) {
		tokens := auth.NewTokens([]byte(testkit.TestJWTSecret), clock.System{})
		token, _, err := tokens.NewAccessToken("user-1", db.UsersSystemRoleMEMBER, "session-1")
		require.NoError(t, err)

		reached := false
		handler := auth.Require(tokens, revocations)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/anything", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		require.True(t, reached)
		require.Equal(t, http.StatusNoContent, rec.Code)
	})

	t.Run("an invalid token is still refused while Redis is down", func(t *testing.T) {
		handler := auth.Require(auth.NewTokens([]byte(testkit.TestJWTSecret), clock.System{}), revocations)(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("must not be reached") }))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/anything", nil)
		req.Header.Set("Authorization", "Bearer not-a-jwt")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestRevocationsRoundTrip(t *testing.T) {
	r := testkit.Revocations(t)
	require.False(t, r.IsRevoked(t.Context(), "session-a"))

	r.Revoke(t.Context(), "session-a", "session-b")
	require.True(t, r.IsRevoked(t.Context(), "session-a"))
	require.True(t, r.IsRevoked(t.Context(), "session-b"))
	require.False(t, r.IsRevoked(t.Context(), "session-c"))

	require.NotPanics(t, func() { r.Revoke(t.Context()) }, "nothing to do is fine")
}
