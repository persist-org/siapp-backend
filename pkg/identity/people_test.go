package identity_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// newMux returns a function that runs a worker task by name through the real asynq mux the
// worker uses, without a Redis queue in between.
func newMux(t *testing.T, e *env) func(task string) error {
	t.Helper()
	mux := asynq.NewServeMux()
	identity.RegisterResyncJob(mux, e.resyncer(), quietLogger())
	return func(task string) error { return mux.ProcessTask(t.Context(), asynq.NewTask(task, nil)) }
}

func TestResyncIsScheduledDaily(t *testing.T) {
	scheduler := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "localhost:6379"}, nil)
	require.NoError(t, identity.RegisterResyncSchedule(scheduler))
}

func (e *env) people() *identity.People {
	e.t.Helper()
	return identity.NewPeople(testkit.Pool(e.t), e.personel, clock.System{})
}

func requireAppError(t *testing.T, err error, status int) *apperr.Error {
	t.Helper()
	appErr, ok := errors.AsType[*apperr.Error](err)
	require.True(t, ok, "want an API error, got %v", err)
	require.Equal(t, status, appErr.Status)
	return appErr
}

func TestEnsureCreatesAPersonOnce(t *testing.T) {
	e := newEnv(t, "")
	people := e.people()

	id, err := people.Ensure(t.Context(), satu, personeltest.NIKAnggota2Satu)
	require.NoError(t, err)
	require.NotZero(t, id)
	require.Equal(t, 1, e.fake.CallCount("/employees"))
	require.Equal(t, personeltest.NIKAnggota2Satu, scalar[string](t, satu, "SELECT nik FROM personnels WHERE id = $1", id))
	require.Equal(t, "Anggota Satu B", scalar[string](t, satu, "SELECT full_name FROM personnels WHERE id = $1", id))

	// The organization was registered too, from Personalia's answer.
	var name string
	require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT name FROM organizations WHERE id = $1", satu).Scan(&name))
	require.Equal(t, "PT Siaga Satu (seed)", name)

	// Known now: the second call is a local read.
	again, err := people.Ensure(t.Context(), satu, personeltest.NIKAnggota2Satu)
	require.NoError(t, err)
	require.Equal(t, id, again)
	require.Equal(t, 1, e.fake.CallCount("/employees"))
	require.Equal(t, 1, countRows(t, satu, "personnels"))
}

// ⚠ Two parallel calls for the same NIK must create ONE row.
func TestEnsureInParallelCreatesOneRow(t *testing.T) {
	e := newEnv(t, "")
	people := e.people()

	const callers = 8
	ids := make(chan int64, callers)
	errs := make(chan error, callers)
	var start sync.WaitGroup
	start.Add(1)
	for range callers {
		go func() {
			start.Wait()
			id, err := people.Ensure(t.Context(), satu, personeltest.NIKAnggota1Satu)
			ids <- id
			errs <- err
		}()
	}
	start.Done()

	first := <-ids
	require.NoError(t, <-errs)
	for range callers - 1 {
		require.Equal(t, first, <-ids)
		require.NoError(t, <-errs)
	}
	require.Equal(t, 1, countRows(t, satu, "personnels"))
}

func TestEnsureOnlyFindsPeopleOfTheCallersOrganization(t *testing.T) {
	e := newEnv(t, "")
	people := e.people()

	// A person of PT Siaga Dua, asked for by Siaga Satu: not found, and nothing is cached in
	// either organization.
	_, err := people.Ensure(t.Context(), satu, personeltest.NIKDanruDua)
	_ = requireAppError(t, err, http.StatusNotFound)
	require.Equal(t, 0, countRows(t, satu, "personnels"))
	require.Equal(t, 0, countRows(t, dua, "personnels"))

	for name, c := range map[string]struct {
		org int64
		nik string
	}{
		"a NIK nobody has": {satu, "NOPE0000"},
		"an empty NIK":     {satu, "  "},
		"no organization":  {0, personeltest.NIKDanruSatu},
	} {
		_, err := people.Ensure(t.Context(), c.org, c.nik)
		_ = requireAppError(t, err, http.StatusNotFound)
		require.NotEmpty(t, name)
	}
}

