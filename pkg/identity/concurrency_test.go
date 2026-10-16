package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// async makes a request on another goroutine and hands back what happened.
//
// ⚠ Nothing on that goroutine may call require: a failed assertion there ends only that
// goroutine (runtime.Goexit), never sends on the channel, and the test then waits forever
// instead of failing. So the request reports its outcome, and the test asserts on it.
func (e *env) async(method, path, token string, body any) <-chan asyncResult {
	out := make(chan asyncResult, 1)
	go func() {
		var reader io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			if err != nil {
				out <- asyncResult{err: err}
				return
			}
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(context.Background(), method, e.srv+"/api/v1"+path, reader)
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- asyncResult{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		raw, err := io.ReadAll(res.Body)
		out <- asyncResult{reply: reply{Status: res.StatusCode, Header: res.Header, Raw: raw}, err: err}
	}()
	return out
}

type asyncResult struct {
	reply
	err error
}

// await waits for an async request, with a deadline so that a stuck request fails the test.
func await(t *testing.T, ch <-chan asyncResult) reply {
	t.Helper()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.reply
	case <-time.After(30 * time.Second):
		t.Fatal("the request did not finish within 30 s")
		return reply{}
	}
}

// openTenantTx starts a transaction of the app role in an organization and leaves it open, so
// a test can hold locks while it starts a request that has to wait for them.
func openTenantTx(t *testing.T, org int64) pgx.Tx {
	t.Helper()
	tx, err := testkit.Pool(t).Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	_, err = tx.Exec(t.Context(), "SELECT set_config('app.current_org', $1, true)", strconv.FormatInt(org, 10))
	require.NoError(t, err)
	return tx
}

// waitUntilBlocked waits until some backend is waiting for a lock, i.e. the request the test
// started has reached the lock the test holds.
func waitUntilBlocked(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, testkit.OwnerPool(t).QueryRow(t.Context(),
			"SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the request never waited for the lock the test holds")
}

// refreshInFlight plays a refresh of the chain that holds refreshToken, stopped just before
// its commit: it holds the person lock (as Refresh does), has created the next session of
// the chain and retired the old one. Committing the returned transaction finishes it.
func refreshInFlight(t *testing.T, org, personnelID int64, refreshToken string) pgx.Tx {
	t.Helper()
	oldID := sessionOf(t, org, refreshToken)
	tx := openTenantTx(t, org)
	_, err := tx.Exec(t.Context(), "SELECT id FROM personnels WHERE id = $1 FOR UPDATE", personnelID)
	require.NoError(t, err)
	var nextID int64
	require.NoError(t, tx.QueryRow(t.Context(), `
		INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, device_token_hash, client, expires_at)
		SELECT personnel_id, family_id, md5(random()::text) || md5(random()::text), device_token_hash, client,
		       now() + interval '1 day'
		FROM sessions WHERE id = $1
		RETURNING id`, oldID).Scan(&nextID))
	_, err = tx.Exec(t.Context(),
		"UPDATE sessions SET revoked_at = now(), revoke_reason = 'ROTATED', replaced_by_id = $2 WHERE id = $1", oldID, nextID)
	require.NoError(t, err)
	return tx
}

// ⚠ A logout that races a refresh of the same chain must end the session the refresh
// creates. Without the per-person lock its UPDATE would wait for the old row, then miss the
// new one (it is not in the statement's snapshot), and the chain would survive the logout.
func TestLogoutEndsTheSessionAnInFlightRefreshCreates(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	tx := refreshInFlight(t, satu, phone.User.ID, phone.RefreshToken)

	done := e.async(http.MethodPost, logoutPath, phone.AccessToken, nil)
	waitUntilBlocked(t)
	require.NoError(t, tx.Commit(t.Context()))

	require.Equal(t, http.StatusOK, await(t, done).Status)
	require.Equal(t, 0, scalar[int](t, satu, "SELECT count(*) FROM sessions WHERE revoked_at IS NULL"),
		"no session of the chain survives the logout")
}

// ⚠ The same race for the admin device reset: the MOBILE session a refresh creates while the
// reset runs must be revoked too.
func TestDeviceResetEndsTheSessionAnInFlightRefreshCreates(t *testing.T) {
	e := newEnv(t, "")
	admin := e.adminToken(personeltest.EmailDanruSatu)
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	tx := refreshInFlight(t, satu, phone.User.ID, phone.RefreshToken)

	done := e.async(http.MethodDelete, "/admin/personnel/"+itoa(phone.User.ID)+"/device-binding", admin, nil)
	waitUntilBlocked(t)
	require.NoError(t, tx.Commit(t.Context()))

	require.Equal(t, http.StatusOK, await(t, done).Status)
	require.Equal(t, 0, scalar[int](t, satu,
		"SELECT count(*) FROM sessions WHERE personnel_id = $1 AND client = 'MOBILE' AND revoked_at IS NULL", phone.User.ID))
}

// ⚠ Two people registering the same push token at the same moment: the second insert trips
// the unique index. It must take the token over, not answer 500.
func TestRegisteringAPushTokenAnotherRegistrationHoldsTakesItOver(t *testing.T) {
	e := newEnv(t, "")
	alice := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	bob := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)

	// Alice's registration of the token, not yet committed.
	tx := openTenantTx(t, satu)
	_, err := tx.Exec(t.Context(), `
		INSERT INTO devices (personnel_id, session_id, platform, push_token)
		VALUES ($1, $2, 'ANDROID', 'fcm-shared')`, alice.User.ID, sessionOf(t, satu, alice.RefreshToken))
	require.NoError(t, err)

	done := e.async(http.MethodPost, tokenPath, bob.AccessToken, map[string]any{"firebase_token": "fcm-shared", "platform": "android"})
	waitUntilBlocked(t) // Bob's insert waits on the unique index entry of Alice's row
	require.NoError(t, tx.Commit(t.Context()))

	res := await(t, done)
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	devices := devicesOf(t, satu)
	require.Len(t, devices, 1)
	require.Equal(t, sessionOf(t, satu, bob.RefreshToken), devices[0].SessionID, "the later registration owns the token")
}

// A NIK that Personalia has handed to another account (a re-hire under a new user) must not
// make the new owner's login fail on the unique (organization, nik) index.
func TestANIKMovedToAnotherAccountDoesNotBreakLogin(t *testing.T) {
	e := newEnv(t, "")
	stale := e.fieldSession(personeltest.EmailAnggota2Satu, phoneA)
	// The cached row of another account still holds the NIK that now belongs to anggota1.
	execTenant(t, satu, "UPDATE personnels SET nik = $1 WHERE id = $2", personeltest.NIKAnggota1Satu, stale.User.ID)

	res := e.login(mobile(personeltest.EmailAnggota1Satu, phoneB))
	require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
	owner := res.data(t)
	require.Equal(t, personeltest.NIKAnggota1Satu, scalar[string](t, satu, "SELECT nik FROM personnels WHERE id = $1", owner.User.ID))
	require.Nil(t, scalar[*string](t, satu, "SELECT nik FROM personnels WHERE id = $1", stale.User.ID), "the stale row let go of it")
}
