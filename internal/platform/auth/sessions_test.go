package auth_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/testkit"
)

const (
	refreshPath   = "/api/v1/auth/refresh"
	logoutPath    = "/api/v1/auth/logout"
	logoutAllPath = "/api/v1/auth/logout-all"
)

type pairBody struct {
	AccessToken          string `json:"accessToken"`
	AccessTokenExpiresAt string `json:"accessTokenExpiresAt"`
	RefreshToken         string `json:"refreshToken"`
}

func doRefresh(c *testkit.Client, token string) *testkit.Response {
	return c.Do(http.MethodPost, refreshPath, map[string]any{"refreshToken": token})
}

// loginAs logs the user in and returns the tokens. Logins never count toward the failure
// limits, so tests can log in as often as they like.
func loginAs(t *testing.T, c *testkit.Client, user testkit.User, rememberMe bool) loginBody {
	t.Helper()
	res := c.Do(http.MethodPost, loginPath, map[string]any{
		"identifier": user.Email, "password": user.Password, "rememberMe": rememberMe,
	})
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
	var body loginBody
	res.JSON(t, &body)
	return body
}

func refreshOK(t *testing.T, c *testkit.Client, token string) pairBody {
	t.Helper()
	res := doRefresh(c, token)
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
	var body pairBody
	res.JSON(t, &body)
	return body
}

func requireError(t *testing.T, res *testkit.Response, status int, code string) {
	t.Helper()
	require.Equal(t, status, res.Status, "%s", res.Body)
	require.Equal(t, code, errorOf(t, res).Error.Code)
}

type fullSession struct {
	ID, FamilyID string
	RememberMe   bool
	ExpiresAt    time.Time
	RevokedAt    sql.NullTime
	ReplacedByID sql.NullString
}

func fullSessionsOf(t *testing.T, pool *sql.DB, userID string) []fullSession {
	t.Helper()
	rows, err := pool.QueryContext(t.Context(), `
		SELECT id, family_id, remember_me, expires_at, revoked_at, replaced_by_id
		FROM sessions WHERE user_id = ? ORDER BY created_at, id`, userID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []fullSession
	for rows.Next() {
		var s fullSession
		require.NoError(t, rows.Scan(&s.ID, &s.FamilyID, &s.RememberMe, &s.ExpiresAt, &s.RevokedAt, &s.ReplacedByID))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func activeCount(sessions []fullSession) int {
	n := 0
	for _, s := range sessions {
		if !s.RevokedAt.Valid {
			n++
		}
	}
	return n
}

func TestRefreshRotatesTheToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rememberMe bool
		ttl        time.Duration
	}{
		{"default session", false, 12 * time.Hour},
		{"remember me keeps its 30 days", true, 30 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &movableClock{now: time.Now().UTC().Truncate(time.Second)}
			c, pool := setup(t, testkit.Deps{Clock: clk})
			user := testkit.CreateUser(t, pool, testkit.UserOpts{Role: db.UsersSystemRoleADMIN})

			first := loginAs(t, c, user, tc.rememberMe)
			clk.Advance(6 * time.Hour)
			refreshedAt := clk.Now()
			second := refreshOK(t, c, first.RefreshToken)

			require.NotEqual(t, first.RefreshToken, second.RefreshToken)
			require.NotEqual(t, first.AccessToken, second.AccessToken)

			sessions := fullSessionsOf(t, pool, user.ID)
			require.Len(t, sessions, 2)
			old, next := sessions[0], sessions[1]

			t.Run("the old session is retired and points at its successor", func(t *testing.T) {
				require.True(t, old.RevokedAt.Valid)
				require.True(t, old.RevokedAt.Time.Equal(refreshedAt))
				require.Equal(t, next.ID, old.ReplacedByID.String)
			})
			t.Run("the new session keeps the family and remember-me, and its lifetime restarts", func(t *testing.T) {
				require.Equal(t, old.FamilyID, next.FamilyID)
				require.Equal(t, tc.rememberMe, next.RememberMe)
				require.False(t, next.RevokedAt.Valid)
				require.True(t, next.ExpiresAt.Equal(refreshedAt.Add(tc.ttl)), "expires_at %s", next.ExpiresAt)
			})
			t.Run("the new access token belongs to the new session", func(t *testing.T) {
				claims, err := auth.NewTokens([]byte(testkit.TestJWTSecret), clk).ParseAccessToken(second.AccessToken)
				require.NoError(t, err)
				require.Equal(t, user.ID, claims.UserID)
				require.Equal(t, db.UsersSystemRoleADMIN, claims.SystemRole)
				require.Equal(t, next.ID, claims.SessionID)
			})
			t.Run("the new refresh token works, and only once", func(t *testing.T) {
				third := refreshOK(t, c, second.RefreshToken)
				require.NotEqual(t, second.RefreshToken, third.RefreshToken)
			})
			t.Run("the old refresh token is rejected", func(t *testing.T) {
				requireError(t, doRefresh(c, first.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)
			})
		})
	}
}

func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	// Two devices, two families.
	phone := loginAs(t, c, user, false)
	tablet := loginAs(t, c, user, false)

	// The phone refreshes twice: t1 -> t2 -> t3.
	t1 := phone.RefreshToken
	t2 := refreshOK(t, c, t1).RefreshToken
	t3 := refreshOK(t, c, t2).RefreshToken

	// Someone replays the long-dead t1.
	requireError(t, doRefresh(c, t1), http.StatusUnauthorized, auth.CodeRefreshReused)

	t.Run("the newest token of that chain is dead too", func(t *testing.T) {
		requireError(t, doRefresh(c, t3), http.StatusUnauthorized, auth.CodeRefreshReused)
	})
	t.Run("every session of the family is revoked in the database", func(t *testing.T) {
		phoneFamily := fullSessionsOf(t, pool, user.ID)[0].FamilyID
		revoked := 0
		for _, s := range fullSessionsOf(t, pool, user.ID) {
			if s.FamilyID == phoneFamily {
				require.True(t, s.RevokedAt.Valid, "session %s", s.ID)
				revoked++
			}
		}
		require.Equal(t, 3, revoked, "t1, t2 and t3 sessions")
	})
	t.Run("another device of the same user is not affected", func(t *testing.T) {
		refreshOK(t, c, tablet.RefreshToken)
	})
}