func TestEnsureWhilePersonaliaIsDown(t *testing.T) {
	e := newEnv(t, "")
	e.fake.Down()

	_, err := e.people().Ensure(t.Context(), satu, personeltest.NIKAnggota1Satu)
	got := requireAppError(t, err, http.StatusServiceUnavailable)
	require.Equal(t, apperr.ReasonPersonelUnavailable, got.Reason)

	// A person who is already known needs no call at all.
	e.fake.Up()
	id, err := e.people().Ensure(t.Context(), satu, personeltest.NIKAnggota1Satu)
	require.NoError(t, err)
	e.fake.Down()
	again, err := e.people().Ensure(t.Context(), satu, personeltest.NIKAnggota1Satu)
	require.NoError(t, err)
	require.Equal(t, id, again)
}

func TestEnsureCachesAnInactivePersonAsInactive(t *testing.T) {
	e := newEnv(t, "")
	id, err := e.people().Ensure(t.Context(), satu, personeltest.NIKNonaktifSatu)
	require.NoError(t, err, "the caller decides what to do with an inactive person")
	require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", id))
}

// --- people resync -----------------------------------------------------------------------

func (e *env) resyncer() *identity.Resyncer {
	e.t.Helper()
	return identity.NewResyncer(testkit.Pool(e.t), e.personel, e.revocations, clock.System{}, quietLogger())
}

func makeAllStale(t *testing.T, org int64) {
	t.Helper()
	execTenant(t, org, "UPDATE personnels SET synced_at = now() - interval '25 hours'")
}

func TestResyncDeactivatesPeopleWhoLeftAndRefreshesTheRest(t *testing.T) {
	e := newEnv(t, "")
	stays := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	leaves := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)
	register(e, leaves.AccessToken, "fcm-leaves")
	lastSeen := scalar[time.Time](t, satu, "SELECT last_seen_at FROM personnels WHERE id = $1", stays.User.ID)

	makeAllStale(t, satu)
	e.fake.Deactivate(personeltest.NIKAnggota2Satu)
	e.fake.Rename(personeltest.NIKAnggota1Satu, "Anggota Satu Baru")

	looked, err := e.resyncer().Resync(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, looked)

	// The person who left: INACTIVE, every session ended, devices gone, tokens cut off.
	require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", leaves.User.ID))
	require.Equal(t, 0, scalar[int](t, satu, "SELECT count(*) FROM sessions WHERE personnel_id = $1 AND revoked_at IS NULL", leaves.User.ID))
	require.Empty(t, devicesOf(t, satu))
	require.Equal(t, http.StatusUnauthorized, e.whoami(leaves.AccessToken), "cut off at once, not after 15 minutes")
	afterResync := e.refresh(leaves.RefreshToken)
	require.Equal(t, http.StatusForbidden, afterResync.Status, "and their refresh fails")
	require.Equal(t, identity.ReasonAccountDisabled, afterResync.err(t).reason(),
		"with the account-inactive answer, not a token-reuse one")

	// The person who stays: fresh data, a new synced_at, sessions untouched.
	require.Equal(t, "ACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", stays.User.ID))
	require.Equal(t, "Anggota Satu Baru", scalar[string](t, satu, "SELECT full_name FROM personnels WHERE id = $1", stays.User.ID))
	require.True(t, scalar[bool](t, satu, "SELECT synced_at > now() - interval '1 minute' FROM personnels WHERE id = $1", stays.User.ID))
	require.Equal(t, http.StatusOK, e.whoami(stays.AccessToken))
	// A resync is not the person's own activity: the billing clock is not moved by it.
	require.True(t, lastSeen.Equal(scalar[time.Time](t, satu, "SELECT last_seen_at FROM personnels WHERE id = $1", stays.User.ID)))
}

