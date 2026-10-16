package identity_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/personel"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// ⚠ The web login gate holds on refresh too: a WEB session of a person whose web role was
// revoked ends at its next refresh, instead of rolling on without a bound phone.
func TestAWebSessionEndsAtRefreshOnceTheWebRoleIsGone(t *testing.T) {
	e := newEnv(t, "")
	first := e.login(web(personeltest.EmailDanruSatu)).data(t)
	execTenant(t, satu, "DELETE FROM web_role_assignments")

	res := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusForbidden, res.Status)
	require.Equal(t, access.ReasonNotAdmin, res.err(t).reason())
	require.Equal(t, 0, liveSessionsOfChain(t, satu, first.RefreshToken), "the chain is ended, not just refused")
	require.Equal(t, http.StatusUnauthorized, e.whoami(first.AccessToken), "and its access token is cut off")
	again := e.refresh(first.RefreshToken)
	require.Equal(t, http.StatusForbidden, again.Status, "the same token keeps the same answer")
	require.Equal(t, access.ReasonNotAdmin, again.err(t).reason())

	// A MOBILE session of the same person is not a web session and is unaffected.
	phone := e.fieldSession(personeltest.EmailDanruSatu, phoneA)
	e.refresh(phone.RefreshToken).data(t)
}

func TestMeReportsAnInactivePersonAsInactive(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	require.True(t, dataOf[meActive](t, e.api(phone.AccessToken, http.MethodGet, "/auth/me", nil)).User.IsActive)

	// The resync deactivated them while the revocation list was unavailable: the token still
	// passes, but /auth/me must not claim they are active.
	execTenant(t, satu, "UPDATE personnels SET status = 'INACTIVE' WHERE id = $1", phone.User.ID)
	require.False(t, dataOf[meActive](t, e.api(phone.AccessToken, http.MethodGet, "/auth/me", nil)).User.IsActive)
}

type meActive struct {
	User struct {
		IsActive bool `json:"is_active"`
	} `json:"user"`
}

// Fetching a person for an admin is not that person's own activity: the billing active-head
// clock (last_seen_at) must not move.
func TestEnsureDoesNotCountAsThePersonsActivity(t *testing.T) {
	e := newEnv(t, "")
	id, err := e.people().Ensure(t.Context(), satu, personeltest.NIKAnggota2Satu)
	require.NoError(t, err)
	require.Nil(t, scalar[*string](t, satu, "SELECT last_seen_at::text FROM personnels WHERE id = $1", id))

	// Their own login does count, and a later Ensure does not undo it.
	e.fieldSession(personeltest.EmailAnggota2Satu, phoneA)
	seen := scalar[string](t, satu, "SELECT last_seen_at::text FROM personnels WHERE id = $1", id)
	execTenant(t, satu, "UPDATE personnels SET nik = nik WHERE id = $1", id)
	_, err = e.people().Ensure(t.Context(), satu, personeltest.NIKAnggota2Satu)
	require.NoError(t, err)
	require.Equal(t, seen, scalar[string](t, satu, "SELECT last_seen_at::text FROM personnels WHERE id = $1", id))
}

// ⚠ Two ORG_ADMINs removing each other at the same moment must not leave the organization
// without an admin: the second finds, under the lock, that it is no longer one.
func TestTwoAdminsRemovingEachOtherLeaveOneAdmin(t *testing.T) {
	e := newEnv(t, "")
	adminA := e.adminToken(personeltest.EmailDanruSatu)
	idA := scalar[int64](t, satu, "SELECT id FROM personnels WHERE email = $1", personeltest.EmailDanruSatu)
	idB := e.makeAdmin(personeltest.EmailSupervisorSatu)

	// B's removal of A, in flight: it holds both assignments, as WebRoles.Delete does.
	tx := openTenantTx(t, satu)
	_, err := tx.Exec(t.Context(),
		"SELECT personnel_id FROM web_role_assignments WHERE personnel_id = ANY($1) ORDER BY personnel_id FOR UPDATE",
		[]int64{idA, idB})
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "DELETE FROM web_role_assignments WHERE personnel_id = $1", idA)
	require.NoError(t, err)

	done := e.async(http.MethodDelete, "/admin/web-roles/"+itoa(idB), adminA, nil)
	waitUntilBlocked(t)
	require.NoError(t, tx.Commit(t.Context()))

	res := await(t, done)
	require.Equal(t, http.StatusForbidden, res.Status)
	require.Equal(t, access.ReasonNotAdmin, res.err(t).reason())
	require.Equal(t, 1, scalar[int](t, satu, "SELECT count(*) FROM web_role_assignments WHERE role = 'ORG_ADMIN'"),
		"one admin is left")
}

