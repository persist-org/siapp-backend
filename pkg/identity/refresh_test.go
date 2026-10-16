package identity_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

const (
	refreshPath = "/auth/refresh"
	logoutPath  = "/auth/logout"
	satu        = int64(personeltest.OrgSatu)
	dua         = int64(personeltest.OrgDua)
)

// refresh sends a request the contract allows, and checks the response against it.
func (e *env) refresh(token string) reply {
	e.t.Helper()
	res := e.client.Do(http.MethodPost, refreshPath, map[string]any{"refresh_token": token})
	e.record(string(res.Body))
	return reply{Status: res.Status, Header: res.Header, Raw: res.Body}
}

// whoami is the status of a protected route with this access token.
func (e *env) whoami(accessToken string) int {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), http.MethodGet, e.srv+whoamiPath, nil)
	require.NoError(e.t, err)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	_ = res.Body.Close()
	return res.StatusCode
}

// logout ends the session of this access token. body nil sends no body at all.
func (e *env) logout(accessToken string, body []byte) reply {
	e.t.Helper()
	return e.postRaw(logoutPath, body, map[string]string{"Authorization": "Bearer " + accessToken})
}

func execTenant(t *testing.T, org int64, query string, args ...any) {
	t.Helper()
	inTenant(t, org, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), query, args...)
		require.NoError(t, err)
	})
}

func sessionOf(t *testing.T, org int64, refreshToken string) int64 {
	t.Helper()
	return scalar[int64](t, org, "SELECT id FROM sessions WHERE refresh_token_hash = $1", auth.HashToken(refreshToken))
}

// liveSessionsOfChain counts the sessions of the chain that holds this refresh token that
// have not been revoked.
func liveSessionsOfChain(t *testing.T, org int64, refreshToken string) int {
	t.Helper()
	return scalar[int](t, org, `
		SELECT count(*) FROM sessions WHERE revoked_at IS NULL AND family_id =
			(SELECT family_id FROM sessions WHERE refresh_token_hash = $1)`, auth.HashToken(refreshToken))
}

func makeStale(t *testing.T, org, personnelID int64) {
	t.Helper()
	execTenant(t, org, "UPDATE personnels SET synced_at = now() - interval '13 hours' WHERE id = $1", personnelID)
}

func TestRefreshRotatesTheToken(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)

	res := e.refresh(first.RefreshToken)
	require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	second := res.data(t)

	require.NotEqual(t, first.RefreshToken, second.RefreshToken)
	require.NotEqual(t, first.AccessToken, second.AccessToken)
	require.True(t, strings.HasPrefix(second.RefreshToken, "900101."))
	require.Equal(t, "Bearer", second.TokenType)
	require.Equal(t, 900, second.ExpiresIn)
	require.Equal(t, first.User.ID, second.User.ID)
	require.Equal(t, identity.MaskNIK(personeltest.NIKAnggota1Satu), second.User.NIK)

	oldID, newID := sessionOf(t, satu, first.RefreshToken), sessionOf(t, satu, second.RefreshToken)
	claims, err := testkit.Tokens(t).ParseAccessToken(second.AccessToken)
	require.NoError(t, err)
	require.Equal(t, newID, claims.SessionID, "the new access token belongs to the new session")

	require.True(t, scalar[bool](t, satu, "SELECT revoked_at IS NOT NULL FROM sessions WHERE id = $1", oldID), "the old session is retired")
	require.Equal(t, newID, scalar[int64](t, satu, "SELECT replaced_by_id FROM sessions WHERE id = $1", oldID))
	require.Nil(t, scalar[*time.Time](t, satu, "SELECT revoked_at FROM sessions WHERE id = $1", newID))
	require.True(t, scalar[bool](t, satu, `
		SELECT a.family_id = b.family_id AND a.remember_me = b.remember_me AND a.client = b.client
		   AND a.device_token_hash = b.device_token_hash
		FROM sessions a, sessions b WHERE a.id = $1 AND b.id = $2`, oldID, newID),
		"the new session keeps the chain, remember_me, client and phone")

	// An access token that was issued before the rotation is not cut off by it: it lives out
	// its 15 minutes, like any access token.
	require.Equal(t, http.StatusOK, e.whoami(first.AccessToken))
	require.Equal(t, http.StatusOK, e.whoami(second.AccessToken))
}

