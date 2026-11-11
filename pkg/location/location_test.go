package location_test

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/location"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

func partitionExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", "public."+name).Scan(&exists))
	return exists
}

func TestMonthlyPartitionsAreCreatedAhead(t *testing.T) {
	m := location.NewMaintenance(testkit.Pool(t), clock.System{})
	require.NoError(t, m.EnsurePartitions(t.Context()))
	require.NoError(t, m.EnsurePartitions(t.Context()), "idempotent")

	first := time.Now().UTC()
	for i := -1; i <= 3; i++ {
		month := time.Date(first.Year(), first.Month()+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		require.True(t, partitionExists(t, "location_logs_"+month.Format("2006_01")), month.Format("2006-01"))
	}
}

func TestRetentionDropsOldPartitionsAndKeepsRecentOnes(t *testing.T) {
	pool := testkit.Pool(t)
	org := testkit.NewOrg(t)
	var personnelID int64
	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"INSERT INTO personnels (personel_user_id, nik, full_name, status) VALUES (1, 'N1', 'Budi', 'ACTIVE') RETURNING id").Scan(&personnelID)
	}))

	// A partition eleven months back (the oldest the helper accepts is twelve) with a point in it.
	old := time.Now().UTC().AddDate(0, -11, 0)
	month := time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC)
	_, err := pool.Exec(t.Context(), "SELECT ensure_location_partition($1::date)", month)
	require.NoError(t, err)
	name := "location_logs_" + month.Format("2006_01")
	require.True(t, partitionExists(t, name))
	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(),
			"INSERT INTO location_logs (personnel_id, logged_at, latitude, longitude, accuracy_meters) VALUES ($1, $2, -6.2, 106.8, 5)",
			personnelID, month.Add(time.Hour))
		return err
	}))

	m := location.NewMaintenance(pool, clock.System{})
	require.NoError(t, m.EnsurePartitions(t.Context()))
	dropped, err := m.DropExpired(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, dropped)
	require.False(t, partitionExists(t, name), "the old month is gone with its points")

	thisMonth := "location_logs_" + time.Now().UTC().Format("2006_01")
	require.True(t, partitionExists(t, thisMonth), "the current month stays")
	dropped, err = m.DropExpired(t.Context())
	require.NoError(t, err)
	require.Zero(t, dropped, "nothing else is due")
}
