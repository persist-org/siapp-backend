package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Phase 10.10: progress, lists and counts per checkpoint.

func TestPatrolProgressPerCheckpoint(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedRound(3, map[string]any{"min_visit": 2})
	first, second, third := z.all[0], z.all[1], z.all[2]

	// Walked in order: the first checkpoint twice, the second once, the third not at all.
	a := f.mustScan(f.personTok, z, scanBodyOf(first, first.payload))
	b := f.mustScan(f.personTok, z, scanBodyOf(second, second.payload))
	f.mustScan(f.personTok, z, scanBodyOf(first, first.payload))
	code, raw := f.approve(danruTok, a.ID)
	require.Equal(t, http.StatusOK, code, string(raw))
	code, raw = f.decline(danruTok, b.ID, "foto kurang jelas")
	require.Equal(t, http.StatusOK, code, string(raw))

	for _, tok := range []string{f.personTok, danruTok} {
		_, list, _ := f.progress(tok, "")
		p := progressOf(t, list, z.ID)
		require.Len(t, p.Checkpoints, 3)
		require.Equal(t, 6, p.Required, "min_visit of every checkpoint: 2 x 3")
		require.Equal(t, 1, p.Submitted)
		require.Equal(t, 1, p.Approved)
		require.Equal(t, 1, p.Declined)

		want := []struct{ submitted, approved, declined int }{{1, 1, 0}, {0, 0, 1}, {0, 0, 0}}
		for i, c := range p.Checkpoints {
			require.Equal(t, z.all[i].ID, c.CheckpointID, "in the order of the zone")
			require.Equal(t, z.all[i].Name, c.Name)
			require.Equal(t, i+1, c.SortOrder)
			require.Equal(t, 2, c.Required)
			require.Equal(t, want[i].submitted, c.Submitted, "checkpoint %d", i+1)
			require.Equal(t, want[i].approved, c.Approved, "checkpoint %d", i+1)
			require.Equal(t, want[i].declined, c.Declined, "checkpoint %d", i+1)
		}
	}
	_ = third

	t.Run("an inactive checkpoint leaves the progress, and its minimum with it", func(t *testing.T) {
		f.exec("UPDATE patrol_checkpoints SET active = false WHERE id = $1", third.ID)
		_, list, _ := f.progress(f.personTok, "")
		p := progressOf(t, list, z.ID)
		require.Len(t, p.Checkpoints, 2)
		require.Equal(t, 4, p.Required)
	})

	t.Run("a zone with no active checkpoint has an empty list and needs nothing", func(t *testing.T) {
		f.exec("UPDATE patrol_checkpoints SET active = false WHERE id = ANY($1::uuid[])", []string{first.ID, second.ID})
		_, list, _ := f.progress(f.personTok, "")
		p := progressOf(t, list, z.ID)
		require.NotNil(t, p.Checkpoints)
		require.Empty(t, p.Checkpoints)
		require.Zero(t, p.Required+p.Submitted+p.Approved+p.Declined)
	})
}

func TestPatrolViewsFilterByCheckpoint(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedRound(2, nil)
	first, second := z.all[0], z.all[1]
	s1 := f.mustScan(f.personTok, z, scanBodyOf(first, first.payload))
	s2 := f.mustScan(f.personTok, z, scanBodyOf(second, second.payload))
	f.mustScan(f.personTok, z, scanBodyOf(first, first.payload))

	t.Run("the list of the zone's scans can be narrowed to one checkpoint", func(t *testing.T) {
		_, all, _ := f.getScans(danruTok, zoneURL(z.ID, "/scans"))
		require.Len(t, all, 3)

		code, list, raw := f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?checkpoint_id="+second.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 1)
		require.Equal(t, s2.ID, list[0].ID)
		require.Equal(t, second.ID, list[0].Checkpoint.ID)

		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?checkpoint_id="+first.ID)
		require.Len(t, list, 2)
		for _, s := range list {
			require.Equal(t, first.ID, s.Checkpoint.ID)
		}
		_ = s1

		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?checkpoint_id=00000000-0000-0000-0000-000000000000")
		require.Empty(t, list, "a checkpoint with no scan of this zone has none to show")
	})

	t.Run("the scanners count only the scans of the checkpoint", func(t *testing.T) {
		counts := func(query string) scannerOut {
			code, raw := f.do(http.MethodGet, zoneURL(z.ID, "/scanners")+query, danruTok, nil, nil)
			require.Equal(t, http.StatusOK, code, string(raw))
			var env struct {
				Data []scannerOut `json:"data"`
			}
			require.NoError(t, json.Unmarshal(raw, &env), string(raw))
			require.Len(t, env.Data, 1)
			return env.Data[0]
		}
		require.Equal(t, 3, counts("").SubmittedCount)
		require.Equal(t, 2, counts("?checkpoint_id="+first.ID).SubmittedCount)
		require.Equal(t, 1, counts("?checkpoint_id="+second.ID).SubmittedCount)
	})

	t.Run("an id that is not a uuid is refused", func(t *testing.T) {
		for _, path := range []string{"/scans", "/scanners"} {
			code, raw := f.do(http.MethodGet, zoneURL(z.ID, path)+"?checkpoint_id="+url.QueryEscape("nope"), danruTok, nil, nil)
			require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		}
	})
}

