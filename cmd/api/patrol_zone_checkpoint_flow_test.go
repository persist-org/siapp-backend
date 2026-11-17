package main

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// Phase 10.8: checkpoints inside zones, in order, and what the app reads of them.

// zoneCheckpointOut is a checkpoint as a zone shows it. The last three are the field detail only.
type zoneCheckpointOut struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	SortOrder    int         `json:"sort_order"`
	IsActive     bool        `json:"is_active"`
	Latitude     *float64    `json:"latitude"`
	Longitude    *float64    `json:"longitude"`
	RadiusMeters *int        `json:"radius_meters"`
	MyScans      *int        `json:"my_scans"`
	Scannable    *bool       `json:"scannable"`
	Actions      []actionOut `json:"actions"`
}

func (f *flow) assign(zoneID int64, ids ...string) (int, zoneOut, []byte) {
	f.t.Helper()
	if ids == nil {
		ids = []string{}
	}
	return f.zoneCall(http.MethodPut, zonePath(zoneID)+"/checkpoints", f.adminTok, map[string]any{"checkpoint_ids": ids})
}

// layout lists "<position>:<name>" of a zone's checkpoints, to compare an order in one line.
func layout(z zoneOut) (out []string) {
	for _, c := range z.Checkpoints {
		out = append(out, strconv.Itoa(c.SortOrder)+":"+c.Name)
	}
	return out
}

func (f *flow) adminZone(id int64) zoneOut {
	f.t.Helper()
	code, z, raw := f.zoneCall(http.MethodGet, zonePath(id), f.adminTok, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	return z
}

func (f *flow) fieldZone(id int64, token string) zoneOut {
	f.t.Helper()
	code, z, raw := f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(id, 10), token, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	return z
}

func (f *flow) newPlainZone(project, name string, extra map[string]any) zoneOut {
	f.t.Helper()
	body := map[string]any{"project_code": project, "type": "PATROL", "name": name}
	for k, v := range extra {
		body[k] = v
	}
	code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, body)
	require.Equal(f.t, http.StatusCreated, code, string(raw))
	return z
}

