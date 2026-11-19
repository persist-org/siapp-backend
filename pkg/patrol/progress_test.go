package patrol

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
)

func TestProgressOfFoldsRowsIntoZones(t *testing.T) {
	cp1 := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	cp2 := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	rows := []db.ZoneProgressRow{
		{ID: 1, Name: "Gerbang", MinVisit: 2, CheckpointID: cp1, CheckpointName: "Pintu", Position: 1, Submitted: 1, Approved: 1},
		{ID: 1, Name: "Gerbang", MinVisit: 2, CheckpointID: cp2, CheckpointName: "Pagar", Position: 2, Declined: 1},
		{ID: 2, Name: "Kosong", MinVisit: 3}, // no active checkpoint: one row with none
	}
	got := progressOf(rows)
	require.Len(t, got, 2)

	gate := got[0]
	require.Equal(t, int64(1), gate.Zone.ID)
	require.Equal(t, 4, gate.Required, "min_visit of each of its checkpoints")
	require.Equal(t, 1, gate.Submitted)
	require.Equal(t, 1, gate.Approved)
	require.Equal(t, 1, gate.Declined)
	require.Len(t, gate.Checkpoints, 2)
	require.Equal(t, "Pintu", gate.Checkpoints[0].Name)
	require.Equal(t, 1, gate.Checkpoints[0].SortOrder)
	require.Equal(t, 2, gate.Checkpoints[0].Required)
	require.Equal(t, 1, gate.Checkpoints[1].Declined)

	empty := got[1]
	require.NotNil(t, empty.Checkpoints)
	require.Empty(t, empty.Checkpoints)
	require.Zero(t, empty.Required+empty.Submitted+empty.Approved+empty.Declined)
	require.Equal(t, 3, empty.Zone.MinVisit, "the zone's own minimum is still shown")
}