func TestRefreshKeepsRememberMe(t *testing.T) {
	e := newEnv(t, "")
	body := web(personeltest.EmailDanruSatu)
	body["remember_me"] = true
	first := e.login(body).data(t)

	second := e.refresh(first.RefreshToken).data(t)
	seconds := scalar[float64](t, satu, "SELECT EXTRACT(EPOCH FROM expires_at - created_at)::float8 FROM sessions WHERE refresh_token_hash = $1",
		auth.HashToken(second.RefreshToken))
	require.InDelta(t, (30 * 24 * time.Hour).Seconds(), seconds, 1, "a remembered session stays a month")
}

// ⚠ Reuse detection: replaying a retired token is theft until proven otherwise.
func TestReusingARetiredTokenEndsTheWholeChain(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	second := e.refresh(first.RefreshToken).data(t)
	third := e.refresh(second.RefreshToken).data(t)
	require.Equal(t, 1, liveSessionsOfChain(t, satu, third.RefreshToken))

	// The old token comes back, as if a thief had kept a copy.
	replay := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusUnauthorized, replay.Status)
	got := replay.err(t)
	require.Equal(t, "UNAUTHENTICATED", got.Code)
	require.Equal(t, identity.ReasonRefreshReused, got.reason())

	// The revocation committed before the error was returned: nothing in the chain is alive,
	// including the token the honest client was about to use.
	require.Equal(t, 0, liveSessionsOfChain(t, satu, third.RefreshToken))
	require.Equal(t, http.StatusUnauthorized, e.refresh(third.RefreshToken).Status)

	// And their access tokens are cut off at once, not after 15 minutes.
	for _, access := range []string{first.AccessToken, second.AccessToken, third.AccessToken} {
		require.Equal(t, http.StatusUnauthorized, e.whoami(access))
	}
}

// ⚠ Two concurrent refreshes with one token must not both succeed.
func TestParallelRefreshesWithOneTokenSucceedOnce(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)

	const attempts = 8
	var ready, start sync.WaitGroup
	ready.Add(attempts)
	start.Add(1)
	results := make(chan int, attempts)
	for range attempts {
		go func() {
			ready.Done()
			start.Wait() // all of them go at the same moment
			res := e.postTo(refreshPath, map[string]any{"refresh_token": first.RefreshToken}, nil)
			results <- res.Status
		}()
	}
	ready.Wait()
	start.Done()

	ok, refused := 0, 0
	for range attempts {
		switch <-results {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
			refused++
		}
	}
	require.Equal(t, 1, ok, "exactly one refresh wins")
	require.Equal(t, attempts-1, refused)
	// The losers found the token retired, which is reuse: the winner's chain is ended too.
	require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken))
}

