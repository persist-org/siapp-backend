package auth_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/mail"
	"siapp/internal/platform/apperr"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/testkit"
)

const (
	forgotPath = "/api/v1/auth/forgot-password"
	resetPath  = "/api/v1/auth/reset-password"
)

func forgot(c *testkit.Client, email string) *testkit.Response {
	return c.Do(http.MethodPost, forgotPath, map[string]any{"email": email})
}

func reset(c *testkit.Client, token, password string) *testkit.Response {
	return c.Do(http.MethodPost, resetPath, map[string]any{"token": token, "newPassword": password})
}

type messageBody struct {
	Message string `json:"message"`
}

func TestForgotPassword(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{Email: "budi.santoso@example.com", FullName: "Budi Santoso"})

	t.Run("a known email gets a link, and the answer is 202", func(t *testing.T) {
		res := forgot(e.anon, user.Email)
		require.Equal(t, http.StatusAccepted, res.Status)
		var body messageBody
		res.JSON(t, &body)
		require.NotEmpty(t, body.Message)

		sent := e.mailer.Sent()
		require.Len(t, sent, 1)
		require.Equal(t, user.Email, sent[0].To)
		require.Equal(t, "Atur ulang kata sandi SiAPP", sent[0].Subject)
		require.Contains(t, sent[0].Text, "Budi Santoso")
		require.Contains(t, sent[0].Text, testkit.DefaultResetURL+"?token=")
		require.Contains(t, sent[0].HTML, "Budi Santoso")
	})

	t.Run("only a hash of the token is stored", func(t *testing.T) {
		token := resetTokenOf(t, e.mailer.Sent()[0])
		sum := sha256.Sum256([]byte(token))

		var hash string
		var expiresAt time.Time
		var usedAt sql.NullTime
		require.NoError(t, e.pool.QueryRowContext(t.Context(),
			"SELECT token_hash, expires_at, used_at FROM password_reset_tokens WHERE user_id = ?", user.ID).
			Scan(&hash, &expiresAt, &usedAt))
		require.Equal(t, hex.EncodeToString(sum[:]), hash)
		require.NotContains(t, hash, token)
		require.False(t, usedAt.Valid)
		require.WithinDuration(t, time.Now().Add(time.Hour), expiresAt, time.Minute, "valid for one hour")
	})

	t.Run("an unknown email gets exactly the same answer as a known one, and no mail", func(t *testing.T) {
		before := len(e.mailer.Sent())
		unknown := forgot(e.anon, "nobody@example.com")
		require.Equal(t, http.StatusAccepted, unknown.Status)
		require.Len(t, e.mailer.Sent(), before, "nothing is sent to an address without an account")

		known := forgot(e.anon, user.Email)
		require.Equal(t, unknown.Status, known.Status)
		require.Equal(t, unknown.Body, known.Body, "the body cannot tell the two cases apart")
	})

	t.Run("the email is matched without regard to case and spaces", func(t *testing.T) {
		before := len(e.mailer.Sent())
		require.Equal(t, http.StatusAccepted, forgot(e.anon, "  Budi.Santoso@EXAMPLE.com ").Status)
		require.Len(t, e.mailer.Sent(), before+1)
	})

	t.Run("a malformed email is VALIDATION_FAILED", func(t *testing.T) {
		for _, body := range []map[string]any{{"email": "nope"}, {}, {"email": ""}, {"email": "a@b.c", "extra": 1}} {
			requireError(t, e.anon.DoInvalid(http.MethodPost, forgotPath, body), http.StatusBadRequest, "VALIDATION_FAILED")
		}
	})
}

func TestForgotPasswordSendsNothingForAccountsThatCannotReset(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	disabled := testkit.CreateUser(t, e.pool, testkit.UserOpts{Status: "DISABLED"})
	deleted := testkit.CreateUser(t, e.pool, testkit.UserOpts{})
	_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET deleted_at = NOW(3) WHERE id = ?", deleted.ID)
	require.NoError(t, err)

	for _, email := range []string{disabled.Email, deleted.Email} {
		require.Equal(t, http.StatusAccepted, forgot(e.anon, email).Status, "the answer does not reveal the state of the account")
	}
	require.Empty(t, e.mailer.Sent())
	var tokens int
	require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM password_reset_tokens").Scan(&tokens))
	require.Zero(t, tokens)
}