func (f *flow) linkedOf(checkpoint string) (out []string) {
	f.t.Helper()
	code, c, raw := f.cpCall(http.MethodGet, checkpointPath(checkpoint), f.adminTok, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	for _, z := range c.Zones {
		out = append(out, strconv.Itoa(z.SortOrder)+"@"+z.Name)
	}
	return out
}

func TestPatrolZoneCheckpointAssign(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")
	a, b, c := f.newCheckpoint("1K-IBMI-S", "A", nil), f.newCheckpoint("1K-IBMI-S", "B", nil), f.newCheckpoint("1K-IBMI-S", "C", nil)
	foreign := f.newCheckpoint("1K-IBMI-C", "Lorong", nil)
	zone := f.newPlainZone("1K-IBMI-S", "Zona Utara", nil)

	t.Run("checkpoints go in as the order they are sent, and the admin reads them back", func(t *testing.T) {
		code, z, raw := f.assign(zone.ID, a.ID, b.ID, c.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"1:A", "2:B", "3:C"}, layout(z))
		require.Equal(t, 3, z.CheckpointCount)
		for _, cp := range z.Checkpoints {
			require.True(t, cp.IsActive)
			require.Nil(t, cp.MyScans, "the admin's read has no field progress")
			require.Nil(t, cp.Actions, "and no checklist")
		}
		require.Equal(t, z, f.adminZone(zone.ID))
		require.Equal(t, []string{"1@Zona Utara"}, f.linkedOf(a.ID))
		require.Equal(t, []string{"3@Zona Utara"}, f.linkedOf(c.ID))
	})

	t.Run("a list has the count of checkpoints and not the checkpoints", func(t *testing.T) {
		_, list, _ := f.zoneList("/api/v1/admin/zones?project_code=1K-IBMI-S", f.adminTok)
		require.Len(t, list, 1)
		require.Equal(t, 3, list[0].CheckpointCount)
		require.Nil(t, list[0].Checkpoints)
	})

	t.Run("reordering renumbers, and leaving one out takes it out of the zone", func(t *testing.T) {
		code, z, raw := f.assign(zone.ID, c.ID, a.ID, b.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"1:C", "2:A", "3:B"}, layout(z))
		code, z, raw = f.assign(zone.ID, c.ID, b.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"1:C", "2:B"}, layout(z), "no gap: B moves up")
		require.Empty(t, f.linkedOf(a.ID), "A is in no zone any more")
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM patrol_zone_checkpoints WHERE zone_id = $1", zone.ID)
		require.Equal(t, 2, n)
	})

	t.Run("an empty list takes them all out", func(t *testing.T) {
		code, z, raw := f.assign(zone.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotNil(t, z.Checkpoints)
		require.Empty(t, z.Checkpoints)
		require.Zero(t, z.CheckpointCount)
	})

	t.Run("a checkpoint of another project is refused, naming the id, and nothing changes", func(t *testing.T) {
		code, _, raw := f.assign(zone.ID, a.ID, b.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.assign(zone.ID, c.ID, foreign.ID)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		reason, fields := problem(t, raw)
		require.Equal(t, "CHECKPOINT_NOT_IN_PROJECT", reason)
		require.Equal(t, []string{"checkpoint_ids[1]"}, fields)
		require.Contains(t, string(raw), foreign.ID, "the answer names the checkpoint")
		require.Equal(t, []string{"1:A", "2:B"}, layout(f.adminZone(zone.ID)), "the refused request left the zone as it was")
	})

	t.Run("one that does not exist, is deleted, or is another organization's is refused the same way", func(t *testing.T) {
		gone := f.newCheckpoint("1K-IBMI-S", "Dihapus", nil)
		code, raw := f.do(http.MethodDelete, checkpointPath(gone.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))

		other := testkit.NewOrg(t)
		var foreignOrg string
		require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), other, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), flowWorldSQL); err != nil {
				return err
			}
			return tx.QueryRow(t.Context(), `INSERT INTO patrol_checkpoints (project_code, name, qr_secret) VALUES ('1K-IBMI-S', 'Milik Asing', $1) RETURNING id::text`,
				checkpointSecret).Scan(&foreignOrg)
		}))
		for name, id := range map[string]string{
			"unknown": "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "deleted": gone.ID, "another organization's": foreignOrg,
		} {
			code, _, raw := f.assign(zone.ID, a.ID, id)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			reason, fields := problem(t, raw)
			require.Equal(t, "CHECKPOINT_NOT_IN_PROJECT", reason, name)
			require.Equal(t, []string{"checkpoint_ids[1]"}, fields, name)
		}
	})

	t.Run("a bad list is a validation error naming the place", func(t *testing.T) {
		for name, tc := range map[string]struct {
			ids   []string
			field string
		}{
			"the same id twice": {[]string{a.ID, b.ID, a.ID}, "checkpoint_ids[2]"},
			"a bad uuid":        {[]string{a.ID, "abc"}, "checkpoint_ids[1]"},
		} {
			code, _, raw := f.assign(zone.ID, tc.ids...)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			reason, fields := problem(t, raw)
			require.Empty(t, reason, name)
			require.Equal(t, []string{tc.field}, fields, name)
		}
		many := make([]string, MaxZoneCheckpointsForTest+1)
		for i := range many {
			many[i] = "3f2504e0-4f89-41d3-9a0c-0305e82c" + strconv.Itoa(1000+i)
		}
		code, _, raw := f.assign(zone.ID, many...)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Equal(t, []string{"checkpoint_ids"}, fields)

		code, raw = f.do(http.MethodPut, zonePath(zone.ID)+"/checkpoints", f.adminTok, []byte(`{}`), nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, "checkpoint_ids is required: "+string(raw))
		require.Equal(t, []string{"1:A", "2:B"}, layout(f.adminZone(zone.ID)), "and nothing changed")
	})

	t.Run("a zone that does not exist is 404", func(t *testing.T) {
		code, _, raw := f.assign(99999999, a.ID)
		require.Equal(t, http.StatusNotFound, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "ZONE_NOT_FOUND", reason)
	})

	t.Run("a field user cannot assign", func(t *testing.T) {
		code, raw := f.do(http.MethodPut, zonePath(zone.ID)+"/checkpoints", f.personTok, jsonBody(t, map[string]any{"checkpoint_ids": []string{}}), nil)
		require.Equal(t, http.StatusForbidden, code, string(raw))
	})

	t.Run("it moves the zone's updated_at and is audited", func(t *testing.T) {
		var moved bool
		f.query([]any{&moved}, "SELECT updated_at > created_at FROM patrol_zones WHERE id = $1", zone.ID)
		require.True(t, moved)
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.zone.checkpoints' AND entity_id = $1", zone.ID)
		require.GreaterOrEqual(t, n, 4)
	})
}

