package identity_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"siapp/pkg/identity"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
)

// newDeadSession inserts a session of the person whose revocation and expiry happened this
// many days ago. A negative revokedDaysAgo means "never revoked"; a negative expiredDaysAgo
// means "expires in that many days" (still alive). It returns the row's id.
func newDeadSession(t *testing.T, org, personnelID int64, revokedDaysAgo, expiredDaysAgo int) int64 {
	t.Helper()
	return scalar[int64](t, org, `
		INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, client, expires_at, revoked_at, created_at, updated_at)
		VALUES ($1, gen_random_uuid(), md5(random()::text) || md5(random()::text), 'WEB',
		        now() - make_interval(days => $2),
		        CASE WHEN $3 < 0 THEN NULL ELSE now() - make_interval(days => $3) END,
		        now() - interval '60 days', now())
		RETURNING id`, personnelID, expiredDaysAgo, revokedDaysAgo)
}

func sessionExists(t *testing.T, org, id int64) bool {
	t.Helper()
	return scalar[int](t, org, "SELECT count(*) FROM sessions WHERE id = $1", id) == 1
}

func newCleaner(t *testing.T) *identity.Cleaner {
	t.Helper()
	return identity.NewCleaner(testkit.Pool(t), clock.System{})
}

func TestCleanupDeletesOnlySessionsLongDead(t *testing.T) {
	e := newEnv(t, "")
	person := e.login(web(personeltest.EmailDanruSatu)).data(t)
	live := sessionOf(t, satu, person.RefreshToken)

	revokedLongAgo := newDeadSession(t, satu, person.User.ID, 31, -1)
	revokedRecently := newDeadSession(t, satu, person.User.ID, 29, -1)
	expiredLongAgo := newDeadSession(t, satu, person.User.ID, -1, 31)
	expiredRecently := newDeadSession(t, satu, person.User.ID, -1, 29)
	notYetExpired := newDeadSession(t, satu, person.User.ID, -1, -5)

	deleted, err := newCleaner(t).Cleanup(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted)

	require.False(t, sessionExists(t, satu, revokedLongAgo))
	require.False(t, sessionExists(t, satu, expiredLongAgo))
	// Recently dead rows stay, so replaying their tokens is still recognised as reuse.
	require.True(t, sessionExists(t, satu, revokedRecently))
	require.True(t, sessionExists(t, satu, expiredRecently))
	require.True(t, sessionExists(t, satu, notYetExpired))
	require.True(t, sessionExists(t, satu, live), "a live session is never touched")
}

func TestCleanupRunsInBatchesAcrossOrganizations(t *testing.T) {
	e := newEnv(t, "")
	inSatu := e.login(web(personeltest.EmailDanruSatu)).data(t)
	inDua := e.login(web(personeltest.EmailDanruDua)).data(t)

	// More than two batches of 1000 in one organization, and some in the other.
	for _, c := range []struct {
		org, person int64
		rows        int
	}{{satu, inSatu.User.ID, 2500}, {dua, inDua.User.ID, 30}} {
		execTenant(t, c.org, `
			INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, client, expires_at, created_at, updated_at)
			SELECT $1, gen_random_uuid(), md5(random()::text || g::text) || md5(g::text), 'WEB',
			       now() - interval '45 days', now() - interval '60 days', now()
			FROM generate_series(1, $2) AS g`, c.person, c.rows)
	}

	deleted, err := newCleaner(t).Cleanup(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 2530, deleted)
	require.Equal(t, 1, countRows(t, satu, "sessions"), "only the live login is left")
	require.Equal(t, 1, countRows(t, dua, "sessions"))
}

// A refresh chain points each retired session at its successor. The cleanup must be able to
// delete the successor first, so the link is cleared instead of blocking the delete.
func TestCleanupDeletesChainsInAnyOrder(t *testing.T) {
	e := newEnv(t, "")
	person := e.login(web(personeltest.EmailDanruSatu)).data(t)
	recentlyRevoked := newDeadSession(t, satu, person.User.ID, 29, -1) // stays
	successor := newDeadSession(t, satu, person.User.ID, -1, 35)       // goes, and is pointed at
	execTenant(t, satu, "UPDATE sessions SET replaced_by_id = $1 WHERE id = $2", successor, recentlyRevoked)

	_, err := newCleaner(t).Cleanup(t.Context())
	require.NoError(t, err)

	require.False(t, sessionExists(t, satu, successor))
	require.True(t, sessionExists(t, satu, recentlyRevoked))
	require.Nil(t, scalar[*int64](t, satu, "SELECT replaced_by_id FROM sessions WHERE id = $1", recentlyRevoked))
}

func TestCleanupRunsAsAWorkerTask(t *testing.T) {
	e := newEnv(t, "")
	person := e.login(web(personeltest.EmailDanruSatu)).data(t)
	old := newDeadSession(t, satu, person.User.ID, 40, -1)

	mux := asynq.NewServeMux()
	identity.RegisterJobs(mux, newCleaner(t), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	require.NoError(t, mux.ProcessTask(t.Context(), asynq.NewTask(identity.TaskCleanupSessions, nil)))
	require.False(t, sessionExists(t, satu, old))
}

func TestCleanupIsScheduledDailyAt2000UTC(t *testing.T) {
	scheduler := asynq.NewScheduler(asynq.RedisClientOpt{Addr: "localhost:6379"}, nil)
	require.NoError(t, identity.RegisterSchedule(scheduler))
}

// ⚠ The cleanup never bypasses row-level security: a session of an organization that has no
// tenant context is invisible to it, and it reaches each organization only through its own
// tenant transaction.
func TestCleanupStaysInsideTenants(t *testing.T) {
	e := newEnv(t, "")
	inSatu := e.login(web(personeltest.EmailDanruSatu)).data(t)
	e.login(web(personeltest.EmailDanruDua)).data(t)
	newDeadSession(t, satu, inSatu.User.ID, 40, -1)

	deleted, err := newCleaner(t).Cleanup(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	require.Equal(t, 1, countRows(t, dua, "sessions"), "the other organization is untouched")

	// Without a tenant set the app role sees no sessions at all.
	var n int
	require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT count(*) FROM sessions").Scan(&n))
	require.Zero(t, n)
}