func TestForgotPasswordSurvivesAMailFailure(t *testing.T) {
	failing := mail.NewFakeSender()
	failing.Err = errors.New("smtp is down")
	e := startEnv(t, testkit.Deps{Mailer: failing})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{})

	res := forgot(e.anon, user.Email)
	require.Equal(t, http.StatusAccepted, res.Status, "a delivery failure must not turn into an error the caller can see")

	var tokens int
	require.NoError(t, e.pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM password_reset_tokens WHERE user_id = ?", user.ID).Scan(&tokens))
	require.Equal(t, 1, tokens)
}

func TestForgotPasswordLimits(t *testing.T) {
	// Every request counts, whether or not the address has an account, so being limited tells
	// an attacker nothing about which addresses exist.
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("3 requests per address, then 429 (account exists: %v)", known), func(t *testing.T) {
			e := startEnv(t, testkit.Deps{})
			email := "nobody@example.com"
			if known {
				email = testkit.CreateUser(t, e.pool, testkit.UserOpts{}).Email
			}
			for i := 1; i <= 3; i++ {
				require.Equal(t, http.StatusAccepted, forgot(e.anon, email).Status, "request %d", i)
			}
			res := forgot(e.anon, email)
			require.Equal(t, http.StatusTooManyRequests, res.Status)
			require.Regexp(t, `^[1-9]\d*$`, res.Header.Get("Retry-After"))
		})
	}

	t.Run("the same address in another spelling shares the allowance", func(t *testing.T) {
		e := startEnv(t, testkit.Deps{})
		for range 3 {
			forgot(e.anon, "someone@example.com")
		}
		require.Equal(t, http.StatusTooManyRequests, forgot(e.anon, "  SOMEONE@Example.com").Status)
	})

	t.Run("one address being limited does not affect another from the same IP", func(t *testing.T) {
		e := startEnv(t, testkit.Deps{})
		for range 4 {
			forgot(e.anon, "victim@example.com")
		}
		require.Equal(t, http.StatusAccepted, forgot(e.anon, "colleague@example.com").Status)
	})

	t.Run("an IP is limited after 30 requests, for any addresses", func(t *testing.T) {
		e := startEnv(t, testkit.Deps{})
		for i := range 30 {
			require.Equal(t, http.StatusAccepted, forgot(e.anon, fmt.Sprintf("person%d@example.com", i)).Status, "request %d", i+1)
		}
		res := forgot(e.anon, "person-31@example.com")
		require.Equal(t, http.StatusTooManyRequests, res.Status)
		require.Regexp(t, `^[1-9]\d*$`, res.Header.Get("Retry-After"))
	})

	t.Run("a request refused by the IP limit does not use up the address's allowance", func(t *testing.T) {
		e := startEnv(t, testkit.Deps{})
		for i := range 30 {
			forgot(e.anon, fmt.Sprintf("p%d@example.com", i))
		}
		for range 5 {
			require.Equal(t, http.StatusTooManyRequests, forgot(e.anon, "target@example.com").Status)
		}
		// The address was never counted, so its allowance is intact for the moment the IP
		// limit lifts. (Checked through Redis rather than by waiting 15 minutes.)
		rdb := testkit.Redis(t)
		sum := sha256.Sum256([]byte("target@example.com"))
		keys, err := rdb.Keys(t.Context(), "*forgot-email:"+hex.EncodeToString(sum[:])).Result()
		require.NoError(t, err)
		require.Empty(t, keys)
	})
}

func TestNewLinkReplacesTheOldOne(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{})

	forgot(e.anon, user.Email)
	forgot(e.anon, user.Email)
	sent := e.mailer.Sent()
	require.Len(t, sent, 2)
	first, second := resetTokenOf(t, sent[0]), resetTokenOf(t, sent[1])
	require.NotEqual(t, first, second)

	requireError(t, reset(e.anon, first, "BrandNewPass1"), http.StatusBadRequest, "AUTH_RESET_INVALID")
	require.Equal(t, http.StatusOK, reset(e.anon, second, "BrandNewPass1").Status)
}