// ⚠ The resync must not overwrite what a login wrote while Personalia was being asked.
func TestResyncSkipsAPersonUpdatedDuringTheCall(t *testing.T) {
	e := newEnv(t, "")
	person := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	makeAllStale(t, satu)
	// Personalia's answer the resync gets says the person left...
	e.fake.Deactivate(personeltest.NIKAnggota1Satu)

	// ...but a login of the person is committing in the meantime, with fresher data.
	tx := openTenantTx(t, satu)
	_, err := tx.Exec(t.Context(), "UPDATE personnels SET synced_at = now() WHERE id = $1", person.User.ID)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { _, err := e.resyncer().Resync(t.Context()); done <- err }()
	waitUntilBlocked(t) // the resync's write transaction waits for the person
	require.NoError(t, tx.Commit(t.Context()))

	require.NoError(t, <-done)
	require.Equal(t, "ACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", person.User.ID),
		"the fresher row is left for the next run")
	require.Equal(t, http.StatusOK, e.whoami(person.AccessToken))
}

// brokenPersonalia answers a login without naming the employing company, which breaks
// Personalia's own contract.
type brokenPersonalia struct{}

func (brokenPersonalia) Login(context.Context, personel.LoginInput) (personel.Identity, error) {
	return personel.Identity{PersonelUserID: 7, Active: true, FullName: "No Company"}, nil
}

func (brokenPersonalia) EmployeesByNIKs(context.Context, int64, []string) ([]personel.Identity, error) {
	return nil, nil
}

func TestALoginAnswerWithoutAnOrganizationIsAnOutageNotA500(t *testing.T) {
	testkit.Reset(t)
	usecase := identity.NewUsecase(testkit.Pool(t), brokenPersonalia{}, testkit.Tokens(t),
		testkit.NewRevocations(t, nil), clock.System{}, quietLogger())

	_, err := usecase.Login(t.Context(), identity.LoginInput{Email: "x@siapp.test", Password: "x", Client: identity.ClientWeb})
	got := requireAppError(t, err, http.StatusServiceUnavailable)
	require.Equal(t, apperr.ReasonPersonelUnavailable, got.Reason)
}

// ⚠ Every revoke must say why (sessions_revoke_reason_check): a new query that revoked without
// a reason would otherwise bring back the "everything reads as a token reuse" answers.
func TestTheDatabaseRefusesARevokeWithoutAReason(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)

	tx := openTenantTx(t, satu)
	_, err := tx.Exec(t.Context(), "UPDATE sessions SET revoked_at = now() WHERE id = $1", sessionOf(t, satu, phone.RefreshToken))
	require.Error(t, err)
	require.Contains(t, err.Error(), "sessions_revoke_reason_check")
}

// Each way a session ends is answered with its own reason on the next refresh.
func TestARevokedTokenIsAnsweredWithWhyItsSessionEnded(t *testing.T) {
	cases := []struct {
		reason string
		status int
		want   string
	}{
		{"ROTATED", http.StatusUnauthorized, identity.ReasonRefreshReused},
		{"REUSE_DETECTED", http.StatusUnauthorized, identity.ReasonRefreshReused},
		{"LOGOUT", http.StatusUnauthorized, "AUTH_TOKEN_INVALID"},
		{"DEVICE_RESET", http.StatusUnauthorized, identity.ReasonDeviceChanged},
		{"DEVICE_CHANGED", http.StatusUnauthorized, identity.ReasonDeviceChanged},
		{"ACCOUNT_DISABLED", http.StatusForbidden, identity.ReasonAccountDisabled},
		{"WEB_ROLE_REVOKED", http.StatusForbidden, access.ReasonNotAdmin},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			e := newEnv(t, "")
			phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
			execTenant(t, satu, "UPDATE sessions SET revoked_at = now(), revoke_reason = $1::session_revoke_reason", c.reason)

			res := e.refresh(phone.RefreshToken)
			require.Equal(t, c.status, res.Status)
			require.Equal(t, c.want, res.err(t).reason())
		})
	}
}

func TestSchedules(t *testing.T) {
	require.Equal(t, "0 20 * * *", identity.CleanupSchedule, "the session cleanup runs daily at 20:00 UTC (03:00 WIB)")
	require.Equal(t, "30 20 * * *", identity.ResyncSchedule, "the people resync runs daily at 20:30 UTC, after the cleanup")
}

func TestDeletingAPushTokenIsValidated(t *testing.T) {
	e := newEnv(t, "")
	phone := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	for name, body := range map[string]map[string]any{
		"no token":       {},
		"an empty token": {"firebase_token": ""},
	} {
		res := e.send(http.MethodDelete, tokenPath, phone.AccessToken, body)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, name)
	}
}