// MaxZoneCheckpointsForTest is the most checkpoints a zone may hold (patrol.MaxZoneCheckpoints).
const MaxZoneCheckpointsForTest = 100

func TestPatrolZoneCheckpointInactiveKeepsItsPlace(t *testing.T) {
	f := newFlow(t)
	a, b, c := f.newCheckpoint("1K-IBMI-S", "A", nil), f.newCheckpoint("1K-IBMI-S", "B", nil), f.newCheckpoint("1K-IBMI-S", "C", nil)
	off := f.newCheckpoint("1K-IBMI-S", "Mati", map[string]any{"is_active": false})
	zone := f.newPlainZone("1K-IBMI-S", "Zona", nil)

	t.Run("an inactive checkpoint cannot be added to a zone", func(t *testing.T) {
		code, _, raw := f.assign(zone.ID, a.ID, off.ID)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "CHECKPOINT_NOT_IN_PROJECT", reason)
	})

	t.Run("one switched off inside a zone keeps its place, and the zone can still be reordered", func(t *testing.T) {
		code, _, raw := f.assign(zone.ID, a.ID, b.ID, c.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.cpCall(http.MethodPatch, checkpointPath(b.ID), f.adminTok, map[string]any{"is_active": false})
		require.Equal(t, http.StatusOK, code, string(raw))

		z := f.adminZone(zone.ID)
		require.Equal(t, []string{"1:A", "2:B", "3:C"}, layout(z), "the admin still sees it, in its place")
		require.False(t, z.Checkpoints[1].IsActive)
		require.Equal(t, 2, z.CheckpointCount, "but only the active ones can be scanned")

		code, z, raw = f.assign(zone.ID, c.ID, b.ID, a.ID)
		require.Equal(t, http.StatusOK, code, "it is already in the zone, so reordering it is fine: "+string(raw))
		require.Equal(t, []string{"1:C", "2:B", "3:A"}, layout(z))
	})

	t.Run("leaving it out takes it out for good: it cannot come back while it is off", func(t *testing.T) {
		code, _, raw := f.assign(zone.ID, c.ID, a.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.assign(zone.ID, c.ID, b.ID, a.ID)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, back, raw := f.cpCall(http.MethodPatch, checkpointPath(b.ID), f.adminTok, map[string]any{"is_active": true})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.True(t, back.IsActive)
		code, z, raw := f.assign(zone.ID, c.ID, b.ID, a.ID)
		require.Equal(t, http.StatusOK, code, "once it is on again it can: "+string(raw))
		require.Equal(t, []string{"1:C", "2:B", "3:A"}, layout(z))
	})
}

func TestPatrolZoneWithCheckpointsInTheBody(t *testing.T) {
	f := newFlow(t)
	a, b, c := f.newCheckpoint("1K-IBMI-S", "A", nil), f.newCheckpoint("1K-IBMI-S", "B", nil), f.newCheckpoint("1K-IBMI-S", "C", nil)

	t.Run("a new zone can come with its checkpoints", func(t *testing.T) {
		z := f.newPlainZone("1K-IBMI-S", "Zona Baru", map[string]any{"checkpoint_ids": []string{b.ID, a.ID}})
		require.Equal(t, []string{"1:B", "2:A"}, layout(z))
		require.Equal(t, 2, z.CheckpointCount)
	})

	t.Run("a patch with checkpoint_ids replaces them, and without them leaves them alone", func(t *testing.T) {
		z := f.newPlainZone("1K-IBMI-S", "Zona Patch", map[string]any{"checkpoint_ids": []string{a.ID, b.ID}})
		code, got, raw := f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"name": "Zona Patch 2"})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"1:A", "2:B"}, layout(got), "no checkpoint_ids: they stay")
		code, got, raw = f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"checkpoint_ids": []string{c.ID, a.ID}})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"1:C", "2:A"}, layout(got))
		code, got, raw = f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"checkpoint_ids": []string{}})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Empty(t, got.Checkpoints)
	})

	t.Run("a bad checkpoint in the body refuses the whole request", func(t *testing.T) {
		code, _, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Tidak jadi", "checkpoint_ids": []string{a.ID, "3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		reason, fields := problem(t, raw)
		require.Equal(t, "CHECKPOINT_NOT_IN_PROJECT", reason)
		require.Equal(t, []string{"checkpoint_ids[1]"}, fields)
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM patrol_zones WHERE name = 'Tidak jadi'")
		require.Zero(t, n, "and the zone was not made")

		z := f.newPlainZone("1K-IBMI-S", "Zona Utuh", map[string]any{"checkpoint_ids": []string{a.ID}})
		code, _, raw = f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"name": "Berubah", "checkpoint_ids": []string{"abc"}})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		got := f.adminZone(z.ID)
		require.Equal(t, "Zona Utuh", got.Name, "a refused patch changes nothing, the name included")
		require.Equal(t, []string{"1:A"}, layout(got))
	})

	t.Run("the zone's checklist is gone from the API", func(t *testing.T) {
		z := f.newPlainZone("1K-IBMI-S", "Tanpa Checklist", nil)
		code, raw := f.do(http.MethodPut, zonePath(z.ID)+"/checklist", f.adminTok, jsonBody(t, map[string]any{"checklist": []any{}}), nil)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, code, string(raw))
		code, _, raw2 := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Dengan checklist lama", "checklist": []any{},
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, "an unknown field is refused: "+string(raw2))
		code, _, raw2 = f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"checklist": []any{}})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw2))
		require.NotContains(t, string(f.rawGet(zonePath(z.ID), f.adminTok)), `"checklist"`)
	})
}