func TestRefreshRejectsBadTokens(t *testing.T) {
	clk := &movableClock{now: time.Now().UTC().Truncate(time.Second)}
	c, pool := setup(t, testkit.Deps{Clock: clk})

	t.Run("unknown token", func(t *testing.T) {
		requireError(t, doRefresh(c, "this-token-was-never-issued"), http.StatusUnauthorized, auth.CodeTokenInvalid)
	})

	t.Run("invalid bodies", func(t *testing.T) {
		bodies := map[string]any{
			"empty token":   map[string]any{"refreshToken": ""},
			"missing":       map[string]any{},
			"unknown field": map[string]any{"refreshToken": "x", "extra": 1},
			"wrong type":    map[string]any{"refreshToken": 123},
			"huge token":    map[string]any{"refreshToken": strings.Repeat("x", 300)},
		}
		for name, body := range bodies {
			t.Run(name, func(t *testing.T) {
				requireError(t, c.DoInvalid(http.MethodPost, refreshPath, body), http.StatusBadRequest, "VALIDATION_FAILED")
			})
		}
	})

	t.Run("expired session: invalid, and it does not revoke anything", func(t *testing.T) {
		testkit.Reset(t, pool)
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		tokens := loginAs(t, c, user, false)

		clk.Advance(12*time.Hour + time.Second)
		requireError(t, doRefresh(c, tokens.RefreshToken), http.StatusUnauthorized, auth.CodeTokenInvalid)
		require.Equal(t, 1, activeCount(fullSessionsOf(t, pool, user.ID)), "expiry is not a sign of theft")
	})

	t.Run("a session is still valid up to its last second, and sliding keeps it alive", func(t *testing.T) {
		testkit.Reset(t, pool)
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		tokens := loginAs(t, c, user, false)

		clk.Advance(12*time.Hour - time.Second)
		next := refreshOK(t, c, tokens.RefreshToken)
		clk.Advance(12*time.Hour - time.Second) // 24 hours after login, but 12 h after the refresh
		refreshOK(t, c, next.RefreshToken)
	})

	t.Run("a disabled user cannot refresh, and nothing is rotated", func(t *testing.T) {
		testkit.Reset(t, pool)
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		tokens := loginAs(t, c, user, false)
		_, err := pool.ExecContext(t.Context(), "UPDATE users SET status = 'DISABLED' WHERE id = ?", user.ID)
		require.NoError(t, err)

		requireError(t, doRefresh(c, tokens.RefreshToken), http.StatusForbidden, auth.CodeAccountDisabled)
		sessions := fullSessionsOf(t, pool, user.ID)
		require.Len(t, sessions, 1)
	})

	t.Run("a deleted user cannot refresh", func(t *testing.T) {
		testkit.Reset(t, pool)
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		tokens := loginAs(t, c, user, false)
		_, err := pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE id = ?", user.ID)
		require.NoError(t, err)

		requireError(t, doRefresh(c, tokens.RefreshToken), http.StatusUnauthorized, auth.CodeTokenInvalid)
	})

	t.Run("a role change reaches the user at the next refresh", func(t *testing.T) {
		testkit.Reset(t, pool)
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		tokens := loginAs(t, c, user, false)
		_, err := pool.ExecContext(t.Context(), "UPDATE users SET system_role = 'ADMIN' WHERE id = ?", user.ID)
		require.NoError(t, err)

		next := refreshOK(t, c, tokens.RefreshToken)
		claims, err := auth.NewTokens([]byte(testkit.TestJWTSecret), clk).ParseAccessToken(next.AccessToken)
		require.NoError(t, err)
		require.Equal(t, db.UsersSystemRoleADMIN, claims.SystemRole)
	})
}

