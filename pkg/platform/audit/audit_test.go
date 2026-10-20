package audit_test

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

func ptr[T any](v T) *T { return &v }

func record(t *testing.T, org int64, e audit.Entry) {
	t.Helper()
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		return audit.Record(t.Context(), q, e)
	}))
}

func entries(t *testing.T, org int64) []db.AuditLog {
	t.Helper()
	var out []db.AuditLog
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) (err error) {
		out, err = q.ListAuditLogsByEntity(t.Context(), db.ListAuditLogsByEntityParams{EntityType: "absence", EntityID: pgtype.Int8{Int64: 7, Valid: true}, Limit: 10})
		return err
	}))
	return out
}

func TestRecord(t *testing.T) {
	testkit.Reset(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)

	record(t, orgA, audit.Entry{
		ActorPersonnelID: ptr(int64(15)), Action: "attendance.absence.create",
		EntityType: "absence", EntityID: 7, Metadata: map[string]any{"status": "APPROVED", "days": 2},
	})

	got := entries(t, orgA)
	require.Len(t, got, 1)
	require.Equal(t, orgA, got[0].OrganizationID)
	require.EqualValues(t, 15, got[0].ActorPersonnelID.Int64)
	require.Equal(t, "attendance.absence.create", got[0].Action)
	require.JSONEq(t, `{"status":"APPROVED","days":2}`, string(got[0].Metadata))
	require.WithinDuration(t, time.Now(), got[0].CreatedAt.Time, time.Minute)

	require.Empty(t, entries(t, orgB), "another organization must not see the audit row")

	t.Run("a system action has no actor and empty metadata", func(t *testing.T) {
		record(t, orgA, audit.Entry{Action: "job.run", EntityType: "absence", EntityID: 7})
		got := entries(t, orgA)
		require.Len(t, got, 2)
		require.False(t, got[0].ActorPersonnelID.Valid)
		require.JSONEq(t, `{}`, string(got[0].Metadata))
	})

	t.Run("action and entity type are required", func(t *testing.T) {
		err := database.WithTenant(t.Context(), testkit.Pool(t), orgA, func(q *db.Queries) error {
			return audit.Record(t.Context(), q, audit.Entry{EntityType: "absence"})
		})
		require.Error(t, err)
	})
}

func TestRecordCommitsAndRollsBackWithTheChange(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)
	boom := errors.New("the change failed")

	err := database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		require.NoError(t, audit.Record(t.Context(), q, audit.Entry{Action: "x.y.z", EntityType: "absence", EntityID: 7}))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Empty(t, entries(t, org), "an audit row for a change that rolled back must not exist")
}

func TestRecordRejectsSensitiveMetadata(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)
	pool := testkit.Pool(t)

	bad := []map[string]any{
		{"password": "x"},
		{"new_password": "x"},
		{"accessToken": "x"},
		{"refresh-token": "x"},
		{"NIK": "3201"},
		{"camera_url": "rtsp://cam"},
		{"nested": map[string]any{"authorization": "Bearer x"}},
		{"list": []any{map[string]any{"secret": "x"}}},
	}
	for _, md := range bad {
		err := database.WithTenant(t.Context(), pool, org, func(q *db.Queries) error {
			return audit.Record(t.Context(), q, audit.Entry{Action: "a.b.c", EntityType: "absence", EntityID: 7, Metadata: md})
		})
		require.ErrorIs(t, err, audit.ErrSensitiveMetadata, "%v", md)
	}
	require.Empty(t, entries(t, org))

	t.Run("ordinary keys are fine", func(t *testing.T) {
		record(t, org, audit.Entry{Action: "a.b.c", EntityType: "absence", EntityID: 7,
			Metadata: map[string]any{"from": "PENDING", "to": "APPROVED", "nickname": "Bud", "reason": "sakit"}})
		require.Len(t, entries(t, org), 1)
	})
}

func partitions(t *testing.T) []string {
	t.Helper()
	rows, err := testkit.OwnerPool(t).Query(t.Context(), `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'audit_logs'::regclass ORDER BY c.relname`)
	require.NoError(t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return names
}

func TestAuditLogsAreAppendOnly(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)
	record(t, org, audit.Entry{Action: "a.b.c", EntityType: "absence", EntityID: 7})
	pool := testkit.Pool(t)

	targets := append([]string{"audit_logs"}, partitions(t)...)
	require.GreaterOrEqual(t, len(targets), 4, "the parent and the first three months")

	for _, table := range targets {
		for _, stmt := range []string{
			fmt.Sprintf("UPDATE %s SET action = 'tampered'", table),
			fmt.Sprintf("DELETE FROM %s", table),
		} {
			err := database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
				_, err := tx.Exec(t.Context(), stmt)
				return err
			})
			require.Error(t, err, stmt)
			require.True(t, database.IsRLSViolation(err), "%s: want SQLSTATE 42501, got %v", stmt, err)
		}
	}
	require.Len(t, entries(t, org), 1, "the row is untouched")
}