func TestRefreshTokensThatCannotWork(t *testing.T) {
	e := newEnv(t, "")
	good := e.login(web(personeltest.EmailDanruSatu)).data(t)
	other := e.login(web(personeltest.EmailDanruDua)).data(t)

	unknown, _, err := auth.NewRefreshToken(satu)
	require.NoError(t, err)
	_, randomPart, _ := strings.Cut(good.RefreshToken, ".")

	tests := map[string]string{
		"not a refresh token":              "garbage",
		"right shape, never issued":        unknown,
		"a token with a zero organization": "0." + randomPart,
		"a token missing its random part":  "900101.",
		// ⚠ Another organization's prefix on a real token: the lookup runs inside that
		// tenant, where the hash does not exist.
		"a real token under another organization": "900102." + randomPart,
	}
	sessionsSatu, sessionsDua := countRows(t, satu, "sessions"), countRows(t, dua, "sessions")
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			res := e.postTo(refreshPath, map[string]any{"refresh_token": token}, nil)
			require.Equal(t, http.StatusUnauthorized, res.Status)
			require.Equal(t, auth.ReasonTokenInvalid, res.err(t).reason())
		})
	}

	// Nothing was touched, and the real tokens still work.
	require.Equal(t, sessionsSatu, countRows(t, satu, "sessions"))
	require.Equal(t, sessionsDua, countRows(t, dua, "sessions"))
	require.Equal(t, 1, liveSessionsOfChain(t, satu, good.RefreshToken))
	require.Equal(t, 1, liveSessionsOfChain(t, dua, other.RefreshToken))
	e.refresh(good.RefreshToken).data(t)
}

func TestRefreshRequestsAreValidated(t *testing.T) {
	e := newEnv(t, "")
	tests := map[string]any{
		"no token":         map[string]any{},
		"an empty token":   map[string]any{"refresh_token": ""},
		"a long token":     map[string]any{"refresh_token": strings.Repeat("x", 129)},
		"an unknown field": map[string]any{"refresh_token": "x", "device_token": "y"},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			res := e.postTo(refreshPath, body, nil)
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
			require.Equal(t, "VALIDATION_ERROR", res.err(t).Code)
		})
	}
}

func TestAnExpiredSessionCannotRefresh(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)
	execTenant(t, satu, "UPDATE sessions SET expires_at = now() - interval '1 minute'")

	res := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusUnauthorized, res.Status)
	require.Equal(t, auth.ReasonTokenInvalid, res.err(t).reason())
}

func TestAnInactivePersonCannotRefresh(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)
	execTenant(t, satu, "UPDATE personnels SET status = 'INACTIVE' WHERE id = $1", first.User.ID)

	res := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusForbidden, res.Status)
	require.Equal(t, identity.ReasonAccountDisabled, res.err(t).reason())
	require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken), "the chain is ended, not just refused")
	require.Equal(t, http.StatusUnauthorized, e.whoami(first.AccessToken))
}

func TestAStalePersonIsRevalidatedWithPersonalia(t *testing.T) {
	t.Run("a fresh copy is not looked up", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(web(personeltest.EmailDanruSatu)).data(t)
		e.refresh(first.RefreshToken).data(t)
		require.Zero(t, e.fake.CallCount("/employees"), "refreshing often must not hammer Personalia")
	})

	t.Run("a stale copy is refreshed from Personalia", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(web(personeltest.EmailDanruSatu)).data(t)
		makeStale(t, satu, first.User.ID)
		e.fake.Rename(personeltest.NIKDanruSatu, "Danru Satu Baru")

		second := e.refresh(first.RefreshToken).data(t)
		require.Equal(t, 1, e.fake.CallCount("/employees"))
		require.Equal(t, "Danru Satu Baru", second.User.FirstName)
		require.Equal(t, "Danru Satu Baru", scalar[string](t, satu, "SELECT full_name FROM personnels WHERE id = $1", first.User.ID))
		require.True(t, scalar[bool](t, satu, "SELECT synced_at > now() - interval '1 minute' FROM personnels WHERE id = $1", first.User.ID))

		// Synced just now, so the next refresh asks nobody.
		e.refresh(second.RefreshToken).data(t)
		require.Equal(t, 1, e.fake.CallCount("/employees"))
	})

	t.Run("a person deactivated in Personalia is refused and stays refused", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(web(personeltest.EmailAnggota2Satu)).data(t)
		makeStale(t, satu, first.User.ID)
		e.fake.Deactivate(personeltest.NIKAnggota2Satu)

		res := e.refresh(first.RefreshToken)
		require.Equal(t, http.StatusForbidden, res.Status)
		require.Equal(t, identity.ReasonAccountDisabled, res.err(t).reason())
		require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", first.User.ID),
			"the finding is kept, so the next refresh needs no Personalia call")
		require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken))
		require.Equal(t, http.StatusUnauthorized, e.whoami(first.AccessToken))
	})

	t.Run("a person who moved to another company is refused", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(web(personeltest.EmailAnggota1Satu)).data(t)
		makeStale(t, satu, first.User.ID)
		e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, personeltest.OrgDua)

		res := e.refresh(first.RefreshToken)
		require.Equal(t, http.StatusForbidden, res.Status)
		require.Equal(t, identity.ReasonAccountDisabled, res.err(t).reason())
	})
}