func TestResetPassword(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{Email: "reset.me@example.com", Password: "OldPassword1"})

	// The user is logged in on two devices, and has locked themselves out by mistyping.
	phone := loginAs(t, e.anon, user, false)
	tablet := loginAs(t, e.anon, user, false)
	require.Equal(t, http.StatusOK, statusOf(t, e.srv, phone.AccessToken))
	_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET failed_login_count = 5, locked_until = DATE_ADD(NOW(3), INTERVAL 10 MINUTE) WHERE id = ?", user.ID)
	require.NoError(t, err)
	requireError(t, login(e.anon, user.Email, "OldPassword1"), http.StatusLocked, "AUTH_ACCOUNT_LOCKED")

	forgot(e.anon, user.Email)
	token := resetTokenOf(t, e.mailer.Sent()[0])

	res := reset(e.anon, token, "NewPassw0rd!")
	require.Equal(t, http.StatusOK, res.Status, "%s", res.Body)
	var body messageBody
	res.JSON(t, &body)
	require.NotEmpty(t, body.Message)

	t.Run("the new password works and the old one does not", func(t *testing.T) {
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "NewPassw0rd!").Status)
		requireError(t, login(e.anon, user.Email, "OldPassword1"), http.StatusUnauthorized, "AUTH_INVALID_CREDENTIALS")
	})

	t.Run("the lockout is cleared", func(t *testing.T) {
		state := lockOf(t, e.pool, user.ID)
		require.False(t, state.LockedUntil.Valid)
	})

	t.Run("every session is ended, refresh tokens and access tokens alike", func(t *testing.T) {
		requireError(t, doRefresh(e.anon, phone.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)
		requireError(t, doRefresh(e.anon, tablet.RefreshToken), http.StatusUnauthorized, auth.CodeRefreshReused)
		require.Equal(t, http.StatusUnauthorized, statusOf(t, e.srv, phone.AccessToken))
		require.Equal(t, http.StatusUnauthorized, statusOf(t, e.srv, tablet.AccessToken))
	})

	t.Run("the link works only once", func(t *testing.T) {
		requireError(t, reset(e.anon, token, "AnotherPassw0rd"), http.StatusBadRequest, "AUTH_RESET_INVALID")
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "NewPassw0rd!").Status, "the second attempt changed nothing")
	})

	t.Run("it is audited, by the user, with no secret", func(t *testing.T) {
		rows := auditRows(t, e.pool, "user.password.reset")
		require.Len(t, rows, 1)
		require.Equal(t, user.ID, rows[0].ActorID.String)
		require.Equal(t, user.ID, rows[0].EntityID)
		require.False(t, rows[0].Metadata.Valid, "nothing to record beyond who and when")
	})
}

func TestResetPasswordRules(t *testing.T) {
	e := startEnv(t, testkit.Deps{})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{Email: "budi.santoso@example.com", Username: "budi_s", Password: "OldPassword1"})

	forgot(e.anon, user.Email)
	token := resetTokenOf(t, e.mailer.Sent()[0])

	// The username rule is covered in the unit test, where a username long enough to pass the
	// length rule can be used without changing this account.
	weak := map[string]string{
		"too short":                  "abc1234",
		"the email name":             "budi.santoso",
		"the email name in capitals": "BUDI.SANTOSO",
	}

	for name, password := range weak {
		t.Run("refuses "+name, func(t *testing.T) {
			requireError(t, reset(e.anon, token, password), http.StatusBadRequest, auth.CodePasswordWeak)
		})
	}

	t.Run("a refused password does not use the link up", func(t *testing.T) {
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "OldPassword1").Status, "the password did not change")
		require.Equal(t, http.StatusOK, reset(e.anon, token, "GoodPassw0rd!").Status, "the same link still works")
	})

	t.Run("a birthdate-style password is allowed", func(t *testing.T) {
		forgot(e.anon, user.Email)
		next := resetTokenOf(t, e.mailer.Sent()[len(e.mailer.Sent())-1])
		require.Equal(t, http.StatusOK, reset(e.anon, next, "10051994").Status, "it is the default password, so it cannot be forbidden")
		require.Equal(t, http.StatusOK, login(e.anon, user.Email, "10051994").Status)
	})
}

