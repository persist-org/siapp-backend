package identity_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel/personeltest"
)

const tokenPath = "/notifications/token"

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func register(e *env, access, token string) reply {
	e.t.Helper()
	return e.api(access, http.MethodPost, tokenPath, map[string]any{"firebase_token": token, "platform": "android", "app_version": "1.4.2"})
}

func unregister(e *env, access, token string) reply {
	e.t.Helper()
	return e.api(access, http.MethodDelete, tokenPath, map[string]any{"firebase_token": token})
}

type deviceRow struct {
	SessionID  int64
	Platform   string
	PushToken  string
	AppVersion string
}

func devicesOf(t *testing.T, org int64) []deviceRow {
	t.Helper()
	var out []deviceRow
	inTenant(t, org, func(tx pgx.Tx) {
		rows, err := tx.Query(t.Context(), "SELECT session_id, platform::text, push_token, COALESCE(app_version, '') FROM devices ORDER BY id")
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var d deviceRow
			require.NoError(t, rows.Scan(&d.SessionID, &d.Platform, &d.PushToken, &d.AppVersion))
			out = append(out, d)
		}
		require.NoError(t, rows.Err())
	})
	return out
}

func TestRegisteringTheSamePushTokenTwiceUpdatesOneRow(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	require.Equal(t, http.StatusOK, register(e, phone.AccessToken, "fcm-1").Status)
	require.Equal(t, http.StatusOK, register(e, phone.AccessToken, "fcm-1").Status)
	devices := devicesOf(t, satu)
	require.Len(t, devices, 1)
	require.Equal(t, deviceRow{SessionID: sessionOf(t, satu, phone.RefreshToken), Platform: "ANDROID", PushToken: "fcm-1", AppVersion: "1.4.2"}, devices[0])

	// A new token for the same install replaces the old one, still one row.
	require.Equal(t, http.StatusOK, register(e, phone.AccessToken, "fcm-2").Status)
	devices = devicesOf(t, satu)
	require.Len(t, devices, 1)
	require.Equal(t, "fcm-2", devices[0].PushToken)

	// The response is the contract's envelope with no data.
	require.Contains(t, string(register(e, phone.AccessToken, "fcm-2").Raw), `"data":null`)
}

func TestARefreshMovesTheDeviceToTheNewSession(t *testing.T) {
	e := newEnv(t, "")
	first := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	require.Equal(t, http.StatusOK, register(e, first.AccessToken, "fcm-1").Status)

	second := e.refresh(first.RefreshToken).data(t)
	devices := devicesOf(t, satu)
	require.Len(t, devices, 1, "the app does not register again after a refresh")
	require.Equal(t, sessionOf(t, satu, second.RefreshToken), devices[0].SessionID)

	// An access token issued before the refresh names the retired session; the device must
	// still land on the live one.
	require.Equal(t, http.StatusOK, register(e, first.AccessToken, "fcm-2").Status)
	devices = devicesOf(t, satu)
	require.Len(t, devices, 1)
	require.Equal(t, sessionOf(t, satu, second.RefreshToken), devices[0].SessionID)
	require.Equal(t, "fcm-2", devices[0].PushToken)
}

func TestAPushTokenBelongsToOneDevice(t *testing.T) {
	e := newEnv(t, "")
	alice := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	bob := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)

	require.Equal(t, http.StatusOK, register(e, alice.AccessToken, "fcm-shared").Status)
	require.Len(t, devicesOf(t, satu), 1)

	// The phone changed hands: Bob registers the same token, and Alice loses it.
	require.Equal(t, http.StatusOK, register(e, bob.AccessToken, "fcm-shared").Status)
	devices := devicesOf(t, satu)
	require.Len(t, devices, 1)
	require.Equal(t, sessionOf(t, satu, bob.RefreshToken), devices[0].SessionID)
}

