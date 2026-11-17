package main

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// The schema of Phase 10.6: checkpoints, their links to zones, their checklist and the answers.
// Nothing here goes through the API (it comes in 10.7 and later); these tests hold what the
// database alone must refuse.

const checkpointSecret = "abcdefghijklmnopqrstuv" // 22 characters, like a real secret

func (f *flow) addProject(code, line string) {
	f.t.Helper()
	f.exec(`INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ($1, '1K-IBMI', 'x', $2, 'Asia/Jakarta')`, code, line)
}

func (f *flow) addZone(project, name string) (id int64) {
	f.t.Helper()
	f.query([]any{&id}, `INSERT INTO patrol_zones (project_code, type, name) VALUES ($1, 'PATROL', $2) RETURNING id`,
		project, name)
	return id
}

func (f *flow) addCheckpoint(project, name string) (id string) {
	f.t.Helper()
	f.query([]any{&id}, `INSERT INTO patrol_checkpoints (project_code, name, qr_secret) VALUES ($1, $2, $3) RETURNING id::text`,
		project, name, checkpointSecret)
	return id
}

func (f *flow) addAction(checkpoint, action string, minPhotos, maxPhotos int) (id string) {
	f.t.Helper()
	f.query([]any{&id}, `INSERT INTO patrol_checkpoint_actions (checkpoint_id, action, min_photos, max_photos) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		checkpoint, action, minPhotos, maxPhotos)
	return id
}

func (f *flow) link(project string, zone int64, checkpoint string, position int) error {
	f.t.Helper()
	return f.tryExec(`INSERT INTO patrol_zone_checkpoints (project_code, zone_id, checkpoint_id, sort_order) VALUES ($1, $2, $3, $4)`,
		project, zone, checkpoint, position)
}

func (f *flow) addScan(zone int64, checkpoint string, at time.Time) (id int64) {
	f.t.Helper()
	f.query([]any{&id}, `INSERT INTO patrol_scans (local_id, zone_id, zone_type, personnel_id, team_code, project_code, occurred_at,
	                                               received_at, work_date, latitude, longitude, accuracy_meters, subject, checkpoint_id)
	                     VALUES (gen_random_uuid(), $1, 'PATROL', $2, '1K-IBMI-S1', '1K-IBMI-S', $3::timestamptz, $3::timestamptz, ($3::timestamptz)::date, -6.2, 106.8, 5, 'Patroli', $4)
	                     RETURNING id`, zone, f.person, at, checkpoint)
	return id
}

func TestCheckpointSchemaRefusesWhatTheDatabaseOwns(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")
	zone := f.addZone("1K-IBMI-S", "Zona Utara")
	security := f.addCheckpoint("1K-IBMI-S", "Gerbang")

	t.Run("an id is generated", func(t *testing.T) {
		require.Len(t, security, 36, "a uuid")
		require.NotEqual(t, security, f.addCheckpoint("1K-IBMI-S", "Pagar"))
	})

	t.Run("a checkpoint", func(t *testing.T) {
		insert := `INSERT INTO patrol_checkpoints (project_code, name, latitude, longitude, radius_meters, qr_secret) VALUES ($1, $2, $3, $4, $5, $6)`
		for name, tc := range map[string]struct {
			args []any
			ok   bool
		}{
			"with a full geofence":           {[]any{"1K-IBMI-S", "A1", -6.2, 106.8, 30, checkpointSecret}, true},
			"without any geofence":           {[]any{"1K-IBMI-S", "A2", nil, nil, nil, checkpointSecret}, true},
			"a point without a radius":       {[]any{"1K-IBMI-S", "A3", -6.2, 106.8, nil, checkpointSecret}, false},
			"a radius without a point":       {[]any{"1K-IBMI-S", "A4", nil, nil, 30, checkpointSecret}, false},
			"half a point":                   {[]any{"1K-IBMI-S", "A5", -6.2, nil, 30, checkpointSecret}, false},
			"a latitude out of range":        {[]any{"1K-IBMI-S", "A6", 91.0, 106.8, 30, checkpointSecret}, false},
			"a zero radius":                  {[]any{"1K-IBMI-S", "A7", -6.2, 106.8, 0, checkpointSecret}, false},
			"a secret of the wrong length":   {[]any{"1K-IBMI-S", "A8", nil, nil, nil, "short"}, false},
			"an empty name":                  {[]any{"1K-IBMI-S", "", nil, nil, nil, checkpointSecret}, false},
			"a project that does not exist":  {[]any{"9K-NONE-S", "A9", nil, nil, nil, checkpointSecret}, false},
			"the name of another, any case":  {[]any{"1K-IBMI-S", "GERBANG", nil, nil, nil, checkpointSecret}, false},
			"the same name in other project": {[]any{"1K-IBMI-C", "Gerbang", nil, nil, nil, checkpointSecret}, true},
		} {
			err := f.tryExec(insert, tc.args...)
			if tc.ok {
				require.NoError(t, err, name)
			} else {
				require.Error(t, err, name)
			}
		}
	})

	t.Run("a deleted checkpoint frees its name and cannot stay active", func(t *testing.T) {
		old := f.addCheckpoint("1K-IBMI-S", "Pos Lama")
		require.Error(t, f.tryExec(`UPDATE patrol_checkpoints SET deleted_at = now() WHERE id = $1`, old), "deleted but still active")
		f.exec(`UPDATE patrol_checkpoints SET active = false, deleted_at = now() WHERE id = $1`, old)
		f.addCheckpoint("1K-IBMI-S", "Pos Lama")
	})

	t.Run("a link", func(t *testing.T) {
		other := f.addCheckpoint("1K-IBMI-S", "Pagar Barat")
		third := f.addCheckpoint("1K-IBMI-S", "Pintu")
		cleaning := f.addCheckpoint("1K-IBMI-C", "Lorong")

		require.NoError(t, f.link("1K-IBMI-S", zone, security, 1))
		require.NoError(t, f.link("1K-IBMI-S", zone, other, 2))
		require.Error(t, f.link("1K-IBMI-S", zone, security, 3), "the same checkpoint twice in a zone")
		require.Error(t, f.link("1K-IBMI-S", zone, third, 2), "two checkpoints at one position")
		require.Error(t, f.link("1K-IBMI-S", zone, third, 0), "a position below 1")
		require.Error(t, f.link("1K-IBMI-C", zone, cleaning, 3), "a zone of another project than the link says")
		require.Error(t, f.link("1K-IBMI-S", zone, cleaning, 3), "a checkpoint of another project than the link says")
		require.NoError(t, f.link("1K-IBMI-S", zone, third, 3))
	})

	t.Run("a checkpoint can be in several zones, with its own position in each", func(t *testing.T) {
		cp := f.addCheckpoint("1K-IBMI-S", "Dua Zona")
		a, b := f.addZone("1K-IBMI-S", "Zona A"), f.addZone("1K-IBMI-S", "Zona B")
		require.NoError(t, f.link("1K-IBMI-S", a, cp, 1))
		require.NoError(t, f.link("1K-IBMI-S", b, cp, 4))
	})

	t.Run("an action", func(t *testing.T) {
		insert := `INSERT INTO patrol_checkpoint_actions (checkpoint_id, action, min_photos, max_photos) VALUES ($1, $2, $3, $4)`
		for name, tc := range map[string]struct {
			action   string
			min, max int
			ok       bool
		}{
			"a photo required":            {"Cek gembok", 1, 2, true},
			"no photo at all":             {"Cek buku", 0, 0, true},
			"the most photos":             {"Cek panel", 5, 5, true},
			"more minimum than maximum":   {"Cek lampu", 2, 1, false},
			"more than five photos":       {"Cek pagar", 0, 6, false},
			"a negative minimum":          {"Cek pintu", -1, 1, false},
			"an empty action":             {"", 0, 1, false},
			"a minimum that needs a slot": {"Cek atap", 1, 0, false},
		} {
			err := f.tryExec(insert, security, tc.action, tc.min, tc.max)
			if tc.ok {
				require.NoError(t, err, name)
			} else {
				require.Error(t, err, name)
			}
		}
		require.Error(t, f.tryExec(`INSERT INTO patrol_checkpoint_actions (checkpoint_id, action, active, deactivated_at) VALUES ($1, 'x', false, NULL)`, security),
			"inactive without a time")
	})
}

func TestScanActionsAreEvidence(t *testing.T) {
	f := newFlow(t)
	zone := f.addZone("1K-IBMI-S", "Zona Utara")
	cp := f.addCheckpoint("1K-IBMI-S", "Gerbang")
	action := f.addAction(cp, "Cek gembok", 1, 2)
	require.NoError(t, f.link("1K-IBMI-S", zone, cp, 1))
	scan := f.addScan(zone, cp, time.Now().UTC())

	insert := `INSERT INTO patrol_scan_actions (scan_id, checkpoint_id, action_id, personnel_id, note, media_ids, snapshot)
	           VALUES ($1, $2, $3, $4, 'aman', '{11,12}', $5::jsonb)`
	snapshot := `{"action":"Cek gembok","instruction":null,"min_photos":1,"max_photos":2,"is_note_required":false,"sort_order":0,"note":"aman","media_ids":[11,12]}`

	require.NoError(t, f.tryExec(insert, scan, cp, action, f.person, snapshot))
	require.Error(t, f.tryExec(insert, scan, cp, action, f.person, snapshot), "an action is answered once per scan")
	for name, bad := range map[string]string{
		"a snapshot without the note":   `{"action":"Cek gembok","media_ids":[]}`,
		"a snapshot without the photos": `{"action":"Cek gembok","note":null}`,
		"a snapshot that is an array":   `[]`,
	} {
		other := f.addAction(cp, "Cek "+name, 0, 1)
		require.Error(t, f.tryExec(insert, scan, cp, other, f.person, bad), name)
	}

	t.Run("the app role cannot change or delete an answer", func(t *testing.T) {
		require.Error(t, f.tryExec(`UPDATE patrol_scan_actions SET note = 'diubah'`))
		require.Error(t, f.tryExec(`DELETE FROM patrol_scan_actions`))
	})

	t.Run("the queries read it back, in the order of the checklist", func(t *testing.T) {
		second := f.addAction(cp, "Cek lampu", 0, 1)
		f.exec(`UPDATE patrol_checkpoint_actions SET sort_order = 1 WHERE id = $1`, second)
		require.NoError(t, f.tryExec(insert, scan, cp, second, f.person, strings.Replace(snapshot, `"sort_order":0`, `"sort_order":1`, 1)))
		var got []db.PatrolScanAction
		require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), f.org, func(q *db.Queries) (err error) {
			got, err = q.ListScanActionsByScans(t.Context(), []int64{scan})
			return err
		}))
		require.Len(t, got, 2)
		require.Equal(t, []int64{11, 12}, got[0].MediaIds)
		require.JSONEq(t, snapshot, string(got[0].Snapshot))
	})
}

func TestCountScansByPositionReadsTheOrderRule(t *testing.T) {
	f := newFlow(t)
	zone := f.addZone("1K-IBMI-S", "Zona Utara")
	other := f.addZone("1K-IBMI-S", "Zona Lain")
	first, second, third := f.addCheckpoint("1K-IBMI-S", "Satu"), f.addCheckpoint("1K-IBMI-S", "Dua"), f.addCheckpoint("1K-IBMI-S", "Tiga")
	require.NoError(t, f.link("1K-IBMI-S", zone, first, 1))
	require.NoError(t, f.link("1K-IBMI-S", zone, second, 2))
	require.NoError(t, f.link("1K-IBMI-S", zone, third, 3))
	require.NoError(t, f.link("1K-IBMI-S", other, second, 1))

	noon := time.Date(2026, 11, 17, 5, 0, 0, 0, time.UTC)
	f.addScan(zone, first, noon)
	f.addScan(zone, second, noon.Add(time.Minute))
	f.addScan(zone, first, noon.Add(2*time.Minute))
	f.addScan(other, second, noon.Add(3*time.Minute)) // another zone: does not count here
	f.addScan(zone, third, noon.Add(time.Hour))       // after the moment asked about

	count := func(before time.Time) []db.CountScansByPositionRow {
		var rows []db.CountScansByPositionRow
		require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), f.org, func(q *db.Queries) (err error) {
			rows, err = q.CountScansByPosition(t.Context(), db.CountScansByPositionParams{
				PersonnelID: f.person, WorkDate: pgtype.Date{Time: noon, Valid: true}, Before: pgtype.Timestamptz{Time: before, Valid: true}, ZoneID: zone,
			})
			return err
		}))
		return rows
	}
	scans := func(rows []db.CountScansByPositionRow) (out []int32) {
		for _, r := range rows {
			out = append(out, r.Scans)
		}
		return out
	}

	rows := count(noon.Add(30 * time.Minute))
	require.Equal(t, []int32{2, 1, 0}, scans(rows), "one row per position, 0 for what was not scanned, other zones left out")
	require.Equal(t, []int32{1, 2, 3}, []int32{rows[0].Position, rows[1].Position, rows[2].Position})
	require.Equal(t, []int32{2, 1, 1}, scans(count(noon.Add(2*time.Hour))), "a later moment sees the later scan")
	require.Equal(t, []int32{0, 0, 0}, scans(count(noon)), "nothing happened before noon")

	t.Run("a removed or inactive checkpoint has no position", func(t *testing.T) {
		f.exec(`UPDATE patrol_checkpoints SET active = false WHERE id = $1`, second)
		require.Equal(t, []int32{2, 0}, scans(count(noon.Add(30*time.Minute))))
	})
}

func TestCheckpointsStayInsideTheirOrganization(t *testing.T) {
	f := newFlow(t)
	zone := f.addZone("1K-IBMI-S", "Zona Utara")
	cp := f.addCheckpoint("1K-IBMI-S", "Gerbang")
	f.addAction(cp, "Cek gembok", 1, 2)
	require.NoError(t, f.link("1K-IBMI-S", zone, cp, 1))
	scan := f.addScan(zone, cp, time.Now().UTC())
	require.NoError(t, f.tryExec(`INSERT INTO patrol_scan_actions (scan_id, checkpoint_id, action_id, personnel_id, snapshot)
	                              SELECT $1, $2, id, $3, '{"action":"x","note":null,"media_ids":[]}' FROM patrol_checkpoint_actions`, scan, cp, f.person))

	intruder := testkit.NewOrg(t)
	for _, table := range []string{"patrol_checkpoints", "patrol_zone_checkpoints", "patrol_checkpoint_actions", "patrol_scan_actions"} {
		var mine, theirs int
		f.query([]any{&mine}, `SELECT count(*) FROM `+table)
		require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), intruder, func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&theirs)
		}))
		require.Equal(t, 1, mine, table)
		require.Zero(t, theirs, "%s: another organization sees nothing", table)
	}

	// Writing a row for another organization is refused by the policy, not silently dropped.
	err := database.WithTenantTx(t.Context(), testkit.Pool(t), intruder, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO patrol_checkpoints (organization_id, project_code, name, qr_secret) VALUES ($1, '1K-IBMI-S', 'Curang', $2)`,
			f.org, checkpointSecret)
		return err
	})
	require.Error(t, err)
}
