package main

import (
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
		return []int{count("patrol_zones"), count("patrol_checklist_items"), count("patrol_checkpoints"),
			count("patrol_zone_checkpoints"), count("patrol_checkpoint_actions")}
	}

	f.exec(writes)
	first := snapshot()
	require.Equal(t, []int{3, 5, 5, 7, 7}, first, "three zones, their old questions, five checkpoints, seven links, seven actions")

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