func TestResetPasswordRejectsBadTokens(t *testing.T) {
	clk := &movableClock{now: time.Now().UTC().Truncate(time.Second)}
	e := startEnv(t, testkit.Deps{Clock: clk})
	user := testkit.CreateUser(t, e.pool, testkit.UserOpts{})

	t.Run("an unknown token", func(t *testing.T) {
		requireError(t, reset(e.anon, "this-token-was-never-issued", "GoodPassw0rd!"), http.StatusBadRequest, auth.CodeResetInvalid)
	})

	t.Run("invalid bodies", func(t *testing.T) {
		for _, body := range []map[string]any{
			{}, {"token": "x"}, {"newPassword": "GoodPassw0rd!"}, {"token": "", "newPassword": "GoodPassw0rd!"},
			{"token": "x", "newPassword": "GoodPassw0rd!", "extra": 1}, {"token": strings.Repeat("x", 300), "newPassword": "GoodPassw0rd!"},
			{"token": "x", "newPassword": strings.Repeat("p", 300)},
		} {
			requireError(t, e.anon.DoInvalid(http.MethodPost, resetPath, body), http.StatusBadRequest, "VALIDATION_FAILED")
		}
	})

	t.Run("a link is valid for one hour, to the last second", func(t *testing.T) {
		forgot(e.anon, user.Email)
		token := resetTokenOf(t, e.mailer.Sent()[0])

		clk.Advance(time.Hour - time.Second)
		// Checked without consuming it: a weak password is refused after the token is judged.
		requireError(t, reset(e.anon, token, "short"), http.StatusBadRequest, auth.CodePasswordWeak)

		clk.Advance(2 * time.Second)
		requireError(t, reset(e.anon, token, "GoodPassw0rd!"), http.StatusBadRequest, auth.CodeResetInvalid)
	})

	t.Run("a link for an account that has been disabled since", func(t *testing.T) {
		forgot(e.anon, user.Email)
		token := resetTokenOf(t, e.mailer.Sent()[len(e.mailer.Sent())-1])
		_, err := e.pool.ExecContext(t.Context(), "UPDATE users SET status = 'DISABLED' WHERE id = ?", user.ID)
		require.NoError(t, err)
		requireError(t, reset(e.anon, token, "GoodPassw0rd!"), http.StatusBadRequest, auth.CodeResetInvalid)
	})
}

// TestParallelResetExactlyOneSucceeds backs the FOR UPDATE on the token row.
func TestParallelResetExactlyOneSucceeds(t *testing.T) {
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{Password: "OldPassword1"})

	mailer := mail.NewFakeSender()
	service := testkit.AuthService(t, pool, nil, mailer)
	require.NoError(t, service.RequestPasswordReset(t.Context(), user.Email))
	token := resetTokenOf(t, mailer.Sent()[0])

	const callers = 6
	passwords := make([]string, callers)
	errs := make([]error, callers) // one slot per goroutine; require must run on the test goroutine
	var wg sync.WaitGroup
	for i := range callers {
		passwords[i] = fmt.Sprintf("Candidate-%d-Passw0rd", i)
		wg.Go(func() { errs[i] = service.ResetPassword(t.Context(), token, passwords[i]) })
	}
	wg.Wait()

	winner := -1
	for i, err := range errs {
		if err == nil {
			require.Equal(t, -1, winner, "two resets succeeded")
			winner = i
			continue
		}
		var appErr *apperr.Error
		require.ErrorAs(t, err, &appErr)
		require.Equal(t, auth.CodeResetInvalid, appErr.Code)
	}
	require.NotEqual(t, -1, winner, "one reset must win")

	// The password is the winner's, and only the winner's.
	var hash string
	require.NoError(t, pool.QueryRowContext(t.Context(), "SELECT password_hash FROM users WHERE id = ?", user.ID).Scan(&hash))
	for i, p := range passwords {
		ok, err := testkit.Passwords.Verify(p, hash)
		require.NoError(t, err)
		require.Equal(t, i == winner, ok, "candidate %d", i)
	}
}