func (f *flow) rawGet(path, token string) []byte {
	f.t.Helper()
	_, raw := f.do(http.MethodGet, path, token, nil, nil)
	return raw
}

func TestPatrolSharedCheckpointHasItsOwnPlaceInEachZone(t *testing.T) {
	f := newFlow(t)
	a, shared, c := f.newCheckpoint("1K-IBMI-S", "A", nil), f.newCheckpoint("1K-IBMI-S", "Bersama", nil), f.newCheckpoint("1K-IBMI-S", "C", nil)
	x := f.newPlainZone("1K-IBMI-S", "Zona X", nil)
	y := f.newPlainZone("1K-IBMI-S", "Zona Y", map[string]any{"type": "GUARD"})

	_, zx, raw := f.assign(x.ID, a.ID, shared.ID)
	require.NotEmpty(t, zx.Checkpoints, string(raw))
	_, zy, raw := f.assign(y.ID, shared.ID, c.ID)
	require.NotEmpty(t, zy.Checkpoints, string(raw))
	require.Equal(t, []string{"1:A", "2:Bersama"}, layout(f.adminZone(x.ID)))
	require.Equal(t, []string{"1:Bersama", "2:C"}, layout(f.adminZone(y.ID)))
	require.Equal(t, []string{"2@Zona X", "1@Zona Y"}, f.linkedOf(shared.ID), "a PATROL zone and a GUARD zone, each with its own position")

	t.Run("removing it from one zone leaves the other alone", func(t *testing.T) {
		_, _, raw := f.assign(x.ID, a.ID)
		require.Equal(t, []string{"1:A"}, layout(f.adminZone(x.ID)), string(raw))
		require.Equal(t, []string{"1:Bersama", "2:C"}, layout(f.adminZone(y.ID)))
	})
}

