package audit_test

import (
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/audit"
	"siapp/internal/platform/database"
	"siapp/internal/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

type row struct {
	actorID    sql.NullString
	action     string
	entityType string
	entityID   string
	metadata   sql.NullString
	createdAt  time.Time
}

func rows(t *testing.T, pool *sql.DB) []row {
	t.Helper()
	res, err := pool.QueryContext(t.Context(),
		"SELECT actor_id, action, entity_type, entity_id, metadata, created_at FROM audit_logs ORDER BY created_at, id")
	require.NoError(t, err)
	defer func() { _ = res.Close() }()

	var out []row
	for res.Next() {
		var r row
		require.NoError(t, res.Scan(&r.actorID, &r.action, &r.entityType, &r.entityID, &r.metadata, &r.createdAt))
		out = append(out, r)
	}
	require.NoError(t, res.Err())
	return out
}

const (
	actor  = "0193a5d8-7c1e-7000-8000-000000000001"
	entity = "0193a5d8-7c1e-7000-8000-000000000002"
)

func TestRecord(t *testing.T) {
	pool := testkit.DB(t)
	at := time.Date(2026, 3, 10, 6, 30, 15, 123_000_000, time.FixedZone("WIT", 9*3600))

	t.Run("commits with the transaction", func(t *testing.T) {
		testkit.Reset(t, pool)
		err := database.WithTx(t.Context(), pool, func(q *db.Queries) error {
			return audit.Record(t.Context(), q, audit.Entry{
				ActorID: actor, Action: "attendance.absence.create", EntityType: "attendance", EntityID: entity,
				Metadata: map[string]any{"reason": "sakit", "days": 2}, At: at,
			})
		})
		require.NoError(t, err)

		got := rows(t, pool)
		require.Len(t, got, 1)
		require.Equal(t, actor, got[0].actorID.String)
		require.Equal(t, "attendance.absence.create", got[0].action)
		require.Equal(t, "attendance", got[0].entityType)
		require.Equal(t, entity, got[0].entityID)
		require.JSONEq(t, `{"reason":"sakit","days":2}`, got[0].metadata.String)
		require.True(t, got[0].createdAt.Equal(at), "created_at is the given instant, stored as UTC")
	})

	t.Run("rolls back with the transaction", func(t *testing.T) {
		testkit.Reset(t, pool)
		errBoom := errors.New("boom")
		err := database.WithTx(t.Context(), pool, func(q *db.Queries) error {
			require.NoError(t, audit.Record(t.Context(), q, audit.Entry{
				ActorID: actor, Action: "user.create", EntityType: "user", EntityID: entity,
			}))
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)
		require.Empty(t, rows(t, pool))
	})

	t.Run("system action and no metadata store NULL; zero time uses the database clock", func(t *testing.T) {
		testkit.Reset(t, pool)
		before := time.Now().UTC().Add(-time.Minute)
		require.NoError(t, audit.Record(t.Context(), db.New(pool), audit.Entry{
			Action: "auth.cleanup-sessions", EntityType: "job", EntityID: entity,
		}))

		got := rows(t, pool)
		require.Len(t, got, 1)
		require.False(t, got[0].actorID.Valid)
		require.False(t, got[0].metadata.Valid)
		require.True(t, got[0].createdAt.After(before), "created_at %s", got[0].createdAt)
	})

	t.Run("survives when the actor or entity does not exist (no foreign keys)", func(t *testing.T) {
		testkit.Reset(t, pool)
		require.NoError(t, audit.Record(t.Context(), db.New(pool), audit.Entry{
			ActorID: actor, Action: "user.delete", EntityType: "user", EntityID: entity,
		}))
		require.Len(t, rows(t, pool), 1)
	})

	t.Run("requires action, entity type and entity ID", func(t *testing.T) {
		testkit.Reset(t, pool)
		for _, e := range []audit.Entry{
			{EntityType: "user", EntityID: entity},
			{Action: "user.create", EntityID: entity},
			{Action: "user.create", EntityType: "user"},
		} {
			require.Error(t, audit.Record(t.Context(), db.New(pool), e))
		}
		require.Empty(t, rows(t, pool))
	})

	t.Run("rejects metadata that cannot be marshalled", func(t *testing.T) {
		testkit.Reset(t, pool)
		err := audit.Record(t.Context(), db.New(pool), audit.Entry{
			Action: "x.y", EntityType: "x", EntityID: entity, Metadata: make(chan int),
		})
		require.Error(t, err)
		require.Empty(t, rows(t, pool))
	})
}
