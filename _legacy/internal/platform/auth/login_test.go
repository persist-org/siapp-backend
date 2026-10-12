package auth_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

const loginPath = "/api/v1/auth/login"

type loginBody struct {
	AccessToken          string `json:"accessToken"`
	AccessTokenExpiresAt string `json:"accessTokenExpiresAt"`
	RefreshToken         string `json:"refreshToken"`
	User                 struct {
		ID         string  `json:"id"`
		Email      string  `json:"email"`
		Username   *string `json:"username"`
		FullName   string  `json:"fullName"`
		Phone      *string `json:"phone"`
		SystemRole string  `json:"systemRole"`
		Status     string  `json:"status"`
	} `json:"user"`
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// setup starts a server on a clean database and returns a contract client for it.
func setup(t *testing.T, deps testkit.Deps) (*testkit.Client, *sql.DB) {
	t.Helper()
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	deps.DB = pool
	return testkit.NewClient(t, testkit.NewServer(t, deps)), pool
}

func login(c *testkit.Client, identifier, password string) *testkit.Response {
	return c.Do(http.MethodPost, loginPath, map[string]any{"identifier": identifier, "password": password})
}

func errorOf(t *testing.T, res *testkit.Response) errBody {
	t.Helper()
	var body errBody
	res.JSON(t, &body)
	return body
}

type sessionRow struct {
	ID, FamilyID, RefreshHash string
	RememberMe                bool
	ExpiresAt                 time.Time
	UserAgent, IP             sql.NullString
}

func sessionsOf(t *testing.T, pool *sql.DB, userID string) []sessionRow {
	t.Helper()
	rows, err := pool.QueryContext(t.Context(), `
		SELECT id, family_id, refresh_token_hash, remember_me, expires_at, user_agent, ip
		FROM sessions WHERE user_id = ? ORDER BY created_at`, userID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		require.NoError(t, rows.Scan(&s.ID, &s.FamilyID, &s.RefreshHash, &s.RememberMe, &s.ExpiresAt, &s.UserAgent, &s.IP))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

type lockState struct {
	Failed      int
	LockedUntil sql.NullTime
	LastLogin   sql.NullTime
}

func lockOf(t *testing.T, pool *sql.DB, userID string) lockState {
	t.Helper()
	var s lockState
	err := pool.QueryRowContext(t.Context(),
		"SELECT failed_login_count, locked_until, last_login_at FROM users WHERE id = ?", userID).
		Scan(&s.Failed, &s.LockedUntil, &s.LastLogin)
	require.NoError(t, err)
	return s
}

func TestLoginSucceeds(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{Email: "budi@example.com", Username: "budi.s", FullName: "Budi Santoso"})

	for _, identifier := range []string{"budi@example.com", "BUDI@Example.com", "  budi@example.com ", "budi.s"} {
		t.Run(identifier, func(t *testing.T) {
			res := login(c, identifier, user.Password)
			require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)

			var body loginBody
			res.JSON(t, &body)
			require.Equal(t, user.ID, body.User.ID)
			require.Equal(t, "budi@example.com", body.User.Email)
			require.Equal(t, "budi.s", *body.User.Username)
			require.Equal(t, "Budi Santoso", body.User.FullName)
			require.Nil(t, body.User.Phone)
			require.Equal(t, "MEMBER", body.User.SystemRole)
			require.Equal(t, "ACTIVE", body.User.Status)
			require.NotEmpty(t, body.RefreshToken)
			require.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`, body.AccessTokenExpiresAt)
		})
	}
}

func TestLoginResponseAndStoredSession(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{Role: db.UsersSystemRoleADMIN})

	res := login(c, user.Email, user.Password)
	require.Equal(t, http.StatusOK, res.Status)
	var body loginBody
	res.JSON(t, &body)

	t.Run("the access token carries the user, role and session", func(t *testing.T) {
		sessions := sessionsOf(t, pool, user.ID)
		require.Len(t, sessions, 1)

		claims, err := auth.NewTokens([]byte(testkit.TestJWTSecret), clock.System{}).ParseAccessToken(body.AccessToken)
		require.NoError(t, err)
		require.Equal(t, user.ID, claims.UserID)
		require.Equal(t, db.UsersSystemRoleADMIN, claims.SystemRole)
		require.Equal(t, sessions[0].ID, claims.SessionID)
	})

	t.Run("only the hash of the refresh token is stored", func(t *testing.T) {
		sessions := sessionsOf(t, pool, user.ID)
		sum := sha256.Sum256([]byte(body.RefreshToken))
		require.Equal(t, hex.EncodeToString(sum[:]), sessions[0].RefreshHash)
		require.NotEqual(t, body.RefreshToken, sessions[0].RefreshHash)
		require.True(t, sessions[0].UserAgent.Valid)
		require.Equal(t, "127.0.0.1", sessions[0].IP.String)
	})

	t.Run("the response never contains password or birthdate", func(t *testing.T) {
		require.NotContains(t, string(res.Body), "password")
		require.NotContains(t, string(res.Body), "birth")
		require.NotContains(t, string(res.Body), "$argon2id")
	})

	t.Run("last login is recorded", func(t *testing.T) {
		require.True(t, lockOf(t, pool, user.ID).LastLogin.Valid)
	})

	t.Run("every login is a new session in a new family", func(t *testing.T) {
		require.Equal(t, http.StatusOK, login(c, user.Email, user.Password).Status)
		sessions := sessionsOf(t, pool, user.ID)
		require.Len(t, sessions, 2)
		require.NotEqual(t, sessions[0].FamilyID, sessions[1].FamilyID)
		require.NotEqual(t, sessions[0].RefreshHash, sessions[1].RefreshHash)
	})
}

func TestLoginSessionLifetime(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c, pool := setup(t, testkit.Deps{Clock: clock.Fixed{T: now}})

	tests := []struct {
		name       string
		rememberMe bool
		wantTTL    time.Duration
	}{
		{"default is 12 hours", false, 12 * time.Hour},
		{"remember me is 30 days", true, 30 * 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testkit.Reset(t, pool)
			user := testkit.CreateUser(t, pool, testkit.UserOpts{})
			res := c.Do(http.MethodPost, loginPath, map[string]any{
				"identifier": user.Email, "password": user.Password, "rememberMe": tc.rememberMe,
			})
			require.Equal(t, http.StatusOK, res.Status)

			sessions := sessionsOf(t, pool, user.ID)
			require.Len(t, sessions, 1)
			require.Equal(t, tc.rememberMe, sessions[0].RememberMe)
			require.True(t, sessions[0].ExpiresAt.Equal(now.Add(tc.wantTTL)), "expires_at %s", sessions[0].ExpiresAt)

			var body loginBody
			res.JSON(t, &body)
			require.Equal(t, now.Add(15*time.Minute).Format("2006-01-02T15:04:05.000Z"), body.AccessTokenExpiresAt)
		})
	}
}

func TestLoginFailures(t *testing.T) {
	t.Run("wrong password and unknown user are indistinguishable", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})

		wrong := login(c, user.Email, "wrong-password")
		unknown := login(c, "nobody@example.com", "wrong-password")

		require.Equal(t, http.StatusUnauthorized, wrong.Status)
		require.Equal(t, wrong.Status, unknown.Status)
		require.Equal(t, auth.CodeInvalidCredentials, errorOf(t, wrong).Error.Code)
		require.Equal(t, errorOf(t, wrong), errorOf(t, unknown), "same code and message")
		require.Empty(t, sessionsOf(t, pool, user.ID))
	})

	t.Run("a wrong password counts, a right one resets", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})

		for range 3 {
			require.Equal(t, http.StatusUnauthorized, login(c, user.Email, "nope").Status)
		}
		require.Equal(t, 3, lockOf(t, pool, user.ID).Failed)

		require.Equal(t, http.StatusOK, login(c, user.Email, user.Password).Status)
		require.Zero(t, lockOf(t, pool, user.ID).Failed)
	})

	t.Run("disabled is reported only after the correct password", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{Status: db.UsersStatusDISABLED})

		res := login(c, user.Email, user.Password)
		require.Equal(t, http.StatusForbidden, res.Status)
		require.Equal(t, auth.CodeAccountDisabled, errorOf(t, res).Error.Code)
		require.Empty(t, sessionsOf(t, pool, user.ID), "no session for a disabled user")

		res = login(c, user.Email, "wrong-password")
		require.Equal(t, http.StatusUnauthorized, res.Status)
		require.Equal(t, auth.CodeInvalidCredentials, errorOf(t, res).Error.Code, "a wrong password does not reveal it")
	})

	t.Run("a soft-deleted user cannot log in", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		_, err := pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE id = ?", user.ID)
		require.NoError(t, err)

		res := login(c, user.Email, user.Password)
		require.Equal(t, http.StatusUnauthorized, res.Status)
		require.Equal(t, auth.CodeInvalidCredentials, errorOf(t, res).Error.Code)
	})

	t.Run("invalid bodies are VALIDATION_FAILED", func(t *testing.T) {
		c, _ := setup(t, testkit.Deps{})
		bodies := map[string]any{
			"missing password":   map[string]any{"identifier": "a@b.c"},
			"missing identifier": map[string]any{"password": "x"},
			"empty":              map[string]any{},
			"unknown field":      map[string]any{"identifier": "a@b.c", "password": "x", "admin": true},
			"wrong type":         map[string]any{"identifier": 123, "password": "x"},
		}
		for name, body := range bodies {
			t.Run(name, func(t *testing.T) {
				res := c.DoInvalid(http.MethodPost, loginPath, body)
				require.Equal(t, http.StatusBadRequest, res.Status)
				require.Equal(t, "VALIDATION_FAILED", errorOf(t, res).Error.Code)
			})
		}
	})

	t.Run("an enormous password is refused before hashing", func(t *testing.T) {
		c, _ := setup(t, testkit.Deps{})
		huge := make([]byte, 300)
		for i := range huge {
			huge[i] = 'x'
		}
		res := c.DoInvalid(http.MethodPost, loginPath, map[string]any{"identifier": "a@b.c", "password": string(huge)})
		require.Equal(t, http.StatusBadRequest, res.Status)
	})
}

// movableClock lets a test jump forward in time.
type movableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestLockout(t *testing.T) {
	clk := &movableClock{now: time.Now().UTC().Truncate(time.Second)}
	start := clk.Now()
	_, pool := setup(t, testkit.Deps{Clock: clk})
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	// The whole scenario makes more than the 10 logins per minute the rate limiter allows,
	// so each step talks to its own server: same database and clock, fresh rate counter.
	fresh := func() *testkit.Client {
		return testkit.NewClient(t, testkit.NewServer(t, testkit.Deps{DB: pool, Clock: clk}))
	}

	t.Run("five failures, then the sixth attempt is locked even with the right password", func(t *testing.T) {
		c := fresh()
		for i := 1; i <= 5; i++ {
			res := login(c, user.Email, "wrong")
			require.Equal(t, http.StatusUnauthorized, res.Status, "attempt %d", i)
		}
		state := lockOf(t, pool, user.ID)
		require.Equal(t, 5, state.Failed)
		require.True(t, state.LockedUntil.Valid)
		require.True(t, state.LockedUntil.Time.Equal(start.Add(15*time.Minute)), "locked_until %s", state.LockedUntil.Time)

		for _, password := range []string{"wrong", user.Password} {
			res := login(c, user.Email, password)
			require.Equal(t, http.StatusLocked, res.Status)
			require.Equal(t, auth.CodeAccountLocked, errorOf(t, res).Error.Code)
		}
		require.Equal(t, 5, lockOf(t, pool, user.ID).Failed, "attempts during the lock are not counted")
		require.Empty(t, sessionsOf(t, pool, user.ID))
	})

	t.Run("still locked just before the lock ends", func(t *testing.T) {
		clk.Advance(15*time.Minute - time.Second)
		require.Equal(t, http.StatusLocked, login(fresh(), user.Email, user.Password).Status)
	})

	t.Run("the first wrong guess after the lock ends locks again at once", func(t *testing.T) {
		c := fresh()
		clk.Advance(2 * time.Second)
		require.Equal(t, http.StatusUnauthorized, login(c, user.Email, "wrong").Status)
		state := lockOf(t, pool, user.ID)
		require.Equal(t, 6, state.Failed)
		require.True(t, state.LockedUntil.Time.After(clk.Now()))
		require.Equal(t, http.StatusLocked, login(c, user.Email, user.Password).Status)
	})

	t.Run("the right password after the lock ends logs in and resets the counter", func(t *testing.T) {
		clk.Advance(16 * time.Minute)
		require.Equal(t, http.StatusOK, login(fresh(), user.Email, user.Password).Status)
		state := lockOf(t, pool, user.ID)
		require.Zero(t, state.Failed)
		require.False(t, state.LockedUntil.Valid)
	})

	t.Run("failures separated by a success never add up to a lock", func(t *testing.T) {
		testkit.Reset(t, pool)
		u := testkit.CreateUser(t, pool, testkit.UserOpts{})
		c := fresh()
		for range 4 {
			login(c, u.Email, "wrong")
		}
		require.Equal(t, http.StatusOK, login(c, u.Email, u.Password).Status)
		for range 4 {
			require.Equal(t, http.StatusUnauthorized, login(c, u.Email, "wrong").Status)
		}
		require.False(t, lockOf(t, pool, u.ID).LockedUntil.Valid)
	})
}

// TestLockoutCountsConcurrentAttempts backs the claim in RecordFailedLogin: the counter is
// one atomic UPDATE, so simultaneous wrong guesses are all counted. It calls the service
// directly because the HTTP rate limit would stop a burst this size.
func TestLockoutCountsConcurrentAttempts(t *testing.T) {
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{})

	clk := clock.System{}
	service := testkit.AuthService(t, pool, clk, nil)

	// Exactly five attempts: the lock is only set by the fifth increment, so none of them can
	// observe a lock and skip counting, and the outcome is deterministic.
	const attempts = 5
	errs := make([]error, attempts) // one slot per goroutine; require must run on the test goroutine
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			_, errs[i] = service.Login(t.Context(), auth.LoginInput{Identifier: user.Email, Password: "wrong"})
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	}

	state := lockOf(t, pool, user.ID)
	require.Equal(t, attempts, state.Failed, "no lost updates")
	require.True(t, state.LockedUntil.Valid)
}

// TestLoginFailureLimits covers the abuse limits, which count only failed logins: 10 a
// minute per IP and account name, 100 a minute per IP.
func TestLoginFailureLimits(t *testing.T) {
	t.Run("hammering one account name is limited, other names are not", func(t *testing.T) {
		c, _ := setup(t, testkit.Deps{})

		for i := 1; i <= 10; i++ {
			res := login(c, "nobody@example.com", "x")
			require.Equal(t, http.StatusUnauthorized, res.Status, "failure %d is within the limit", i)
		}
		res := login(c, "nobody@example.com", "x")
		require.Equal(t, http.StatusTooManyRequests, res.Status)
		require.Equal(t, "RATE_LIMITED", errorOf(t, res).Error.Code)
		require.Regexp(t, `^([1-9]|[1-5]\d|60)$`, res.Header.Get("Retry-After"), "seconds, within the 1 minute window")

		// The same name in another spelling shares the bucket.
		require.Equal(t, http.StatusTooManyRequests, login(c, "  NOBODY@Example.com", "x").Status)

		// Another account name from the same IP (a colleague on the same wifi) is untouched.
		require.Equal(t, http.StatusUnauthorized, login(c, "someone-else@example.com", "x").Status)
	})

	t.Run("a flood of failures from one IP blocks that IP, even for a valid user", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})

		// A different name each time, so only the per-IP limit can be the one that trips.
		for i := range 100 {
			res := login(c, fmt.Sprintf("nobody%d@example.com", i), "x")
			require.Equal(t, http.StatusUnauthorized, res.Status, "failure %d is within the limit", i+1)
		}
		res := login(c, user.Email, user.Password)
		require.Equal(t, http.StatusTooManyRequests, res.Status, "the right password does not get past a limit")
		require.Empty(t, sessionsOf(t, pool, user.ID))
	})

	t.Run("successful logins never use up the budget", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})

		// Three times the per-account failure limit, all from one IP for one account: a
		// shift change where many people share a device or network.
		for i := 1; i <= 30; i++ {
			res := login(c, user.Email, user.Password)
			require.Equal(t, http.StatusOK, res.Status, "login %d", i)
		}
		require.Equal(t, http.StatusUnauthorized, login(c, user.Email, "wrong").Status, "still not limited")
	})

	t.Run("refused attempts are not counted: locked and disabled accounts", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})

		disabled := testkit.CreateUser(t, pool, testkit.UserOpts{Status: db.UsersStatusDISABLED})
		for i := 1; i <= 12; i++ {
			res := login(c, disabled.Email, disabled.Password)
			require.Equal(t, http.StatusForbidden, res.Status, "attempt %d", i)
		}

		locked := testkit.CreateUser(t, pool, testkit.UserOpts{})
		for range 5 {
			require.Equal(t, http.StatusUnauthorized, login(c, locked.Email, "wrong").Status)
		}
		for i := 1; i <= 12; i++ {
			res := login(c, locked.Email, locked.Password)
			require.Equal(t, http.StatusLocked, res.Status, "attempt %d", i)
		}
	})

	t.Run("the lockout still comes first for a real account", func(t *testing.T) {
		c, pool := setup(t, testkit.Deps{})
		user := testkit.CreateUser(t, pool, testkit.UserOpts{})
		for range 5 {
			require.Equal(t, http.StatusUnauthorized, login(c, user.Email, "wrong").Status)
		}
		require.Equal(t, http.StatusLocked, login(c, user.Email, "wrong").Status, "locked at 5, before the 10 a minute limit")
	})
}

func TestLoginLimitsAreIsolatedPerServerPrefix(t *testing.T) {
	// Each test server gets its own Redis key prefix, so an exhausted limit in one test
	// cannot leak into another.
	first, _ := setup(t, testkit.Deps{})
	for range 11 {
		login(first, "nobody@example.com", "x")
	}
	require.Equal(t, http.StatusTooManyRequests, login(first, "nobody@example.com", "x").Status)

	second, _ := setup(t, testkit.Deps{})
	require.Equal(t, http.StatusUnauthorized, login(second, "nobody@example.com", "x").Status)
}

func TestSeedAdmin(t *testing.T) {
	c, pool := setup(t, testkit.Deps{})
	q := db.New(pool)

	t.Run("creates the first admin, who can log in", func(t *testing.T) {
		created, err := auth.SeedAdmin(t.Context(), q, testkit.Passwords, "  Admin@Example.COM ", "s3cret-password")
		require.NoError(t, err)
		require.True(t, created)

		res := login(c, "admin@example.com", "s3cret-password")
		require.Equal(t, http.StatusOK, res.Status)
		var body loginBody
		res.JSON(t, &body)
		require.Equal(t, "ADMIN", body.User.SystemRole)
		require.Equal(t, "ACTIVE", body.User.Status)
	})

	t.Run("is idempotent", func(t *testing.T) {
		created, err := auth.SeedAdmin(t.Context(), q, testkit.Passwords, "someone-else@example.com", "another-password")
		require.NoError(t, err)
		require.False(t, created)

		var users int
		require.NoError(t, pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users))
		require.Equal(t, 1, users)
	})

	t.Run("refuses a weak password or a bad email", func(t *testing.T) {
		testkit.Reset(t, pool)
		_, err := auth.SeedAdmin(t.Context(), q, testkit.Passwords, "admin@example.com", "short")
		require.Error(t, err)
		_, err = auth.SeedAdmin(t.Context(), q, testkit.Passwords, "not-an-email", "long-enough-password")
		require.Error(t, err)
		var users int
		require.NoError(t, pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&users))
		require.Zero(t, users)
	})

	t.Run("an email already used by a non-admin is an error, not a silent success", func(t *testing.T) {
		testkit.Reset(t, pool)
		member := testkit.CreateUser(t, pool, testkit.UserOpts{})
		created, err := auth.SeedAdmin(t.Context(), q, testkit.Passwords, member.Email, "long-enough-password")
		require.Error(t, err)
		require.False(t, created)
	})
}