// ⚠ A Personalia outage must not log the whole field force out.
func TestRefreshSurvivesPersonaliaBeingDown(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)
	makeStale(t, satu, first.User.ID)
	e.fake.Down()

	second := e.refresh(first.RefreshToken).data(t)
	require.Equal(t, "ACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", first.User.ID),
		"the cached status is kept")
	require.True(t, scalar[bool](t, satu, "SELECT synced_at < now() - interval '12 hours' FROM personnels WHERE id = $1", first.User.ID),
		"and it stays stale, so the next refresh tries again")
	require.NotEmpty(t, second.AccessToken)

	logs := e.logs.String()
	require.Contains(t, logs, "could not re-validate")
	require.NotContains(t, logs, personeltest.NIKDanruSatu, "the NIK is personal data")

	e.fake.Up()
	e.refresh(second.RefreshToken).data(t)
	require.Equal(t, 1, scalar[int](t, satu, "SELECT count(*) FROM personnels WHERE synced_at > now() - interval '1 minute'"))
}

func TestTheBoundPhoneRefreshesAndAnotherPhoneDoesNot(t *testing.T) {
	t.Run("the same phone refreshes and is noted as seen", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
		execTenant(t, satu, "UPDATE device_bindings SET last_seen_at = now() - interval '1 day'")

		e.refresh(first.RefreshToken).data(t)
		require.True(t, scalar[bool](t, satu, "SELECT last_seen_at > now() - interval '1 minute' FROM device_bindings"))
	})

	t.Run("a reset binding and another phone ends the first phone's chain", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
		// An admin frees the phone slot (SiAPP's own reset, Phase 01.3), and the person signs
		// in on a new phone, which binds.
		execTenant(t, satu, "DELETE FROM device_bindings WHERE personnel_id = $1", first.User.ID)
		onB := e.login(mobile(personeltest.EmailAnggota1Satu, phoneB)).data(t)

		res := e.refresh(first.RefreshToken)
		require.Equal(t, http.StatusUnauthorized, res.Status)
		got := res.err(t)
		require.Equal(t, "UNAUTHENTICATED", got.Code)
		require.Equal(t, identity.ReasonDeviceChanged, got.reason())
		require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken), "the first phone's chain is revoked")
		require.Equal(t, http.StatusUnauthorized, e.whoami(first.AccessToken), "and its access token is cut off")

		// The new phone is unaffected.
		e.refresh(onB.RefreshToken).data(t)
	})

	t.Run("a binding that is simply gone ends the chain too", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
		execTenant(t, satu, "DELETE FROM device_bindings")

		res := e.refresh(first.RefreshToken)
		require.Equal(t, http.StatusUnauthorized, res.Status)
		require.Equal(t, identity.ReasonDeviceChanged, res.err(t).reason())
		require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken))
	})

	t.Run("a web session has no phone to check", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.login(web(personeltest.EmailAnggota1Satu)).data(t)
		e.refresh(first.RefreshToken).data(t)
	})
}

