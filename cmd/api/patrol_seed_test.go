package main

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedStep6 reads the SQL of step 6 of scripts/seed-dev-server.sh (patrol zones, checkpoints and
// checklists) out of the real script, so this test breaks when the script does. It returns what writes
// (without its BEGIN / COMMIT and psql commands: the test runs it in its own transaction) and the
// query that lists each zone's checkpoints with their QR text. The demo project's codes are swapped for
// the test world's.
func seedStep6(t *testing.T, org int64) (writes, summary string) {
	t.Helper()
	raw, err := os.ReadFile("../../scripts/seed-dev-server.sh")
	require.NoError(t, err)
	script := string(raw)
	start := strings.Index(script, `say "6/8 Patrol zones, checkpoints and checklists"`)
	require.NotEqual(t, -1, start, "step 6 of the seed script was not found")
	script = script[start:]
	script = script[strings.Index(script, "<<'SQL'\n")+len("<<'SQL'\n"):]
	script = script[:strings.Index(script, "\nSQL\n")]

	// The demo regu and project become the test world's. The regu goes first: its code starts with the project's.
	script = strings.NewReplacer("1K-DEMO-S1", "1K-IBMI-S1", "1K-DEMO-S", "1K-IBMI-S").Replace(script)

	var write, read []string
	var inSummary bool
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, `\echo Checkpoints of each zone`):
			inSummary = true
		case strings.HasPrefix(trimmed, `\`), trimmed == "BEGIN;", trimmed == "COMMIT;", strings.HasPrefix(trimmed, "SELECT set_config("):
			// psql commands, the transaction and the tenant setting (the test's transaction has them).
		case inSummary:
			read = append(read, line)
		default:
			write = append(write, line)
		}
	}
	summary = strings.ReplaceAll(strings.Join(read, "\n"), ":'org'", "'"+strconv.FormatInt(org, 10)+"'")
	return strings.Join(write, "\n"), strings.TrimSpace(summary)
}

func TestSeedPatrolCheckpoints(t *testing.T) {
	f := newFlow(t)
	writes, summary := seedStep6(t, f.org)
	count := func(table string) (n int) {
		f.query([]any{&n}, "SELECT count(*) FROM "+table)
		return n
	}
	snapshot := func() []int {
		return []int{count("patrol_zones"), count("patrol_checkpoints"), count("patrol_zone_checkpoints"), count("patrol_checkpoint_actions")}
	}

	f.exec(writes)
	first := snapshot()
	require.Equal(t, []int{3, 5, 7, 7}, first, "three zones, five checkpoints, seven links, seven actions")

	t.Run("running it again changes nothing", func(t *testing.T) {
		var secrets string
		f.query([]any{&secrets}, "SELECT string_agg(qr_secret, ',' ORDER BY name) FROM patrol_checkpoints")
		f.exec(writes)
		require.Equal(t, first, snapshot())
		var again string
		f.query([]any{&again}, "SELECT string_agg(qr_secret, ',' ORDER BY name) FROM patrol_checkpoints")
		require.Equal(t, secrets, again, "the secrets of printed stickers are not remade")
	})

	t.Run("an admin's edits survive a re-run", func(t *testing.T) {
		f.exec(`UPDATE patrol_checkpoint_actions SET active = false, deactivated_at = now()
		        WHERE checkpoint_id = (SELECT id FROM patrol_checkpoints WHERE name = 'Pos Jaga')`)
		f.exec(`DELETE FROM patrol_zone_checkpoints WHERE zone_id = (SELECT id FROM patrol_zones WHERE name = 'Pos Belakang')`)
		f.exec(writes)
		var jaga int
		f.query([]any{&jaga}, "SELECT count(*) FROM patrol_checkpoint_actions WHERE checkpoint_id = (SELECT id FROM patrol_checkpoints WHERE name = 'Pos Jaga')")
		require.Equal(t, 1, jaga, "a checkpoint whose action was switched off does not get it back, nor a second one")
		var belakang int
		f.query([]any{&belakang}, "SELECT count(*) FROM patrol_zone_checkpoints WHERE zone_id = (SELECT id FROM patrol_zones WHERE name = 'Pos Belakang')")
		require.Equal(t, 2, belakang, "a zone left with no checkpoint gets its demo ones back")
	})

	t.Run("the zones hold them in the order the demo promises, two of them shared", func(t *testing.T) {
		zones := map[string][]string{}
		for _, name := range []string{"Pos Gerbang Utama", "Pos Belakang", "Pos Jaga Depan"} {
			var z int64
			f.query([]any{&z}, "SELECT id FROM patrol_zones WHERE name = $1", name)
			code, got, raw := f.zoneCall("GET", zonePath(z), f.adminTok, nil)
			require.Equal(t, 200, code, string(raw))
			zones[name] = layout(got)
		}
		require.Equal(t, []string{"1:Gerbang Utama", "2:Pagar Timur", "3:Pintu Belakang"}, zones["Pos Gerbang Utama"])
		require.Equal(t, []string{"1:Pintu Belakang", "2:Ruang Panel Listrik"}, zones["Pos Belakang"])
		require.Equal(t, []string{"1:Pos Jaga", "2:Gerbang Utama"}, zones["Pos Jaga Depan"])
	})

	t.Run("the checklists use every rule and the guard's read shows them", func(t *testing.T) {
		var rules string
		f.query([]any{&rules}, `SELECT string_agg(rule, ', ' ORDER BY rule) FROM (
		                          SELECT DISTINCT CASE WHEN min_photos > 0 THEN 'photo required' WHEN max_photos = 0 THEN 'no photo'
		                                               ELSE 'photo optional' END AS rule
		                          FROM patrol_checkpoint_actions) r`)
		require.Equal(t, "no photo, photo optional, photo required", rules)
		var noted, several int
		f.query([]any{&noted}, "SELECT count(*) FROM patrol_checkpoint_actions WHERE is_note_required")
		f.query([]any{&several}, "SELECT count(*) FROM patrol_checkpoint_actions WHERE max_photos > 1")
		require.Positive(t, noted)
		require.Positive(t, several)
	})

	t.Run("the summary prints each checkpoint's QR text, in the shape a scan will check", func(t *testing.T) {
		var rows, matching int
		f.query([]any{&rows}, "SELECT count(*) FROM ("+strings.TrimSuffix(summary, ";")+") s")
		require.Equal(t, 7, rows, "one line per zone and checkpoint")
		f.query([]any{&matching}, `SELECT count(*) FROM (`+strings.TrimSuffix(summary, ";")+`) s
		        WHERE s.qr_payload ~ ('^PERSIST-CP:1:' || $1::text || ':[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}:[A-Za-z0-9_-]{22}$')`,
			strconv.FormatInt(f.org, 10))
		require.Equal(t, 7, matching)
	})
}

// seedStep7 reads the SQL of step 7 of scripts/seed-dev-server.sh (patrol routes, scans and zone
// check-ins) out of the real script, as seedStep6 does for step 6. The demo project and regu become the
// test world's.
func seedStep7(t *testing.T, org int64) string {
	t.Helper()
	raw, err := os.ReadFile("../../scripts/seed-dev-server.sh")
	require.NoError(t, err)
	script := string(raw)
	start := strings.Index(script, `say "7/8 Patrol routes, scans (pending / approved / declined) and zone check-ins"`)
	require.NotEqual(t, -1, start, "step 7 of the seed script was not found")
	script = script[start:]
	script = script[strings.Index(script, "<<'SQL'\n")+len("<<'SQL'\n"):]
	script = script[:strings.Index(script, "\nSQL\n")]
	script = strings.NewReplacer("1K-DEMO-S1", "1K-IBMI-S1", "1K-DEMO-S", "1K-IBMI-S", ":'org'", "'"+strconv.FormatInt(org, 10)+"'").Replace(script)

	var write []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, `\`), trimmed == "BEGIN;", trimmed == "COMMIT;", strings.HasPrefix(trimmed, "SELECT set_config("):
			// psql commands, the transaction and the tenant setting (the test's transaction has them).
		default:
			write = append(write, line)
		}
	}
	return strings.Join(write, "\n")
}

// The scans of the demo data are of a checkpoint and answer its checklist, so the views of Phase 10.10 show
// them like a scan made through the API.
func TestSeedPatrolScans(t *testing.T) {
	f := newFlow(t)
	f.exec(func() string { w, _ := seedStep6(t, f.org); return w }())

	// The demo's people: a DANRU and two members of the regu, scheduled on each of the last four days.
	_, danruTok := f.addMember("1K-IBMI-S1", "DANRU", 900700, "KPIDANRU01", "Danru Seed")
	f.addMember("1K-IBMI-S1", "MEMBER", 900701, "KPIANG0001", "Anggota Seed 1")
	f.addMember("1K-IBMI-S1", "MEMBER", 900702, "KPIANG0002", "Anggota Seed 2")
	var shiftID int64
	f.query([]any{&shiftID}, "INSERT INTO shifts (name, start_minute, end_minute) VALUES ('Siang seed', 480, 960) RETURNING id")
	f.exec(`INSERT INTO schedules (personnel_id, work_date, shift_id)
	        SELECT p.id, d::date, $1 FROM personnels p
	        CROSS JOIN generate_series((now() AT TIME ZONE 'Asia/Jakarta')::date - 3, (now() AT TIME ZONE 'Asia/Jakarta')::date, interval '1 day') d
	        WHERE p.nik IN ('KPIANG0001', 'KPIANG0002')`, shiftID)

	writes := seedStep7(t, f.org)
	count := func(sql string) (n int) {
		f.query([]any{&n}, sql)
		return n
	}
	f.exec(writes)
	scans := count("SELECT count(*) FROM patrol_scans")
	actions := count("SELECT count(*) FROM patrol_scan_actions")
	require.Positive(t, scans)
	require.Positive(t, actions)

	t.Run("every scan is of the first checkpoint of its zone", func(t *testing.T) {
		require.Zero(t, count(`SELECT count(*) FROM patrol_scans s
		    WHERE s.checkpoint_id IS DISTINCT FROM (SELECT zc.checkpoint_id FROM patrol_zone_checkpoints zc
		                                            WHERE zc.zone_id = s.zone_id ORDER BY zc.sort_order LIMIT 1)`))
	})

	t.Run("each scan answers every active action of its checkpoint, once", func(t *testing.T) {
		require.Zero(t, count(`SELECT count(*) FROM patrol_scans s
		    WHERE (SELECT count(*) FROM patrol_scan_actions a WHERE a.scan_id = s.id)
		       <> (SELECT count(*) FROM patrol_checkpoint_actions c WHERE c.checkpoint_id = s.checkpoint_id AND c.active)`))
		require.Zero(t, count(`SELECT count(*) FROM patrol_scan_actions a JOIN patrol_checkpoint_actions c ON c.id = a.action_id
		    WHERE a.snapshot->>'action' <> c.action OR a.snapshot->'media_ids' <> '[]'::jsonb`))
		require.Zero(t, count(`SELECT count(*) FROM patrol_scan_actions a JOIN patrol_checkpoint_actions c ON c.id = a.action_id
		    WHERE c.is_note_required AND a.note IS NULL`), "an action that needs a note has one")
	})

	t.Run("the patrols count the scans made on their day", func(t *testing.T) {
		require.Zero(t, count(`SELECT count(*) FROM patrol_routes r
		    WHERE r.scan_count <> (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = r.id)`))
	})

	t.Run("running it again changes nothing", func(t *testing.T) {
		f.exec(writes)
		require.Equal(t, scans, count("SELECT count(*) FROM patrol_scans"))
		require.Equal(t, actions, count("SELECT count(*) FROM patrol_scan_actions"))
	})

	t.Run("the views show a seeded scan with its checkpoint and its actions", func(t *testing.T) {
		var id int64
		f.query([]any{&id}, `SELECT s.id FROM patrol_scans s
		    WHERE EXISTS (SELECT 1 FROM patrol_scan_actions a WHERE a.scan_id = s.id) ORDER BY s.id LIMIT 1`)
		code, got, raw := f.getScan(danruTok, id)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotEmpty(t, got.Checkpoint.Name)
		require.NotEmpty(t, got.Actions)
		for _, a := range got.Actions {
			require.NotEmpty(t, a.Action)
			require.NotNil(t, a.Photos)
		}
	})
}
