package database_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/database"
	"siapp/internal/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// probeTable is created by the tests, not by a migration, because the platform has no
// tables of its own yet. Reset truncates it like any other table.
func probeTable(t *testing.T, pool *sql.DB) {
	t.Helper()
	_, err := pool.ExecContext(t.Context(), `
		CREATE TABLE IF NOT EXISTS tx_probe (
			id   INT PRIMARY KEY,
			code VARCHAR(20) NOT NULL,
			UNIQUE KEY uq_tx_probe_code (code)
		) ENGINE=InnoDB`)
	require.NoError(t, err)
	testkit.Reset(t, pool)
}

func countProbe(t *testing.T, pool *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM tx_probe").Scan(&n))
	return n
}

func TestWithTx(t *testing.T) {
	pool := testkit.DB(t)
	probeTable(t, pool)
	errBoom := errors.New("boom")

	t.Run("rolls back when fn returns an error", func(t *testing.T) {
		testkit.Reset(t, pool)
		err := database.WithTxRaw(t.Context(), pool, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO tx_probe (id, code) VALUES (1, 'a')")
			require.NoError(t, err)
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)
		require.Equal(t, 0, countProbe(t, pool))
	})

	t.Run("rolls back and re-panics when fn panics", func(t *testing.T) {
		testkit.Reset(t, pool)
		require.PanicsWithValue(t, "kaboom", func() {
			_ = database.WithTxRaw(t.Context(), pool, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), "INSERT INTO tx_probe (id, code) VALUES (1, 'a')")
				require.NoError(t, err)
				panic("kaboom")
			})
		})
		require.Equal(t, 0, countProbe(t, pool))
	})

	t.Run("commits when fn returns nil", func(t *testing.T) {
		testkit.Reset(t, pool)
		err := database.WithTxRaw(t.Context(), pool, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO tx_probe (id, code) VALUES (1, 'a')")
			return err
		})
		require.NoError(t, err)
		require.Equal(t, 1, countProbe(t, pool))
	})

	t.Run("passes queries bound to the transaction", func(t *testing.T) {
		err := database.WithTx(t.Context(), pool, func(q *db.Queries) error {
			one, err := q.Ping(t.Context())
			require.EqualValues(t, 1, one)
			return err
		})
		require.NoError(t, err)
	})
}

func TestDuplicateKey(t *testing.T) {
	pool := testkit.DB(t)
	probeTable(t, pool)
	ctx := context.Background()

	_, err := pool.ExecContext(ctx, "INSERT INTO tx_probe (id, code) VALUES (1, 'a')")
	require.NoError(t, err)

	tests := []struct {
		name    string
		insert  string
		wantKey string
	}{
		{"primary key", "INSERT INTO tx_probe (id, code) VALUES (1, 'b')", "PRIMARY"},
		{"unique key", "INSERT INTO tx_probe (id, code) VALUES (2, 'a')", "uq_tx_probe_code"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.ExecContext(ctx, tc.insert)
			require.True(t, database.IsDuplicateKey(err))
			require.Equal(t, tc.wantKey, database.DuplicateKeyName(err))
		})
	}

	t.Run("other errors", func(t *testing.T) {
		_, err := pool.ExecContext(ctx, "INSERT INTO no_such_table VALUES (1)")
		require.Error(t, err)
		require.False(t, database.IsDuplicateKey(err))
		require.Empty(t, database.DuplicateKeyName(err))
		require.False(t, database.IsDuplicateKey(nil))
	})
}

func TestOpenRejectsDSNWithoutUTC(t *testing.T) {
	tests := []struct {
		dsn  string
		want string
	}{
		{"u:p@tcp(localhost:3306)/x?loc=UTC&time_zone=%27%2B00%3A00%27", "parseTime=true"},
		{"u:p@tcp(localhost:3306)/x?parseTime=true&loc=Local&time_zone=%27%2B00%3A00%27", "loc=UTC"},
		{"u:p@tcp(localhost:3306)/x?parseTime=true&loc=UTC", "time_zone"},
	}
	for _, tc := range tests {
		_, err := database.Open(context.Background(), tc.dsn)
		require.ErrorContains(t, err, tc.want)
	}
}