func TestPartitionsAreProtectedWhenQueriedDirectly(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)
	record(t, org, audit.Entry{Action: "a.b.c", EntityType: "absence", EntityID: 7})

	// A partition can be addressed by name, bypassing the parent's policy; it has its own.
	current := "audit_logs_" + time.Now().UTC().Format("2006_01")
	var n int
	require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT count(*) FROM "+current).Scan(&n))
	require.Zero(t, n, "outside a tenant a partition shows nothing")
}

func TestEnsurePartitions(t *testing.T) {
	testkit.Reset(t)
	pool := testkit.Pool(t)

	// The migration made the current month and two more; ensuring adds a fourth, once.
	require.NoError(t, audit.EnsurePartitions(t.Context(), pool, clock.System{}))
	require.NoError(t, audit.EnsurePartitions(t.Context(), pool, clock.System{}))
	require.Len(t, partitions(t), 4, "current month and the next three")

	// Moving the clock forward creates later months.
	later := clock.Fixed{T: time.Now().UTC().AddDate(0, 6, 0)}
	require.NoError(t, audit.EnsurePartitions(t.Context(), pool, later))
	after := partitions(t)
	require.Len(t, after, 8, "months +6..+9 are new")

	t.Run("new partitions have RLS, FORCE and the policy, and cannot be updated", func(t *testing.T) {
		newest := after[len(after)-1]
		var rls, force bool
		var policies int
		require.NoError(t, testkit.OwnerPool(t).QueryRow(t.Context(), `
			SELECT relrowsecurity, relforcerowsecurity,
			       (SELECT count(*) FROM pg_policy WHERE polrelid = c.oid AND polname = 'org_isolation')
			FROM pg_class c WHERE relname = $1`, newest).Scan(&rls, &force, &policies))
		require.True(t, rls)
		require.True(t, force)
		require.Equal(t, 1, policies)

		_, err := pool.Exec(t.Context(), "UPDATE "+newest+" SET action = 'x'")
		require.True(t, database.IsRLSViolation(err), "got %v", err)
	})

	t.Run("a month far from today is refused", func(t *testing.T) {
		far := clock.Fixed{T: time.Now().UTC().AddDate(5, 0, 0)}
		require.Error(t, audit.EnsurePartitions(t.Context(), pool, far))
	})

	t.Run("a row lands in the partition of its month", func(t *testing.T) {
		org := testkit.NewOrg(t)
		inMonth := time.Now().UTC().AddDate(0, 6, 0)
		require.NoError(t, database.WithTenantTx(t.Context(), pool, org, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				"INSERT INTO audit_logs (action, entity_type, entity_id, created_at) VALUES ('a.b.c', 'absence', 7, $1)", inMonth)
			return err
		}))
		var tableName string
		require.NoError(t, testkit.OwnerPool(t).QueryRow(t.Context(),
			"SELECT tableoid::regclass::text FROM audit_logs").Scan(&tableName))
		require.Equal(t, "audit_logs_"+inMonth.Format("2006_01"), tableName)
	})
}

// The hierarchy's rows have a code, not a number (D19), so an audit row can name its entity
// by code instead.
func TestRecordAnEntityByCode(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)

	record(t, org, audit.Entry{
		ActorPersonnelID: ptr(int64(15)), Action: "region.create", EntityType: "region", EntityCode: "OS1",
		Metadata: map[string]any{"after": map[string]any{"description": "Outsourcing 1"}},
	})

	var got []db.AuditLog
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) (err error) {
		got, err = q.ListAuditLogsByCode(t.Context(), db.ListAuditLogsByCodeParams{
			EntityType: "region", EntityCode: pgtype.Text{String: "OS1", Valid: true}, Limit: 10,
		})
		return err
	}))
	require.Len(t, got, 1)
	require.Equal(t, "region.create", got[0].Action)
	require.False(t, got[0].EntityID.Valid, "a coded entity has no number")
	require.Equal(t, "OS1", got[0].EntityCode.String)
	require.Empty(t, entries(t, org), "and it is not found by number")
}
