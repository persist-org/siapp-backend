package database_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

var probeOnce sync.Once

// probeTable creates (once) a throwaway tenant table that follows the README's RLS
// template exactly, empties the database, and returns the table's name. The table exists
// only in the test database.
func probeTable(t *testing.T) {
	t.Helper()
	owner := testkit.OwnerPool(t)
	probeOnce.Do(func() {
		_, err := owner.Exec(context.Background(), `
			CREATE TABLE isolation_probe (
				id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
				                REFERENCES organizations(id),
				note            TEXT NOT NULL
			);
			ALTER TABLE isolation_probe ENABLE ROW LEVEL SECURITY;
			ALTER TABLE isolation_probe FORCE ROW LEVEL SECURITY;
			CREATE POLICY org_isolation ON isolation_probe
				USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
				WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
			GRANT SELECT, INSERT, UPDATE, DELETE ON isolation_probe TO siapp_app;`)
		if err != nil {
			panic(err)
		}
	})
	testkit.Reset(t)
}

func insertNote(ctx context.Context, tx pgx.Tx, note string) error {
	_, err := tx.Exec(ctx, "INSERT INTO isolation_probe (note) VALUES ($1)", note)
	return err
}

func countNotes(ctx context.Context, tx pgx.Tx) (n int, err error) {
	err = tx.QueryRow(ctx, "SELECT count(*) FROM isolation_probe").Scan(&n)
	return n, err
}

func TestWithTenantCommitsOnNil(t *testing.T) {
	probeTable(t)
	pool, org := testkit.Pool(t), testkit.NewOrg(t)

	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		return insertNote(t.Context(), tx, "kept")
	}))

	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Equal(t, 1, n)
		return err
	}))
}

func TestWithTenantRollsBackOnError(t *testing.T) {
	probeTable(t)
	pool, org := testkit.Pool(t), testkit.NewOrg(t)
	boom := errors.New("boom")

	err := database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		require.NoError(t, insertNote(t.Context(), tx, "discarded"))
		return boom
	})
	require.ErrorIs(t, err, boom)

	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Zero(t, n)
		return err
	}))
}

func TestWithTenantRollsBackAndRepanics(t *testing.T) {
	probeTable(t)
	pool, org := testkit.Pool(t), testkit.NewOrg(t)

	require.PanicsWithValue(t, "kaboom", func() {
		_ = database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
			require.NoError(t, insertNote(t.Context(), tx, "discarded"))
			panic("kaboom")
		})
	})

	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Zero(t, n)
		return err
	}))
}

func TestWithTenantRejectsInvalidOrganization(t *testing.T) {
	pool := testkit.Pool(t)
	for _, id := range []int64{0, -1} {
		err := database.WithTenant(t.Context(), pool, id, func(*db.Queries) error {
			t.Fatal("fn must not run")
			return nil
		})
		require.Error(t, err)
	}
}

// The tests below are the proof for LOCK F3 (tenant isolation). Keep them.

func TestIsolationRowsAreInvisibleToOtherTenants(t *testing.T) {
	probeTable(t)
	pool := testkit.Pool(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)

	require.NoError(t, database.WithTenantTx(t.Context(), pool, orgA, func(tx pgx.Tx) error {
		return insertNote(t.Context(), tx, "a-secret")
	}))

	require.NoError(t, database.WithTenantTx(t.Context(), pool, orgB, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Zero(t, n, "org B must not see org A's rows")
		return err
	}))
	require.NoError(t, database.WithTenantTx(t.Context(), pool, orgA, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Equal(t, 1, n)
		return err
	}))
}

func TestIsolationRejectsWritingAnotherTenantsRow(t *testing.T) {
	probeTable(t)
	pool := testkit.Pool(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)

	err := database.WithTenantTx(t.Context(), pool, orgA, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(),
			"INSERT INTO isolation_probe (organization_id, note) VALUES ($1, 'planted')", orgB)
		return err
	})
	require.Error(t, err)
	require.True(t, database.IsRLSViolation(err), "want SQLSTATE 42501, got %v", err)
}

func TestIsolationWithoutTenantSeesNothing(t *testing.T) {
	probeTable(t)
	pool := testkit.Pool(t)
	org := testkit.NewOrg(t)
	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		return insertNote(t.Context(), tx, "hidden")
	}))

	var n int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM isolation_probe").Scan(&n))
	require.Zero(t, n, "a query outside WithTenant must see no tenant rows")
}