func TestADeviceNeverOutlivesItsLogin(t *testing.T) {
	t.Run("logout deletes it", func(t *testing.T) {
		e := newEnv(t, "")
		phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		register(e, phone.AccessToken, "fcm-1")
		require.Len(t, devicesOf(t, satu), 1)

		require.Equal(t, http.StatusOK, e.logout(phone.AccessToken, nil).Status)
		require.Empty(t, devicesOf(t, satu))
	})

	t.Run("a replayed refresh token ends the chain and its devices", func(t *testing.T) {
		e := newEnv(t, "")
		first := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		second := e.refresh(first.RefreshToken).data(t)
		register(e, second.AccessToken, "fcm-1")
		require.Len(t, devicesOf(t, satu), 1)

		require.Equal(t, http.StatusUnauthorized, e.refresh(first.RefreshToken).Status) // the replay
		require.Empty(t, devicesOf(t, satu), "a stolen chain must stop receiving notifications")
	})

	t.Run("the row goes with its session", func(t *testing.T) {
		e := newEnv(t, "")
		phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		register(e, phone.AccessToken, "fcm-1")
		execTenant(t, satu, "DELETE FROM sessions")
		require.Empty(t, devicesOf(t, satu))
	})

	t.Run("it cannot be registered after logout", func(t *testing.T) {
		e := newEnv(t, "")
		phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
		require.Equal(t, http.StatusOK, e.logout(phone.AccessToken, nil).Status)
		require.Equal(t, http.StatusUnauthorized, register(e, phone.AccessToken, "fcm-1").Status)
	})
}

func TestUnregisteringADevice(t *testing.T) {
	e := newEnv(t, "")
	alice := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	bob := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)
	register(e, alice.AccessToken, "fcm-alice")

	// Bob cannot delete Alice's token: it is not on his chain.
	require.Equal(t, http.StatusOK, unregister(e, bob.AccessToken, "fcm-alice").Status)
	require.Len(t, devicesOf(t, satu), 1)

	require.Equal(t, http.StatusOK, unregister(e, alice.AccessToken, "fcm-alice").Status)
	require.Empty(t, devicesOf(t, satu))
	// Deleting what is not there is not an error: the app may call it twice.
	require.Equal(t, http.StatusOK, unregister(e, alice.AccessToken, "fcm-alice").Status)
}

func TestPushTokenRequestsAreValidated(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	for name, body := range map[string]map[string]any{
		"an unknown platform": {"firebase_token": "fcm-1", "platform": "windows"},
		"no platform":         {"firebase_token": "fcm-1"},
		"no token":            {"platform": "ios"},
		"an empty token":      {"firebase_token": "", "platform": "ios"},
	} {
		res := e.send(http.MethodPost, tokenPath, phone.AccessToken, body)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, name)
	}
	require.Empty(t, devicesOf(t, satu))
	require.Equal(t, http.StatusUnauthorized, e.send(http.MethodPost, tokenPath, "", map[string]any{"firebase_token": "x", "platform": "ios"}).Status)
}

func TestRegisteringADeviceIsIdempotentWithAKey(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	body := mustMarshal(t, map[string]any{"firebase_token": "fcm-1", "platform": "android"})
	headers := map[string]string{"Authorization": "Bearer " + phone.AccessToken, "Idempotency-Key": uuid.NewString()}

	first := e.postRaw(tokenPath, body, headers)
	require.Equal(t, http.StatusOK, first.Status)
	require.Empty(t, first.Header.Get("Idempotent-Replayed"))

	replay := e.postRaw(tokenPath, body, headers)
	require.Equal(t, http.StatusOK, replay.Status)
	require.Equal(t, "true", replay.Header.Get("Idempotent-Replayed"))
	require.Len(t, devicesOf(t, satu), 1)
}

func TestDevicesStayInsideTheirOrganization(t *testing.T) {
	e := newEnv(t, "")
	inSatu := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	inDua := e.fieldSession(personeltest.EmailAnggota1Dua, phoneB)
	register(e, inSatu.AccessToken, "fcm-satu")

	// Org B's token cannot delete org A's device, even with the right push token.
	require.Equal(t, http.StatusOK, unregister(e, inDua.AccessToken, "fcm-satu").Status)
	require.Len(t, devicesOf(t, satu), 1)

	// And registering the same token string in org B takes nothing from org A.
	require.Equal(t, http.StatusOK, register(e, inDua.AccessToken, "fcm-satu").Status)
	require.Len(t, devicesOf(t, satu), 1)
	require.Len(t, devicesOf(t, dua), 1)
}