func TestLogout(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})
	phone := loginAs(t, c, user, false)
	tablet := loginAs(t, c, user, false)
	phoneClient := c.WithToken(phone.AccessToken)

	t.Run("requires a token", func(t *testing.T) {
		requireError(t, c.DoInvalid(http.MethodPost, logoutPath, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("ends only the current session", func(t *testing.T) {
		res := phoneClient.Do(http.MethodPost, logoutPath, nil)
		require.Equal(t, http.StatusOK, res.Status)
		var body struct {
			Message string `json:"message"`
		}
		res.JSON(t, &body)
		require.Equal(t, "Anda telah keluar.", body.Message)

		sessions := fullSessionsOf(t, pool, user.ID)
		require.Len(t, sessions, 2)
		require.True(t, sessions[0].RevokedAt.Valid, "the phone session")
		require.False(t, sessions[1].RevokedAt.Valid, "the tablet session")
	})

	t.Run("the same access token is refused afterwards", func(t *testing.T) {
		requireError(t, phoneClient.Do(http.MethodPost, logoutPath, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	t.Run("the refresh token no longer works", func(t *testing.T) {
		requireError(t, doRefresh(c, phone.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)
	})

	t.Run("the other device keeps working", func(t *testing.T) {
		refreshOK(t, c, tablet.RefreshToken)
	})
}

func TestLogoutOnlyEndsTheCallersOwnSessions(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	alice := testkit.CreateUser(t, pool, testkit.UserOpts{})
	bob := testkit.CreateUser(t, pool, testkit.UserOpts{})
	aliceTokens := loginAs(t, c, alice, false)
	bobTokens := loginAs(t, c, bob, false)

	// Alice's access token carries Alice's session ID; logging out never touches Bob's.
	require.Equal(t, http.StatusOK, c.WithToken(aliceTokens.AccessToken).Do(http.MethodPost, logoutPath, nil).Status)
	require.Equal(t, 1, activeCount(fullSessionsOf(t, pool, bob.ID)))
	refreshOK(t, c, bobTokens.RefreshToken)
}

func TestLogoutAll(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})
	other := testkit.CreateUser(t, pool, testkit.UserOpts{})

	devices := []loginBody{loginAs(t, c, user, false), loginAs(t, c, user, true), loginAs(t, c, user, false)}
	otherTokens := loginAs(t, c, other, false)

	t.Run("requires a token", func(t *testing.T) {
		requireError(t, c.DoInvalid(http.MethodPost, logoutAllPath, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	})

	logoutAll := func(accessToken string) (sessionsEnded int64) {
		res := c.WithToken(accessToken).Do(http.MethodPost, logoutAllPath, nil)
		require.Equal(t, http.StatusOK, res.Status)
		var body struct {
			Message       string `json:"message"`
			SessionsEnded int64  `json:"sessionsEnded"`
		}
		res.JSON(t, &body)
		require.Equal(t, "Anda telah keluar dari semua perangkat.", body.Message)
		return body.SessionsEnded
	}
	require.EqualValues(t, 3, logoutAll(devices[0].AccessToken), "the three sessions that were active")

	// The token that made the call is cut off too, so it cannot be used a second time. A
	// fresh login can, and ends exactly the one session it created.
	requireError(t, c.WithToken(devices[0].AccessToken).Do(http.MethodPost, logoutAllPath, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	again := loginAs(t, c, user, false)
	require.EqualValues(t, 1, logoutAll(again.AccessToken))

	for _, d := range devices {
		requireError(t, doRefresh(c, d.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)
	}
	require.Zero(t, activeCount(fullSessionsOf(t, pool, user.ID)))

	t.Run("other users are untouched", func(t *testing.T) {
		refreshOK(t, c, otherTokens.RefreshToken)
	})
}

// TestParallelRefreshExactlyOneSucceeds backs the FOR UPDATE in GetSessionForRefresh. It
// calls the service directly so that the requests really overlap.
func TestParallelRefreshExactlyOneSucceeds(t *testing.T) {
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	clk := clock.System{}
	service := testkit.AuthService(t, pool, clk, nil)
	login, err := service.Login(t.Context(), auth.LoginInput{Identifier: user.Email, Password: user.Password})
	require.NoError(t, err)

	const callers = 8
	errs := make([]error, callers) // one slot per goroutine; require must run on the test goroutine
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			_, errs[i] = service.Refresh(t.Context(), auth.RefreshInput{RefreshToken: login.RefreshToken})
		})
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorIs(t, err, auth.ErrRefreshReused)
	}
	require.Equal(t, 1, succeeded, "exactly one refresh may win")

	// The losers replayed a retired token, so the whole chain, including the winner's new
	// session, is revoked.
	require.Zero(t, activeCount(fullSessionsOf(t, pool, user.ID)))
}

func TestSessionCleanup(t *testing.T) {
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	now := time.Now().UTC().Truncate(time.Second)
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	future := now.Add(12 * time.Hour)
	ptr := func(t time.Time) *time.Time { return &t }

	insert := func(name string, expiresAt time.Time, revokedAt *time.Time) {
		var revoked sql.NullTime
		if revokedAt != nil {
			revoked = sql.NullTime{Time: *revokedAt, Valid: true}
		}
		_, err := pool.ExecContext(t.Context(), `
			INSERT INTO sessions (id, user_id, family_id, refresh_token_hash, remember_me, expires_at, revoked_at)
			VALUES (UUID(), ?, UUID(), SHA2(?, 256), FALSE, ?, ?)`, user.ID, name, expiresAt, revoked)
		require.NoError(t, err)
	}

	// Deleted: dead for more than 30 days, one way or the other.
	insert("revoked-31d", future, ptr(days(31)))
	insert("expired-31d", days(31), nil)
	insert("revoked-and-expired-40d", days(40), ptr(days(40)))
	// Kept: dead for less than 30 days, so a replay is still recognised as reuse.
	insert("revoked-29d", future, ptr(days(29)))
	insert("expired-29d", days(29), nil)
	// Kept: alive.
	insert("active", future, nil)

	// Enough old rows to need several batches (the batch size is 1000).
	conn, err := pool.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(t.Context(), "SET SESSION cte_max_recursion_depth = 5000")
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `
		INSERT INTO sessions (id, user_id, family_id, refresh_token_hash, remember_me, expires_at, revoked_at)
		WITH RECURSIVE seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 2500)
		SELECT UUID(), ?, UUID(), SHA2(CONCAT('bulk-revoked-', n), 256), FALSE, ?, ? FROM seq`,
		user.ID, future, days(60))
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `
		INSERT INTO sessions (id, user_id, family_id, refresh_token_hash, remember_me, expires_at, revoked_at)
		WITH RECURSIVE seq AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 2500)
		SELECT UUID(), ?, UUID(), SHA2(CONCAT('bulk-expired-', n), 256), FALSE, ?, NULL FROM seq`,
		user.ID, days(60))
	require.NoError(t, err)

	// Run it the way the worker does, through the registered asynq handler.
	mux := asynq.NewServeMux()
	auth.RegisterJobs(mux, auth.NewSessionCleaner(pool, clock.Fixed{T: now}), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, mux.ProcessTask(t.Context(), asynq.NewTask(auth.TaskCleanupSessions, nil)))

	rows, err := pool.QueryContext(t.Context(), "SELECT refresh_token_hash FROM sessions")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var remaining []string
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))
		remaining = append(remaining, hash)
	}
	require.NoError(t, rows.Err())

	require.Len(t, remaining, 3, "5000 bulk rows and the three old ones are gone")
	for _, name := range []string{"revoked-29d", "expired-29d", "active"} {
		var hash string
		require.NoError(t, pool.QueryRowContext(t.Context(), "SELECT SHA2(?, 256)", name).Scan(&hash))
		require.Contains(t, remaining, hash, name)
	}

	t.Run("running it again finds nothing", func(t *testing.T) {
		deleted, err := auth.NewSessionCleaner(pool, clock.Fixed{T: now}).Cleanup(t.Context())
		require.NoError(t, err)
		require.Zero(t, deleted)
	})
}

func TestSessionCleanupStopsWhenCancelled(t *testing.T) {
	pool := testkit.DB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := auth.NewSessionCleaner(pool, clock.System{}).Cleanup(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCleanupIsScheduled(t *testing.T) {
	scheduler := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "localhost:6379"}, &asynq.SchedulerOpts{Location: time.UTC})
	require.NoError(t, auth.RegisterSchedule(scheduler), "the cron expression must be valid")
}