func TestResyncTreatsEveryWayOfLeavingTheSame(t *testing.T) {
	e := newEnv(t, "")
	moved := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	unknown := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)
	makeAllStale(t, satu)
	e.fake.MoveToCompany(personeltest.NIKAnggota1Satu, personeltest.OrgDua)
	// A person Personalia does not return at all (the fixture list has no such NIK).
	execTenant(t, satu, "UPDATE personnels SET nik = 'GONE0001' WHERE id = $1", unknown.User.ID)

	_, err := e.resyncer().Resync(t.Context())
	require.NoError(t, err)
	require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", moved.User.ID), "employed elsewhere now")
	require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", unknown.User.ID), "no longer returned")
}

func TestResyncOnlyLooksAtStaleActivePeople(t *testing.T) {
	e := newEnv(t, "")
	e.fieldSession(personeltest.EmailAnggota1Satu, phoneA) // fresh
	inactive := e.fieldSession(personeltest.EmailAnggota2Satu, phoneB)
	execTenant(t, satu, "UPDATE personnels SET status = 'INACTIVE', synced_at = now() - interval '30 days' WHERE id = $1", inactive.User.ID)

	looked, err := e.resyncer().Resync(t.Context())
	require.NoError(t, err)
	require.Zero(t, looked)
	require.Zero(t, e.fake.CallCount("/employees"), "nothing to ask Personalia about")
}

// ⚠ An outage of Personalia must not deactivate anybody.
func TestResyncNeverDeactivatesOnAnOutage(t *testing.T) {
	e := newEnv(t, "")
	person := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	makeAllStale(t, satu)
	e.fake.Down()

	_, err := e.resyncer().Resync(t.Context())
	require.Error(t, err, "the run fails, so the scheduler retries it")
	require.Equal(t, "ACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", person.User.ID))
	require.Equal(t, http.StatusOK, e.whoami(person.AccessToken))
}

func TestResyncWorksInBatchesAndPerOrganization(t *testing.T) {
	e := newEnv(t, "")
	inSatu := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	inDua := e.fieldSession(personeltest.EmailAnggota1Dua, phoneB)
	// 450 people Personalia does not know, in Satu: two full batches and a partial one.
	execTenant(t, satu, `
		INSERT INTO personnels (personel_user_id, nik, full_name, status, synced_at)
		SELECT 5000000 + g, 'BULK' || g, 'Bulk ' || g, 'ACTIVE', now() - interval '25 hours'
		FROM generate_series(1, 450) AS g`)
	makeAllStale(t, satu)
	makeAllStale(t, dua)
	e.fake.Deactivate(personeltest.NIKAnggota1Dua)

	looked, err := e.resyncer().Resync(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 452, looked)
	// 451 people in Satu = 3 requests (200 + 200 + 51), 1 in Dua.
	require.Equal(t, 4, e.fake.CallCount("/employees"))
	require.Equal(t, 450, scalar[int](t, satu, "SELECT count(*) FROM personnels WHERE status = 'INACTIVE'"))
	require.Equal(t, "ACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", inSatu.User.ID))
	require.Equal(t, "INACTIVE", scalar[string](t, dua, "SELECT status::text FROM personnels WHERE id = $1", inDua.User.ID))
}

func TestResyncRunsAsAWorkerTask(t *testing.T) {
	e := newEnv(t, "")
	person := e.fieldSession(personeltest.EmailAnggota1Satu, phoneA)
	makeAllStale(t, satu)
	e.fake.Deactivate(personeltest.NIKAnggota1Satu)

	mux := newMux(t, e)
	require.NoError(t, mux(identity.TaskResyncPeople))
	require.Equal(t, "INACTIVE", scalar[string](t, satu, "SELECT status::text FROM personnels WHERE id = $1", person.User.ID))
}