func TestPatrolSharedCheckpointCountsPerZone(t *testing.T) {
	f := newFlow(t)
	a := f.fencedRound(1, nil)
	b := f.fencedRound(1, nil)
	shared := a.all[0]
	// The checkpoint also stands first in zone B (a link is all it takes).
	f.exec("DELETE FROM patrol_zone_checkpoints WHERE zone_id = $1", b.ID)
	f.exec(`INSERT INTO patrol_zone_checkpoints (zone_id, checkpoint_id, project_code, sort_order)
	        SELECT id, $2::uuid, project_code, 1 FROM patrol_zones WHERE id = $1`, b.ID, shared.ID)
	b.all, b.checkpoint = []patrolCheckpoint{shared}, shared

	f.mustScan(f.personTok, a, scanBodyOf(shared, shared.payload))

	_, list, _ := f.progress(f.personTok, "")
	require.Equal(t, 1, progressOf(t, list, a.ID).Submitted)
	require.Equal(t, 1, progressOf(t, list, a.ID).Checkpoints[0].Submitted)
	require.Zero(t, progressOf(t, list, b.ID).Submitted, "a scan made in zone A does not count for zone B")
	require.Zero(t, progressOf(t, list, b.ID).Checkpoints[0].Submitted)
}

func TestPatrolScanViewKeepsWhatWasAnswered(t *testing.T) {
	f := newFlow(t)
	danruID, danruTok := f.danru()
	_ = danruID
	z := f.fencedZone([]map[string]any{{"action": "Periksa gembok", "instruction": "Tarik gemboknya"}}, nil)
	scan := f.mustScan(f.personTok, z, scanBody(z))

	// The checklist changes after the scan: the action is renamed.
	f.exec("UPDATE patrol_checkpoint_actions SET action = 'Teks baru', instruction = 'Instruksi baru' WHERE id = $1", z.checkpoint.Actions[0].ID)

	code, got, raw := f.getScan(danruTok, scan.ID)
	require.Equal(t, http.StatusOK, code, string(raw))
	require.Len(t, got.Actions, 1)
	require.Equal(t, "Periksa gembok", got.Actions[0].Action, "the scan shows the action as it was answered")
	require.NotNil(t, got.Actions[0].Instruction)
	require.Equal(t, "Tarik gemboknya", *got.Actions[0].Instruction)
}

func TestPatrolPendingListShowsCheckpointAndActions(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone([]map[string]any{{"action": "Periksa pagar"}}, nil)
	scan := f.mustScan(f.personTok, z, scanBody(z))
	_, colleagueTok := f.addMember("1K-IBMI-S1", "MEMBER", 900710, "KOLEGA0010", "Kolega Sepuluh")
	f.mustScan(colleagueTok, z, scanBody(z))

	_, list, _ := f.getScans(danruTok, "/api/v1/patrol/pending-approvals")
	require.Len(t, list, 2)
	for _, s := range list {
		require.Equal(t, z.checkpoint.ID, s.Checkpoint.ID)
		require.Equal(t, z.checkpoint.Name, s.Checkpoint.Name)
		require.Len(t, s.Actions, 1)
		require.Equal(t, "Periksa pagar", s.Actions[0].Action)
	}

	_, own, _ := f.getScans(f.personTok, "/api/v1/patrol/pending-approvals")
	for _, s := range own {
		require.Equal(t, f.person, s.PersonnelID, "a member still sees only their own scans")
	}
	_ = scan
}

func TestPatrolBulkApproveOfARound(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedRound(3, nil)
	var ids []int64
	for _, cp := range z.all {
		ids = append(ids, f.mustScan(f.personTok, z, scanBodyOf(cp, cp.payload)).ID)
	}

	code, got, raw := f.approveMany(danruTok, ids...)
	require.Equal(t, http.StatusOK, code, string(raw))
	require.Len(t, got.Approved, 3)
	require.Empty(t, got.Skipped)

	_, list, _ := f.progress(f.personTok, "")
	p := progressOf(t, list, z.ID)
	require.Equal(t, 3, p.Approved)
	require.Zero(t, p.Submitted)
	for _, c := range p.Checkpoints {
		require.Equal(t, 1, c.Approved, "every checkpoint's counter moved")
	}
	require.Equal(t, 1, f.countInbox(f.personTok, "PATROL_APPROVED"), "three scans of one author, one notification")
}