// ⚠ The device check is a local read: it never calls Personalia, so it works while
// Personalia is down and fails closed all the same.
func TestTheDeviceCheckNeverCallsPersonalia(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	e.fake.Down()
	calls := len(e.fake.Calls())

	second := e.refresh(first.RefreshToken).data(t)
	require.Equal(t, calls, len(e.fake.Calls()), "a fresh person and a bound phone need no call at all")

	execTenant(t, satu, "DELETE FROM device_bindings")
	res := e.refresh(second.RefreshToken)
	require.Equal(t, http.StatusUnauthorized, res.Status)
	require.Equal(t, identity.ReasonDeviceChanged, res.err(t).reason(), "refused even with Personalia down")
}

func TestLogoutEndsTheChainAndCutsOffTheAccessTokens(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
	second := e.refresh(first.RefreshToken).data(t)
	require.Equal(t, http.StatusOK, e.whoami(second.AccessToken))

	res := e.logout(second.AccessToken, nil)
	require.Equal(t, http.StatusOK, res.Status)

	// At once, everywhere: the token that logged out, and the one from before the refresh.
	require.Equal(t, http.StatusUnauthorized, e.whoami(second.AccessToken))
	require.Equal(t, http.StatusUnauthorized, e.whoami(first.AccessToken), "the whole chain, not just this session")
	// The refresh token is dead as well.
	require.Equal(t, 0, liveSessionsOfChain(t, satu, second.RefreshToken))
	require.Equal(t, http.StatusUnauthorized, e.refresh(second.RefreshToken).Status)

	// It does not unbind the phone: the binding is the admin's record.
	require.Equal(t, 1, countRows(t, satu, "device_bindings"))
	// So the person signs in again on the same phone.
	e.login(mobile(personeltest.EmailAnggota1Satu, phoneA)).data(t)
}

func TestLogoutDetails(t *testing.T) {
	e := newEnv(t, "")
	token := func() string { return e.login(web(personeltest.EmailDanruSatu)).data(t).AccessToken }

	t.Run("a repeat with the same token is refused, the first already cut it off", func(t *testing.T) {
		access := token()
		require.Equal(t, http.StatusOK, e.logout(access, nil).Status)
		require.Equal(t, http.StatusUnauthorized, e.logout(access, nil).Status)
	})

	t.Run("it needs a token", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, e.postRaw(logoutPath, nil, nil).Status)
	})

	t.Run("the optional body carries the firebase token", func(t *testing.T) {
		res := e.logout(token(), []byte(`{"firebase_token":"fcm-token-123"}`))
		require.Equal(t, http.StatusOK, res.Status)
	})

	t.Run("a body that is not JSON is refused and ends nothing", func(t *testing.T) {
		access := token()
		require.Equal(t, http.StatusBadRequest, e.logout(access, []byte(`{nope`)).Status)
		require.Equal(t, http.StatusOK, e.whoami(access))
	})

	t.Run("it ends only its own chain", func(t *testing.T) {
		mine, theirs := token(), token() // two logins of one person: two chains
		require.Equal(t, http.StatusOK, e.logout(mine, nil).Status)
		require.Equal(t, http.StatusOK, e.whoami(theirs))
	})

	t.Run("the response is the contract's envelope with no data", func(t *testing.T) {
		e.client.Token = token()
		t.Cleanup(func() { e.client.Token = "" })
		res := e.client.Do(http.MethodPost, logoutPath, nil)
		require.Equal(t, http.StatusOK, res.Status)
		require.Contains(t, string(res.Body), `"data":null`)
	})
}

func TestARevokedSessionIsCutOffAcrossOrganizations(t *testing.T) {
	// Revocation is by session id, and ids are unique across tenants, so ending one
	// organization's session can never cut off another's token.
	e := newEnv(t, "")
	inSatu := e.login(web(personeltest.EmailDanruSatu)).data(t)
	inDua := e.login(web(personeltest.EmailDanruDua)).data(t)

	require.Equal(t, http.StatusOK, e.logout(inSatu.AccessToken, nil).Status)
	require.Equal(t, http.StatusOK, e.whoami(inDua.AccessToken))
}