func TestIsolationSettingDoesNotLeakAcrossPooledConnections(t *testing.T) {
	probeTable(t)
	// One connection, so org A's transaction and org B's transaction share it.
	pool, err := database.Open(t.Context(), testkit.AppURL(t), database.WithMaxConns(1))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)

	require.NoError(t, database.WithTenantTx(t.Context(), pool, orgA, func(tx pgx.Tx) error {
		return insertNote(t.Context(), tx, "a-only")
	}))
	require.NoError(t, database.WithTenantTx(t.Context(), pool, orgB, func(tx pgx.Tx) error {
		n, err := countNotes(t.Context(), tx)
		require.Zero(t, n, "org B reused org A's connection and must see only its own rows")
		return err
	}))

	// And a bare query on the same connection afterwards sees no tenant at all.
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM isolation_probe").Scan(&n))
	require.Zero(t, n)
}

// unprotectedTables lists tables in public that have an organization_id column but lack
// RLS, FORCE RLS or the org_isolation policy.
func unprotectedTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'organization_id' AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p')
		  AND NOT (c.relrowsecurity
		           AND c.relforcerowsecurity
		           AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid AND p.polname = 'org_isolation'))
		ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Guard test: a tenant table added by any later migration without RLS fails here.
func TestEveryTenantTableIsProtectedByRLS(t *testing.T) {
	probeTable(t)
	owner := testkit.OwnerPool(t)

	bad, err := unprotectedTables(t.Context(), owner)
	require.NoError(t, err)
	require.Empty(t, bad, "tables with organization_id but without ENABLE+FORCE RLS and the org_isolation policy")

	t.Run("the guard detects an unprotected table", func(t *testing.T) {
		_, err := owner.Exec(t.Context(), "CREATE TABLE unprotected_probe (organization_id BIGINT)")
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = owner.Exec(context.Background(), "DROP TABLE unprotected_probe") })

		bad, err := unprotectedTables(t.Context(), owner)
		require.NoError(t, err)
		require.Equal(t, []string{"unprotected_probe"}, bad)
	})
}

func TestOrganizationsRegistry(t *testing.T) {
	testkit.Reset(t)
	pool := testkit.Pool(t)

	var first db.Organization
	require.NoError(t, database.WithoutTenant(t.Context(), pool, func(q *db.Queries) (err error) {
		first, err = q.UpsertOrganizationSeen(t.Context(), db.UpsertOrganizationSeenParams{ID: 77, Name: "Acme"})
		return err
	}))
	require.EqualValues(t, 77, first.ID)
	require.Equal(t, db.OrganizationStatusACTIVE, first.Status)

	// A second sighting with a new name updates the name and keeps first_seen_at.
	var second db.Organization
	require.NoError(t, database.WithoutTenant(t.Context(), pool, func(q *db.Queries) (err error) {
		second, err = q.UpsertOrganizationSeen(t.Context(), db.UpsertOrganizationSeenParams{ID: 77, Name: "Acme Ltd"})
		return err
	}))
	require.Equal(t, "Acme Ltd", second.Name)
	require.True(t, first.FirstSeenAt.Time.Equal(second.FirstSeenAt.Time))

	require.NoError(t, database.WithoutTenant(t.Context(), pool, func(q *db.Queries) error {
		got, err := q.GetOrganization(t.Context(), 77)
		require.Equal(t, "Acme Ltd", got.Name)
		return err
	}))

	t.Run("the app role cannot delete a tenant", func(t *testing.T) {
		_, err := pool.Exec(t.Context(), "DELETE FROM organizations WHERE id = 77")
		require.Error(t, err)
		require.True(t, database.IsRLSViolation(err), "want SQLSTATE 42501 (permission denied), got %v", err)
	})
}

func TestErrorClassifiers(t *testing.T) {
	testkit.Reset(t)
	pool := testkit.Pool(t)
	_, err := pool.Exec(t.Context(), "INSERT INTO organizations (id, name) VALUES (5, 'x')")
	require.NoError(t, err)

	_, err = pool.Exec(t.Context(), "INSERT INTO organizations (id, name) VALUES (5, 'dup')")
	require.True(t, database.IsUniqueViolation(err))
	require.Equal(t, "organizations_pkey", database.ConstraintName(err))
	require.False(t, database.IsExclusionViolation(err))
	require.False(t, database.IsRLSViolation(err))

	require.False(t, database.IsUniqueViolation(errors.New("plain")))
	require.Empty(t, database.ConstraintName(nil))
}

func TestResetEmptiesTablesAndRestartsIdentities(t *testing.T) {
	probeTable(t)
	pool, org := testkit.Pool(t), testkit.NewOrg(t)
	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		return insertNote(t.Context(), tx, "x")
	}))

	testkit.Reset(t)

	var orgs int
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM organizations").Scan(&orgs))
	require.Zero(t, orgs)

	org = testkit.NewOrg(t)
	var id int64
	require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), "INSERT INTO isolation_probe (note) VALUES ('y') RETURNING id").Scan(&id)
	}))
	require.EqualValues(t, 1, id, "identity must restart after Reset")
}