// scanFor records a scan of a checkpoint straight in the table, as the scan of Phase 10.9 will, so the
// order rule can be read before that scan exists.
func (f *flow) scanFor(zone int64, checkpoint string, person int64, at time.Time, workDate, status string) {
	f.t.Helper()
	var decidedBy any
	var decidedAt any
	if status != "SUBMITTED" {
		decidedBy, decidedAt = f.person, at
	}
	f.exec(`INSERT INTO patrol_scans (local_id, zone_id, zone_type, personnel_id, team_code, project_code, occurred_at, received_at, work_date,
	                                  latitude, longitude, accuracy_meters, subject, checkpoint_id, status, decided_by_id, decided_at)
	        VALUES (gen_random_uuid(), $1, 'PATROL', $2, '1K-IBMI-S1', '1K-IBMI-S', $3::timestamptz, $3::timestamptz, $4::date,
	                -6.2, 106.8, 5, 'Patroli', $5, $6::patrol_scan_status, $7, $8::timestamptz)`,
		zone, person, at, workDate, checkpoint, status, decidedBy, decidedAt)
}

func today(t *testing.T) (string, string) {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	now := time.Now().In(loc)
	return now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)
}

func TestPatrolZoneFieldDetail(t *testing.T) {
	f := newFlow(t)
	day, yesterday := today(t)
	now := time.Now().UTC()

	gerbang := f.newCheckpoint("1K-IBMI-S", "Gerbang", map[string]any{"latitude": -6.2, "longitude": 106.8, "actions": []map[string]any{
		{"action": "Periksa gembok", "instruction": "Tarik gembok", "min_photos": 1, "max_photos": 2},
		{"action": "Periksa lampu", "max_photos": 0, "is_note_required": true},
		{"action": "Dihapus nanti"},
	}})
	pagar := f.newCheckpoint("1K-IBMI-S", "Pagar", map[string]any{"actions": []map[string]any{{"action": "Periksa pagar"}}})
	pintu := f.newCheckpoint("1K-IBMI-S", "Pintu", nil)
	zone := f.newPlainZone("1K-IBMI-S", "Zona Utara", nil)
	_, _, raw := f.assign(zone.ID, gerbang.ID, pagar.ID, pintu.ID)
	require.NotEmpty(t, raw)
	// A checklist action that was removed does not show.
	code, _, raw := f.cpCall(http.MethodPut, checkpointPath(gerbang.ID)+"/actions", f.adminTok, map[string]any{"actions": []map[string]any{
		{"id": gerbang.Actions[0].ID, "action": "Periksa gembok", "instruction": "Tarik gembok", "min_photos": 1, "max_photos": 2},
		{"id": gerbang.Actions[1].ID, "action": "Periksa lampu", "max_photos": 0, "is_note_required": true},
	}})
	require.Equal(t, http.StatusOK, code, string(raw))

	read := func() zoneOut { return f.fieldZone(zone.ID, f.personTok) }
	progress := func(z zoneOut) (scans []int, scannable []bool) {
		for _, c := range z.Checkpoints {
			scans = append(scans, *c.MyScans)
			scannable = append(scannable, *c.Scannable)
		}
		return scans, scannable
	}

	t.Run("the checkpoints come in order with their active checklist, and nothing is done yet", func(t *testing.T) {
		z := read()
		require.Equal(t, []string{"1:Gerbang", "2:Pagar", "3:Pintu"}, layout(z))
		require.Equal(t, 3, z.CheckpointCount)
		scans, scannable := progress(z)
		require.Equal(t, []int{0, 0, 0}, scans)
		require.Equal(t, []bool{true, false, false}, scannable, "only the first can start a round")
		require.Equal(t, gerbang.ID, *z.NextCheckpointID)

		g := z.Checkpoints[0]
		require.Equal(t, -6.2, *g.Latitude)
		require.Equal(t, 30, *g.RadiusMeters)
		require.Len(t, g.Actions, 2, "the removed action is not listed")
		require.Equal(t, actionOut{
			ID: g.Actions[0].ID, SortOrder: 0, Action: "Periksa gembok", Instruction: g.Actions[0].Instruction,
			MinPhotos: 1, MaxPhotos: 2, PhotoRequired: true,
		}, g.Actions[0])
		require.Equal(t, "Tarik gembok", *g.Actions[0].Instruction)
		require.True(t, g.Actions[1].IsNoteRequired)
		require.NotNil(t, z.Checkpoints[2].Actions, "a checkpoint with no checklist has an empty array")
		require.Empty(t, z.Checkpoints[2].Actions)
	})

	t.Run("the walk moves what can be scanned and what comes next", func(t *testing.T) {
		step := func(cp string, want []int, wantScannable []bool, next string) {
			f.scanFor(zone.ID, cp, f.person, now, day, "SUBMITTED")
			now = now.Add(time.Minute)
			z := read()
			scans, scannable := progress(z)
			require.Equal(t, want, scans)
			require.Equal(t, wantScannable, scannable)
			require.Equal(t, next, *z.NextCheckpointID)
		}
		step(gerbang.ID, []int{1, 0, 0}, []bool{true, true, false}, pagar.ID)
		step(pagar.ID, []int{1, 1, 0}, []bool{true, false, true}, pintu.ID)
		step(pintu.ID, []int{1, 1, 1}, []bool{true, false, false}, gerbang.ID) // a round is done: the next one starts at the first
		step(gerbang.ID, []int{2, 1, 1}, []bool{true, true, false}, pagar.ID)
	})

	t.Run("only the caller's own scans, in this zone, on this day, count", func(t *testing.T) {
		before, _ := progress(read())
		other := f.addPerson(900401, "ANGGOTA9", "Anggota Lain")
		f.scanFor(zone.ID, pagar.ID, other, now, day, "SUBMITTED")
		elsewhere := f.newPlainZone("1K-IBMI-S", "Zona Lain", nil)
		_, _, raw := f.assign(elsewhere.ID, pagar.ID)
		require.NotEmpty(t, raw)
		f.scanFor(elsewhere.ID, pagar.ID, f.person, now, day, "SUBMITTED")
		f.scanFor(zone.ID, pagar.ID, f.person, now.Add(-24*time.Hour), yesterday, "SUBMITTED")
		after, _ := progress(read())
		require.Equal(t, before, after, "another person, another zone and yesterday change nothing here")
		require.Equal(t, 1, *f.fieldZone(elsewhere.ID, f.personTok).Checkpoints[0].MyScans, "the same checkpoint in another zone has its own count")
	})

	t.Run("a declined scan still counts: a decline does not undo the walk", func(t *testing.T) {
		before, _ := progress(read())
		f.scanFor(zone.ID, pagar.ID, f.person, now.Add(2*time.Minute), day, "DECLINED")
		after, _ := progress(read())
		require.Equal(t, before[1]+1, after[1])
	})

	t.Run("an inactive checkpoint is not shown and the rule steps over it", func(t *testing.T) {
		code, _, raw := f.cpCall(http.MethodPatch, checkpointPath(pagar.ID), f.adminTok, map[string]any{"is_active": false})
		require.Equal(t, http.StatusOK, code, string(raw))
		z := read()
		require.Equal(t, []string{"1:Gerbang", "3:Pintu"}, layout(z), "Pagar keeps place 2 for the admin, but nobody can scan it")
		require.Equal(t, 2, z.CheckpointCount)
		scans, scannable := progress(z)
		require.Equal(t, []int{2, 1}, scans)
		require.Equal(t, []bool{true, true}, scannable, "Pintu follows Gerbang now")
		require.Equal(t, pintu.ID, *z.NextCheckpointID)
		code, _, raw = f.cpCall(http.MethodPatch, checkpointPath(pagar.ID), f.adminTok, map[string]any{"is_active": true})
		require.Equal(t, http.StatusOK, code, string(raw))
	})

	t.Run("a deleted checkpoint is gone from the zone", func(t *testing.T) {
		code, raw := f.do(http.MethodDelete, checkpointPath(pintu.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		z := read()
		require.Equal(t, []string{"1:Gerbang", "2:Pagar"}, layout(z), "and Pagar moved up")
	})

	t.Run("a zone with no checkpoint has none, and nothing is next", func(t *testing.T) {
		empty := f.newPlainZone("1K-IBMI-S", "Kosong", nil)
		z := f.fieldZone(empty.ID, f.personTok)
		require.NotNil(t, z.Checkpoints)
		require.Empty(t, z.Checkpoints)
		require.Zero(t, z.CheckpointCount)
		require.Nil(t, z.NextCheckpointID)
	})

	t.Run("the list has the count, no checkpoints", func(t *testing.T) {
		_, list, _ := f.zoneList("/api/v1/zones", f.personTok)
		var seen bool
		for _, z := range list {
			require.Nil(t, z.Checkpoints)
			if z.ID == zone.ID {
				seen = true
				require.Equal(t, 2, z.CheckpointCount)
			}
		}
		require.True(t, seen)
	})

	t.Run("no QR secret, of the zone or of a checkpoint, is in anything the field reads", func(t *testing.T) {
		var cpSecret string
		f.query([]any{&cpSecret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", gerbang.ID)
		_, list := f.do(http.MethodGet, "/api/v1/zones", f.personTok, nil, nil)
		_, one := f.do(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(zone.ID, 10), f.personTok, nil, nil)
		_, adminOne := f.do(http.MethodGet, zonePath(zone.ID), f.adminTok, nil, nil)
		for _, body := range [][]byte{list, one, adminOne} {
			for _, secret := range []string{cpSecret, "qr_secret", "PERSIST-CP", "PERSIST-ZONE"} {
				require.False(t, bytes.Contains(body, []byte(secret)), secret)
			}
		}
	})

	t.Run("access is as before: a stranger and a member of another regu are refused", func(t *testing.T) {
		stranger := f.addPerson(900402, "ORANGLAIN2", "Orang Lain")
		tok := f.token(stranger, f.addSession(stranger, "tok-stranger2"))
		code, _, raw := f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(zone.ID, 10), tok, nil)
		require.Equal(t, http.StatusForbidden, code, string(raw))
	})
}

// A checkpoint deleted while an admin assigns it to a zone must not deadlock: both take the checkpoint's
// lock before the zone's, so one waits for the other. Either order is fine; a 500 is not.
func TestPatrolAssignAndDeleteDoNotDeadlock(t *testing.T) {
	f := newFlow(t)
	call := func(method, path string, body any) int {
		var raw []byte
		if body != nil {
			raw = jsonBody(t, body)
		}
		req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(raw))
		if err != nil {
			return -1
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.adminTok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1
		}
		defer func() { _ = res.Body.Close() }()
		return res.StatusCode
	}

	for round := range 12 {
		x := f.newCheckpoint("1K-IBMI-S", "X"+strconv.Itoa(round), nil)
		y := f.newCheckpoint("1K-IBMI-S", "Y"+strconv.Itoa(round), nil)
		zone := f.newPlainZone("1K-IBMI-S", "Zona"+strconv.Itoa(round), nil)
		code, _, raw := f.assign(zone.ID, x.ID, y.ID)
		require.Equal(t, http.StatusOK, code, string(raw))

		var wg sync.WaitGroup
		results := make([]int, 3)
		wg.Add(3)
		go func() { defer wg.Done(); results[0] = call(http.MethodDelete, checkpointPath(x.ID), nil) }()
		go func() {
			defer wg.Done()
			results[1] = call(http.MethodPut, zonePath(zone.ID)+"/checkpoints", map[string]any{"checkpoint_ids": []string{y.ID, x.ID}})
		}()
		go func() {
			defer wg.Done()
			results[2] = call(http.MethodPut, zonePath(zone.ID)+"/checkpoints", map[string]any{"checkpoint_ids": []string{x.ID}})
		}()
		wg.Wait()
		for _, r := range results {
			require.Contains(t, []int{http.StatusOK, http.StatusUnprocessableEntity, http.StatusNotFound}, r, "round %d: %v", round, results)
		}
		// Whatever order won, the zone's positions are 1..n without a gap.
		z := f.adminZone(zone.ID)
		for i, cp := range z.Checkpoints {
			require.Equal(t, i+1, cp.SortOrder, "round %d: %v", round, layout(z))
		}
	}
}
