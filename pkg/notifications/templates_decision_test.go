package notifications_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
)

// PATROL_APPROVED and GUARD_APPROVED are one type for both outcomes (data.decision tells them
// apart), so the text must say which one it is.
func TestPatrolDecisionTexts(t *testing.T) {
	approved := notifications.Vars{ActorName: "Budi", Subject: "Patroli malam"}
	declined := notifications.Vars{ActorName: "Budi", Subject: "Patroli malam", Declined: true, Note: "foto buram"}

	for _, typ := range []notifications.Type{notifications.TypePatrolApproved, notifications.TypeGuardApproved} {
		title, body := notifications.Render(typ, approved)
		require.Contains(t, title, "disetujui", typ)
		require.Contains(t, body, "disetujui oleh Budi", typ)
		require.NotContains(t, body, "Catatan", typ)

		title, body = notifications.Render(typ, declined)
		require.Contains(t, title, "ditolak", typ)
		require.Contains(t, body, "ditolak oleh Budi", typ)
		require.Contains(t, body, "Catatan: foto buram", typ)
		require.NotContains(t, body, "disetujui", typ)
	}

	t.Run("a decline without a note, or without a name, still reads well", func(t *testing.T) {
		_, body := notifications.Render(notifications.TypePatrolApproved, notifications.Vars{Declined: true})
		require.Equal(t, "Patroli Anda ditolak.", body)
	})

	t.Run("the cleaning line uses its own words", func(t *testing.T) {
		title, _ := notifications.Render(notifications.TypePatrolApproved, notifications.Vars{BusinessLine: notifications.LineCleaning, Declined: true})
		require.Contains(t, title, "ditolak")
		require.NotContains(t, title, "Patroli ", "cleaning does not call it a patrol")
	})
}
