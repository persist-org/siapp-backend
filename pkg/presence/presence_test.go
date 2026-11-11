package presence_test

import (
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
	"siapp/pkg/presence"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// person adds a person to a new organization and returns the organization and their id.
func person(t *testing.T, name string) (org, id int64) {
	t.Helper()
	org = testkit.NewOrg(t)
	return org, addPerson(t, org, name)
}

func addPerson(t *testing.T, org int64, name string) (id int64) {
	t.Helper()
	require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"INSERT INTO personnels (personel_user_id, nik, full_name, status) VALUES ((SELECT coalesce(max(personel_user_id), 0) + 1 FROM personnels), $1::text, $1::text, 'ACTIVE') RETURNING id", name).Scan(&id)
	}))
	return id
}

func lastSeen(t *testing.T, org, id int64) (seen *time.Time) {
	t.Helper()
	require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), "SELECT last_seen_at FROM personnels WHERE id = $1", id).Scan(&seen)
	}))
	return seen
}

func newService(t *testing.T, c clock.Clock) *presence.Service {
	t.Helper()
	return presence.New(testkit.Redis(t), testkit.RandomPrefix(t), testkit.Pool(t), c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// stepClock is a clock a test moves by hand.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func TestTouchPersistsOncePerMinute(t *testing.T) {
	org, id := person(t, "Budi")
	clk := &stepClock{now: time.Now().UTC().Truncate(time.Millisecond)}
	prefix := testkit.RandomPrefix(t)
	s := presence.New(testkit.Redis(t), prefix, testkit.Pool(t), clk, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	start := clk.now

	require.NoError(t, s.Touch(t.Context(), org, id))
	first := lastSeen(t, org, id)
	require.NotNil(t, first)
	require.WithinDuration(t, start, *first, time.Millisecond)

	// The gate is shut: 30 s later the person is still online, but nothing is written.
	clk.now = start.Add(30 * time.Second)
	require.NoError(t, s.Touch(t.Context(), org, id))
	require.WithinDuration(t, start, *lastSeen(t, org, id), time.Millisecond, "throttled")
	got, err := s.Many(t.Context(), org, []int64{id})
	require.NoError(t, err)
	require.True(t, got[id].Online)
	require.WithinDuration(t, clk.now, *got[id].LastSeenAt, time.Millisecond, "online reports the fresh time, from Redis")

	// Once the gate's minute has passed (its key expires), the next touch writes again.
	gate := prefix + "org:" + strconv.FormatInt(org, 10) + ":presence:persist:" + strconv.FormatInt(id, 10)
	require.EqualValues(t, 1, testkit.Redis(t).Del(t.Context(), gate).Val(), "the gate key is where the doc says")
	clk.now = start.Add(90 * time.Second)
	require.NoError(t, s.Touch(t.Context(), org, id))
	require.WithinDuration(t, clk.now, *lastSeen(t, org, id), time.Millisecond)
}

func TestManyReadsRedisThenTheDatabase(t *testing.T) {
	org, online := person(t, "Budi")
	offline := addPerson(t, org, "Cici")
	never := addPerson(t, org, "Dodi")
	require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), "UPDATE personnels SET last_seen_at = now() - interval '2 hours' WHERE id = $1", offline)
		return err
	}))
	s := newService(t, clock.System{})
	require.NoError(t, s.Touch(t.Context(), org, online))

	got, err := s.Many(t.Context(), org, []int64{online, offline, never})
	require.NoError(t, err)
	require.True(t, got[online].Online)
	require.NotNil(t, got[online].LastSeenAt)
	require.False(t, got[offline].Online, "seen two hours ago")
	require.NotNil(t, got[offline].LastSeenAt, "but the persisted time is still reported")
	require.WithinDuration(t, time.Now().Add(-2*time.Hour), *got[offline].LastSeenAt, time.Minute)
	require.False(t, got[never].Online)
	require.Nil(t, got[never].LastSeenAt)
}

func TestMarkOfflineAndPerOrganizationKeys(t *testing.T) {
	s := newService(t, clock.System{})
	orgA, a := person(t, "Budi")
	orgB := testkit.NewOrg(t)

	require.NoError(t, s.Touch(t.Context(), orgA, a))
	gotA, err := s.Many(t.Context(), orgA, []int64{a})
	require.NoError(t, err)
	require.True(t, gotA[a].Online)
	gotB, err := s.Many(t.Context(), orgB, []int64{a})
	require.NoError(t, err)
	require.False(t, gotB[a].Online, "the same id in another organization has its own key")

	require.NoError(t, s.MarkOffline(t.Context(), orgA, a))
	gotA, err = s.Many(t.Context(), orgA, []int64{a})
	require.NoError(t, err)
	require.False(t, gotA[a].Online)
	require.NotNil(t, gotA[a].LastSeenAt, "logout keeps the last seen")
}
