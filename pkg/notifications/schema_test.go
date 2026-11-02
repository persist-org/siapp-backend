package notifications_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// enumLabels returns the labels of a Postgres enum, in the order of the enum.
func enumLabels(t *testing.T, enum string) []string {
	t.Helper()
	rows, err := testkit.OwnerPool(t).Query(t.Context(), "SELECT unnest(enum_range(NULL::"+enum+"))::text")
	require.NoError(t, err)
	labels, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return labels
}

// A type added to the enum by a migration, and forgotten in AllTypes, would be skipped by the
// render test that walks AllTypes. This fails first.
func TestAllTypesMatchTheDatabaseEnum(t *testing.T) {
	var fromGo []string
	for _, typ := range notifications.AllTypes() {
		fromGo = append(fromGo, string(typ))
	}
	require.ElementsMatch(t, enumLabels(t, "notification_type"), fromGo)

	require.ElementsMatch(t, enumLabels(t, "notification_priority"),
		[]string{string(notifications.PriorityNormal), string(notifications.PriorityHigh)})
}

// tryExec runs one statement as the app role in an organization and returns its error.
func (f *flow) tryExec(org int64, sql string, args ...any) error {
	f.t.Helper()
	return database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.t.Context(), sql, args...)
		return err
	})
}

// The table defends itself: whatever a caller, a script or a later phase does, these rows cannot
// be written.
func TestTableRules(t *testing.T) {
	f := newFlow(t)
	_, person := f.person(f.orgA)

	// $1 is the recipient in every statement.
	rejected := []struct {
		name      string
		recipient int64
		sql       string
		want      string
	}{
		{"an empty title", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body) VALUES ($1, 'TEAM_CHANGED', '', 'x')", "notifications_text_check"},
		{"an empty body", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body) VALUES ($1, 'TEAM_CHANGED', 'x', '')", "notifications_text_check"},
		{"an entity type without an id", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_type) VALUES ($1, 'TEAM_CHANGED', 't', 'b', 'panic')", "notifications_entity_check"},
		{"an entity id without a type", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body, entity_id) VALUES ($1, 'TEAM_CHANGED', 't', 'b', 4)", "notifications_entity_check"},
		{"data that is not an object", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body, data) VALUES ($1, 'TEAM_CHANGED', 't', 'b', '[1]')", "notifications_data_check"},
		{"a type that is not in the enum", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body) VALUES ($1, 'NOT_A_TYPE', 't', 'b')", "notification_type"},
		{"a priority that is not in the enum", person, "INSERT INTO notifications (recipient_personnel_id, type, title, body, priority) VALUES ($1, 'TEAM_CHANGED', 't', 'b', 'URGENT')", "notification_priority"},
		{"a recipient that does not exist", 999999, "INSERT INTO notifications (recipient_personnel_id, type, title, body) VALUES ($1, 'TEAM_CHANGED', 't', 'b')", "notifications_recipient_personnel_id_fkey"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, f.tryExec(f.orgA, tc.sql, tc.recipient), tc.want)
		})
	}

	t.Run("a good row is accepted, with the defaults", func(t *testing.T) {
		require.NoError(t, f.tryExec(f.orgA, "INSERT INTO notifications (recipient_personnel_id, type, title, body) VALUES ($1, 'TEAM_CHANGED', 't', 'b')", person))
		f.inTenant(f.orgA, func(tx pgx.Tx) {
			var priority, data string
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT priority::text, data::text FROM notifications").Scan(&priority, &data))
			require.Equal(t, "NORMAL", priority)
			require.Equal(t, "{}", data)
		})
	})
}

// Row-level security, attacked directly with SQL as the app role: the HTTP tests prove the
// endpoints are careful, this proves the table does not depend on them.
func TestRowLevelSecurity(t *testing.T) {
	f := newFlow(t)
	tokenA, a := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	f.seed(f.orgA, actor, a, tokenA, 2)
	_, b := f.person(f.orgB)

	t.Run("another organization sees none of the rows", func(t *testing.T) {
		require.Zero(t, f.count(f.orgB))
		require.Equal(t, 2, f.count(f.orgA))
	})

	t.Run("with no organization set, no row is visible", func(t *testing.T) {
		var n int
		require.NoError(t, testkit.Pool(t).QueryRow(t.Context(), "SELECT count(*) FROM notifications").Scan(&n))
		require.Zero(t, n)
	})

	t.Run("a row cannot be written for another organization", func(t *testing.T) {
		err := f.tryExec(f.orgA,
			"INSERT INTO notifications (organization_id, recipient_personnel_id, type, title, body) VALUES ($1, $2, 'TEAM_CHANGED', 't', 'b')", f.orgB, b)
		require.ErrorContains(t, err, "row-level security")
	})

	t.Run("another organization cannot change or delete a row", func(t *testing.T) {
		f.inTenant(f.orgB, func(tx pgx.Tx) {
			tag, err := tx.Exec(t.Context(), "UPDATE notifications SET read_at = now()")
			require.NoError(t, err)
			require.Zero(t, tag.RowsAffected())
			tag, err = tx.Exec(t.Context(), "DELETE FROM notifications")
			require.NoError(t, err)
			require.Zero(t, tag.RowsAffected())
		})
		require.Equal(t, 2, f.count(f.orgA))
	})
}
