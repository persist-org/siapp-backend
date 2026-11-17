package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/patrol"
)

// The centre of the checkpoints these tests make.
const zoneLat, zoneLng = -6.2, 106.8

type scanOut struct {
	ID               int64      `json:"id"`
	LocalID          string     `json:"local_id"`
	ZoneID           int64      `json:"zone_id"`
	ZoneType         string     `json:"zone_type"`
	CheckpointID     *string    `json:"checkpoint_id"`
	PersonnelID      int64      `json:"personnel_id"`
	TeamCode         string     `json:"team_code"`
	ProjectCode      string     `json:"project_code"`
	WorkDate         string     `json:"work_date"`
	DistanceMeters   *int       `json:"distance_meters"`
	Subject          string     `json:"subject"`
	Status           string     `json:"status"`
	SubmittedOffline bool       `json:"submitted_offline"`
	Answers          []answerIn `json:"answers"`
}

// answerIn is an answer of the old zone checklist, as a scan made before the checkpoints shows it. A scan
// made now has none: its answers are rows of patrol_scan_actions (Phase 10.10 shows them).
type answerIn struct {
	ItemID   int64   `json:"item_id"`
	Label    string  `json:"label"`
	Value    any     `json:"value"`
	Note     *string `json:"note"`
	MediaIDs []int64 `json:"media_ids"`
}

// patrolCheckpoint is a checkpoint of the test world with the text its sticker holds (read from the
// database: the API never returns the secret).
type patrolCheckpoint struct {
	checkpointOut
	payload string
}

// patrolZone is a zone of the test world with the checkpoints a guard scans in it. checkpoint is the
// first, and payload the text a scan of it sends: a test may change payload to send a wrong one.
type patrolZone struct {
	zoneOut
	checkpoint patrolCheckpoint
	payload    string
	all        []patrolCheckpoint
}

func (f *flow) checkpointPayload(id string) string {
	f.t.Helper()
	var secret string
	f.query([]any{&secret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", id)
	return patrol.CheckpointPayload(f.org, id, secret)
}

// buildZone makes a zone through the admin API with n checkpoints in it, in order, each made with the
// checkpoint fields in cp (a point, a radius) and the checklist in actions.
func (f *flow) buildZone(body map[string]any, n int, cp map[string]any, actions []map[string]any) patrolZone {
	f.t.Helper()
	if body["project_code"] == nil {
		body["project_code"] = "1K-IBMI-S"
	}
	if body["type"] == nil {
		body["type"] = "PATROL"
	}
	if body["name"] == nil {
		body["name"] = "Pos " + uuid.NewString()[:8]
	}
	code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, body)
	require.Equal(f.t, http.StatusCreated, code, string(raw))

	out := patrolZone{zoneOut: z}
	ids := make([]string, 0, n)
	for range n {
		extra := map[string]any{}
		for k, v := range cp {
			extra[k] = v
		}
		if actions != nil {
			extra["actions"] = actions
		}
		c := f.newCheckpoint(z.ProjectCode, "Titik "+uuid.NewString()[:8], extra)
		out.all = append(out.all, patrolCheckpoint{checkpointOut: c, payload: f.checkpointPayload(c.ID)})
		ids = append(ids, c.ID)
	}
	if n > 0 {
		code, assigned, raw := f.assign(z.ID, ids...)
		require.Equal(f.t, http.StatusOK, code, string(raw))
		out.zoneOut = assigned
		out.checkpoint, out.payload = out.all[0], out.all[0].payload
	}
	return out
}

// newZone makes a zone with one checkpoint that has no point, so a scan from anywhere is accepted. The
// keys "actions" (the checkpoint's checklist) and "cp" (more checkpoint fields) of body are for the
// checkpoint; the rest is the zone's.
func (f *flow) newZone(body map[string]any) patrolZone {
	f.t.Helper()
	actions, _ := body["actions"].([]map[string]any)
	cp, _ := body["cp"].(map[string]any)
	delete(body, "actions")
	delete(body, "cp")
	return f.buildZone(body, 1, cp, actions)
}

// fencedZone is a zone with one checkpoint at the test centre with a 50 m radius and the given checklist.
func (f *flow) fencedZone(actions []map[string]any, extra map[string]any) patrolZone {
	f.t.Helper()
	body := map[string]any{}
	for k, v := range extra {
		body[k] = v
	}
	return f.buildZone(body, 1, map[string]any{"latitude": zoneLat, "longitude": zoneLng, "radius_meters": 50}, actions)
}

// fencedRound is a zone with n checkpoints, all at the test centre, in order, with no checklist.
func (f *flow) fencedRound(n int, extra map[string]any) patrolZone {
	f.t.Helper()
	body := map[string]any{}
	for k, v := range extra {
		body[k] = v
	}
	return f.buildZone(body, n, map[string]any{"latitude": zoneLat, "longitude": zoneLng, "radius_meters": 50}, nil)
}

// photo registers an uploaded patrol photo of the person and returns its id.
func (f *flow) photo(owner int64) int64 {
	f.t.Helper()
	var id int64
	f.query([]any{&id}, `INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
		VALUES ($1, 'PATROL_SCAN', 'image/jpeg', 1000, 'pending', 'UPLOADED', 'CAMERA') RETURNING id`, owner)
	// The key has the shape the storage driver expects (media.ValidKey), so the photo can be shown.
	f.exec(`UPDATE media SET storage_key = $1::text || '/PATROL_SCAN/' || to_char(now(), 'YYYY/MM') || '/' || id || '-' || md5(random()::text)
		WHERE id = $2`, strconv.FormatInt(f.org, 10), id)
	return id
}

// scansMade counts the bodies made for each checkpoint (by id: a rotated QR is the same checkpoint): the server refuses two scans of a checkpoint by one
// person less than 30 seconds apart, so each later body of a checkpoint is made 31 seconds earlier than the
// one before it, as a guard's scans would be. The first is made now.
var (
	scansMadeMu sync.Mutex
	scansMade   = map[string]int{}
)

// bodyAt is a valid scan of the checkpoint made at a moment, at its centre, answering every action of its
// checklist with the note it needs (and no photo: a test that has an action with a photo adds them).
func bodyAt(cp patrolCheckpoint, payload string, at time.Time) map[string]any {
	answers := make([]map[string]any, 0, len(cp.Actions))
	for _, a := range cp.Actions {
		ans := map[string]any{"action_id": a.ID}
		if a.IsNoteRequired {
			ans["note"] = "ok"
		}
		answers = append(answers, ans)
	}
	return map[string]any{
		"local_id": uuid.NewString(), "occurred_at": at.UTC().Format(time.RFC3339Nano), "checkpoint_id": cp.ID, "qr_payload": payload,
		"latitude": zoneLat, "longitude": zoneLng, "accuracy_meters": 5, "subject": "Patroli malam", "actions": answers,
	}
}

// scanBodyOf is a valid scan of a checkpoint of the zone, made now, or 31 seconds before the previous one
// of that checkpoint (see scansMade).
func scanBodyOf(cp patrolCheckpoint, payload string) map[string]any {
	scansMadeMu.Lock()
	n := scansMade[cp.ID]
	scansMade[cp.ID]++
	scansMadeMu.Unlock()
	return bodyAt(cp, payload, time.Now().Add(-time.Duration(n)*31*time.Second))
}

// scanBody is a valid scan of the zone's first checkpoint.
func scanBody(z patrolZone) map[string]any { return scanBodyOf(z.checkpoint, z.payload) }

// scan posts a scan with its local_id as the Idempotency-Key, as the app does.
func (f *flow) scan(token string, zoneID int64, body map[string]any) (int, scanOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/scan", token, jsonBody(f.t, body),
		map[string]string{"Idempotency-Key": body["local_id"].(string)})
	var env struct {
		Data scanOut `json:"data"`
	}
	if code < 300 {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) mustScan(token string, z patrolZone, body map[string]any) scanOut {
	f.t.Helper()
	code, s, raw := f.scan(token, z.ID, body)
	require.Equal(f.t, http.StatusCreated, code, string(raw))
	return s
}

func reasonIs(t *testing.T, code int, raw []byte, wantCode int, wantReason string) {
	t.Helper()
	require.Equal(t, wantCode, code, string(raw))
	reason, _ := problem(t, raw)
	require.Equal(t, wantReason, reason, string(raw))
}

func countOf(t *testing.T, f *flow, sql string, args ...any) int {
	t.Helper()
	var n int
	f.query([]any{&n}, sql, args...)
	return n
}

func (f *flow) countInbox(token, typ string) int {
	n := 0
	for _, it := range f.inbox(token) {
		if it.Type == typ {
			n++
		}
	}
	return n
}

// detailsOf reads details of an error answer.
func detailsOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	return env.Error.Details
}

func TestPatrolScanHappyPath(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone([]map[string]any{
		{"action": "Pagar terkunci?", "instruction": "Tarik gembok"},
		{"action": "Kondisi lampu", "is_note_required": true},
	}, nil)
	body := scanBody(z)
	body["notes"] = "  semua aman "
	var scan scanOut

	t.Run("an in-radius scan is stored with the shift rule's regu and date, and the DANRU is told", func(t *testing.T) {
		scan = f.mustScan(f.personTok, z, body)
		s := scan
		require.Equal(t, z.ID, s.ZoneID)
		require.Equal(t, "PATROL", s.ZoneType)
		require.Equal(t, z.checkpoint.ID, *s.CheckpointID)
		require.Equal(t, f.person, s.PersonnelID)
		require.Equal(t, "1K-IBMI-S1", s.TeamCode, "the regu comes from the membership, not from the app")
		require.Equal(t, "1K-IBMI-S", s.ProjectCode)
		require.Equal(t, "SUBMITTED", s.Status)
		require.False(t, s.SubmittedOffline)
		require.NotNil(t, s.DistanceMeters)
		require.Zero(t, *s.DistanceMeters)
		loc, _ := time.LoadLocation("Asia/Jakarta")
		require.Equal(t, time.Now().In(loc).Format(time.DateOnly), s.WorkDate, "no shift: the business date in the project's timezone")
		require.Empty(t, s.Answers, "the old answers column is an empty list: the answers are rows of patrol_scan_actions")

		require.Equal(t, 1, f.countInbox(danruTok, "PATROL_SUBMITTED"), "the DANRU is notified")
		require.Zero(t, f.countInbox(f.personTok, "OFFLINE_SUBMISSION_SENT"), "an online scan has no offline notice")
	})

	t.Run("the notice says which checkpoint it was", func(t *testing.T) {
		var id, name string
		f.query([]any{&id, &name}, "SELECT data->>'checkpoint_id', data->>'checkpoint_name' FROM notifications WHERE type = 'PATROL_SUBMITTED'")
		require.Equal(t, z.checkpoint.ID, id)
		require.Equal(t, z.checkpoint.Name, name)
	})

	t.Run("each action is a row with the note, the photos and a snapshot that says the same", func(t *testing.T) {
		require.Equal(t, 2, countOf(t, f, "SELECT count(*) FROM patrol_scan_actions WHERE scan_id = $1", scan.ID))
		require.Equal(t, 2, countOf(t, f, `SELECT count(*) FROM patrol_scan_actions
			WHERE scan_id = $1 AND checkpoint_id = $2 AND personnel_id = $3
			  AND coalesce(snapshot->>'note', '') = coalesce(note, '') AND (snapshot->'media_ids') = to_jsonb(media_ids)`,
			scan.ID, z.checkpoint.ID, f.person), "a snapshot's note and photos are those of its columns")
		var actionName, instruction, note string
		f.query([]any{&actionName, &instruction}, `SELECT snapshot->>'action', coalesce(snapshot->>'instruction', '')
			FROM patrol_scan_actions WHERE scan_id = $1 ORDER BY (snapshot->>'sort_order')::int LIMIT 1`, scan.ID)
		require.Equal(t, "Pagar terkunci?", actionName, "the snapshot holds the action as it was")
		require.Equal(t, "Tarik gembok", instruction)
		f.query([]any{&note}, `SELECT note FROM patrol_scan_actions WHERE scan_id = $1 AND snapshot->>'action' = 'Kondisi lampu'`, scan.ID)
		require.Equal(t, "ok", note)
	})

	t.Run("a guard zone notifies with the guard type", func(t *testing.T) {
		g := f.fencedZone(nil, map[string]any{"type": "GUARD"})
		f.mustScan(f.personTok, g, scanBody(g))
		require.Equal(t, 1, f.countInbox(danruTok, "GUARD_SUBMITTED"))
	})

	t.Run("a checkpoint without a point takes a scan from anywhere", func(t *testing.T) {
		open := f.newZone(map[string]any{})
		b := scanBody(open)
		b["latitude"], b["longitude"] = -7.5, 110.1
		s := f.mustScan(f.personTok, open, b)
		require.Nil(t, s.DistanceMeters)
	})

	t.Run("the zone's own point is only for the map: the checkpoint's is what counts", func(_ *testing.T) {
		far := f.buildZone(map[string]any{"latitude": 1.0, "longitude": 100.0, "radius_meters": 10}, 1,
			map[string]any{"latitude": zoneLat, "longitude": zoneLng, "radius_meters": 50}, nil)
		f.mustScan(f.personTok, far, scanBody(far))
	})

	t.Run("the subject is the checkpoint's name when none is sent, and the notes are kept trimmed", func(t *testing.T) {
		z2 := f.fencedZone(nil, nil)
		b := scanBody(z2)
		delete(b, "subject")
		s := f.mustScan(f.personTok, z2, b)
		require.Equal(t, z2.checkpoint.Name, s.Subject)
		b2 := scanBody(z2)
		b2["subject"] = "   "
		require.Equal(t, z2.checkpoint.Name, f.mustScan(f.personTok, z2, b2).Subject, "a blank subject is none")

		var notes string
		f.query([]any{&notes}, "SELECT notes FROM patrol_scans WHERE zone_id = $1 ORDER BY id LIMIT 1", z.ID)
		require.Equal(t, "semua aman", notes)
	})
}

func TestPatrolScanCheckpointAndQR(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)
	other := f.fencedZone(nil, nil)
	secret := z.payload[len(z.payload)-22:]

	t.Run("a bad QR, another checkpoint's, another organization's, an old secret and a zone sticker are all refused", func(t *testing.T) {
		for name, payload := range map[string]string{
			"garbage":              "hello",
			"another checkpoint's": other.payload,
			"another organization": patrol.CheckpointPayload(f.org+1, z.checkpoint.ID, secret),
			"a wrong secret":       patrol.CheckpointPayload(f.org, z.checkpoint.ID, "ZZZZZZZZZZZZZZZZZZZZZZ"),
			"the old zone sticker": "PERSIST-ZONE:1:" + strconv.FormatInt(f.org, 10) + ":" + strconv.FormatInt(z.ID, 10) + ":" + secret,
			"the id of another":    patrol.CheckpointPayload(f.org, other.checkpoint.ID, secret),
		} {
			b := scanBody(z)
			b["qr_payload"] = payload
			code, _, raw := f.scan(f.personTok, z.ID, b)
			reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_QR_INVALID")
			_ = name
		}
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
	})

	t.Run("the id on the sticker may be in capitals: it is the same checkpoint", func(_ *testing.T) {
		b := scanBody(z)
		b["qr_payload"] = "PERSIST-CP:1:" + strconv.FormatInt(f.org, 10) + ":" + strings.ToUpper(z.checkpoint.ID) + ":" + secret
		f.mustScan(f.personTok, z, b)
	})

	t.Run("a rotated QR stops working, the new one works", func(t *testing.T) {
		code, raw := f.do(http.MethodPost, checkpointPath(z.checkpoint.ID)+"/rotate-qr", f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw2 := f.scan(f.personTok, z.ID, scanBody(z))
		reasonIs(t, code, raw2, http.StatusUnprocessableEntity, "CHECKPOINT_QR_INVALID")
		z.payload = f.checkpointPayload(z.checkpoint.ID)
		z.checkpoint.payload = z.payload
		f.mustScan(f.personTok, z, scanBody(z))
	})

	t.Run("a checkpoint that is not this zone's is refused, whatever its own sticker says", func(t *testing.T) {
		stray := f.fencedZone(nil, nil) // its checkpoint is in no zone we scan
		b := scanBodyOf(stray.checkpoint, stray.payload)
		code, _, raw := f.scan(f.personTok, z.ID, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_NOT_IN_ZONE")

		b = scanBody(z)
		b["checkpoint_id"] = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
		code, _, raw = f.scan(f.personTok, z.ID, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_NOT_IN_ZONE")
	})

	t.Run("a checkpoint switched off or deleted is not scannable", func(t *testing.T) {
		off := f.fencedZone(nil, nil)
		code, _, raw := f.cpCall(http.MethodPatch, checkpointPath(off.checkpoint.ID), f.adminTok, map[string]any{"is_active": false})
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.scan(f.personTok, off.ID, scanBody(off))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_NOT_IN_ZONE")

		gone := f.fencedZone(nil, nil)
		codeD, rawD := f.do(http.MethodDelete, checkpointPath(gone.checkpoint.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, codeD, string(rawD))
		code, _, raw = f.scan(f.personTok, gone.ID, scanBody(gone))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_NOT_IN_ZONE")
	})

	t.Run("a checkpoint_id that is missing or is not a uuid is a validation error naming the field", func(t *testing.T) {
		for name, mutate := range map[string]func(map[string]any){
			"missing":    func(m map[string]any) { delete(m, "checkpoint_id") },
			"not a uuid": func(m map[string]any) { m["checkpoint_id"] = "abc" },
		} {
			b := scanBody(z)
			mutate(b)
			code, _, raw := f.scan(f.personTok, z.ID, b)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			_, fields := problem(t, raw)
			require.Contains(t, fields, "checkpoint_id", name)
		}
	})

	t.Run("a scan outside the radius is refused, inside it accepted, and the tolerance is capped", func(t *testing.T) {
		far := scanBody(z)
		far["latitude"] = zoneLat + 0.001 // about 111 m
		code, _, raw := f.scan(f.personTok, z.ID, far)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_OUTSIDE_RADIUS")
		require.NotNil(t, detailsOf(t, raw)["distance_meters"])
		require.EqualValues(t, 50, detailsOf(t, raw)["radius_meters"])

		hopeful := scanBody(z)
		hopeful["latitude"], hopeful["accuracy_meters"] = zoneLat+0.001, 5000 // a poor fix does not unlock the checkpoint
		code, _, raw = f.scan(f.personTok, z.ID, hopeful)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_OUTSIDE_RADIUS")

		edge := scanBody(z)
		edge["latitude"], edge["accuracy_meters"] = zoneLat+0.0006, 20 // about 67 m, inside 50 + 20
		s := f.mustScan(f.personTok, z, edge)
		require.NotNil(t, s.DistanceMeters)
		require.Greater(t, *s.DistanceMeters, 50, "outside the radius itself, let in by the accuracy")
	})

	t.Run("a refused scan stores nothing", func(t *testing.T) {
		before := countOf(t, f, "SELECT count(*) FROM patrol_scans")
		far := scanBody(z)
		far["latitude"] = zoneLat + 0.01
		f.scan(f.personTok, z.ID, far)
		require.Equal(t, before, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_scan_actions WHERE scan_id NOT IN (SELECT id FROM patrol_scans)"))
	})
}

func TestPatrolScanWhoMayScan(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)

	t.Run("a person with no regu is refused", func(t *testing.T) {
		stranger := f.addPerson(900401, "ORANGLAIN2", "Orang Lain")
		tok := f.token(stranger, f.addSession(stranger, "tok-stranger2"))
		code, _, raw := f.scan(tok, z.ID, scanBody(z))
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})

	t.Run("a member of another project is not in this one", func(t *testing.T) {
		f.exec(`INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ('1K-IBMI-C', '1K-IBMI', 'IBMI kebersihan', 'cleaning', 'Asia/Jakarta')`)
		f.exec(`INSERT INTO teams (code, project_code, description, active) VALUES ('1K-IBMI-C1', '1K-IBMI-C', 'IBMI C1', true)`)
		cleaner := f.addPerson(900402, "CLEANER001", "Petugas Kebersihan")
		f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-C1', $1, 'MEMBER', current_date - 30)", cleaner)
		tok := f.token(cleaner, f.addSession(cleaner, "tok-cleaner"))
		code, _, raw := f.scan(tok, z.ID, scanBody(z))
		reasonIs(t, code, raw, http.StatusForbidden, "ZONE_NOT_IN_PROJECT")
	})

	t.Run("a zone for another regu is not this member's", func(t *testing.T) {
		s2 := f.fencedZone(nil, map[string]any{"team_code": "1K-IBMI-S2"})
		code, _, raw := f.scan(f.personTok, s2.ID, scanBody(s2))
		reasonIs(t, code, raw, http.StatusForbidden, "ZONE_NOT_IN_PROJECT")
		s1 := f.fencedZone(nil, map[string]any{"team_code": "1K-IBMI-S1"})
		f.mustScan(f.personTok, s1, scanBody(s1))
	})

	t.Run("the permission of the zone's type is what counts", func(t *testing.T) {
		f.exec("UPDATE role_permissions SET scope = 'NONE' WHERE permission = 'patrol.scan'")
		code, _, raw := f.scan(f.personTok, z.ID, scanBody(z))
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		guard := f.fencedZone(nil, map[string]any{"type": "GUARD"})
		f.mustScan(f.personTok, guard, scanBody(guard))
	})

	t.Run("a switched-off or missing zone is 404", func(t *testing.T) {
		off := f.fencedZone(nil, map[string]any{"is_active": false})
		code, _, raw := f.scan(f.personTok, off.ID, scanBody(off))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
		missing := z
		missing.ID = 99999999
		code, _, raw = f.scan(f.personTok, missing.ID, scanBody(missing))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
	})
}

// oneAction is a zone with one checkpoint with one action, and what a scan of it answers.
func (f *flow) oneAction(action map[string]any) patrolZone {
	return f.fencedZone([]map[string]any{action}, nil)
}

func TestPatrolScanActionRules(t *testing.T) {
	f := newFlow(t)
	type tc struct {
		action  map[string]any
		answer  map[string]any // merged into the answer of the action; nil leaves the action unanswered
		photos  int
		wantErr string
	}
	for name, c := range map[string]tc{
		"an action with no answer":                {map[string]any{"action": "A"}, nil, 0, "CHECKLIST_ANSWER_REQUIRED"},
		"a required photo is missing":             {map[string]any{"action": "A", "min_photos": 1, "max_photos": 2}, map[string]any{}, 0, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"two required photos, one sent":           {map[string]any{"action": "A", "min_photos": 2, "max_photos": 3}, map[string]any{}, 1, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"a required note is missing":              {map[string]any{"action": "A", "is_note_required": true}, map[string]any{}, 0, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"a required note is blank":                {map[string]any{"action": "A", "is_note_required": true}, map[string]any{"note": "   "}, 0, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"one photo too many":                      {map[string]any{"action": "A", "max_photos": 1}, map[string]any{}, 2, "CHECKLIST_ANSWER_INVALID"},
		"a photo for an action with no slot":      {map[string]any{"action": "A", "max_photos": 0}, map[string]any{}, 1, "CHECKLIST_ANSWER_INVALID"},
		"an optional photo may be left out":       {map[string]any{"action": "A", "max_photos": 2}, map[string]any{}, 0, ""},
		"a required photo is there":               {map[string]any{"action": "A", "min_photos": 1, "max_photos": 2}, map[string]any{}, 1, ""},
		"the most photos":                         {map[string]any{"action": "A", "min_photos": 5, "max_photos": 5}, map[string]any{}, 5, ""},
		"a note and a photo, both asked and sent": {map[string]any{"action": "A", "min_photos": 1, "is_note_required": true}, map[string]any{"note": "aman"}, 1, ""},
	} {
		z := f.oneAction(c.action)
		b := scanBody(z)
		b["actions"] = []map[string]any{}
		if c.answer != nil {
			answer := map[string]any{"action_id": z.checkpoint.Actions[0].ID}
			for k, v := range c.answer {
				answer[k] = v
			}
			if c.photos > 0 {
				ids := make([]int64, 0, c.photos)
				for range c.photos {
					ids = append(ids, f.photo(f.person))
				}
				answer["media_ids"] = ids
			}
			b["actions"] = []map[string]any{answer}
		}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		if c.wantErr == "" {
			require.Equal(t, http.StatusCreated, code, name+": "+string(raw))
			continue
		}
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, c.wantErr)
		require.Equal(t, z.checkpoint.Actions[0].ID, detailsOf(t, raw)["action_id"], name+": the error names the action")
	}

	t.Run("a note over 1000 characters is a validation error", func(t *testing.T) {
		z := f.oneAction(map[string]any{"action": "A"})
		b := scanBody(z)
		b["actions"] = []map[string]any{{"action_id": z.checkpoint.Actions[0].ID, "note": strings.Repeat("a", 1001)}}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("an action of another checkpoint, an unknown one, a bad id and a repeated one are refused", func(t *testing.T) {
		z := f.oneAction(map[string]any{"action": "A"})
		other := f.oneAction(map[string]any{"action": "B"})
		mine := z.checkpoint.Actions[0].ID
		for name, answers := range map[string][]map[string]any{
			"another checkpoint's": {{"action_id": mine}, {"action_id": other.checkpoint.Actions[0].ID}},
			"unknown":              {{"action_id": mine}, {"action_id": "3f2504e0-4f89-41d3-9a0c-0305e82c3301"}},
			"not a uuid":           {{"action_id": mine}, {"action_id": "abc"}},
			"answered twice":       {{"action_id": mine}, {"action_id": mine}},
		} {
			b := scanBody(z)
			b["actions"] = answers
			code, _, raw := f.scan(f.personTok, z.ID, b)
			reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ITEM_INVALID")
			_ = name
		}
	})

	t.Run("the same photo in two actions is refused, different photos are fine", func(t *testing.T) {
		z := f.fencedZone([]map[string]any{{"action": "A", "min_photos": 1}, {"action": "B", "min_photos": 1}}, nil)
		a, b2 := z.checkpoint.Actions[0].ID, z.checkpoint.Actions[1].ID
		shared := f.photo(f.person)
		b := scanBody(z)
		b["actions"] = []map[string]any{{"action_id": a, "media_ids": []int64{shared}}, {"action_id": b2, "media_ids": []int64{shared}}}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ANSWER_INVALID")

		b = scanBody(z)
		b["actions"] = []map[string]any{{"action_id": a, "media_ids": []int64{f.photo(f.person)}}, {"action_id": b2, "media_ids": []int64{f.photo(f.person)}}}
		f.mustScan(f.personTok, z, b)
	})

	t.Run("a checkpoint with no checklist takes a scan with no actions", func(_ *testing.T) {
		z := f.fencedZone(nil, nil)
		b := scanBody(z)
		delete(b, "actions")
		f.mustScan(f.personTok, z, b)
	})
}

func TestPatrolScanPhotos(t *testing.T) {
	f := newFlow(t)
	z := f.oneAction(map[string]any{"action": "Foto gembok", "min_photos": 1, "max_photos": 2})
	action := z.checkpoint.Actions[0].ID
	photo := f.photo(f.person)

	b := scanBody(z)
	b["actions"] = []map[string]any{{"action_id": action, "media_ids": []int64{photo}}}
	s := f.mustScan(f.personTok, z, b)

	t.Run("the photos are attached to the scan, and listed on the action", func(t *testing.T) {
		var typ string
		var id int64
		f.query([]any{&typ, &id}, "SELECT attached_entity_type, attached_entity_id FROM media WHERE id = $1", photo)
		require.Equal(t, "PATROL_SCAN", typ)
		require.Equal(t, s.ID, id)
		var ids []int64
		f.query([]any{&ids}, "SELECT media_ids FROM patrol_scan_actions WHERE scan_id = $1", s.ID)
		require.Equal(t, []int64{photo}, ids)
	})

	t.Run("a photo that is already used, or someone else's, is refused and nothing is saved", func(t *testing.T) {
		before := countOf(t, f, "SELECT count(*) FROM patrol_scans")
		beforeActions := countOf(t, f, "SELECT count(*) FROM patrol_scan_actions")
		again := scanBody(z)
		again["actions"] = []map[string]any{{"action_id": action, "media_ids": []int64{photo}}}
		code, _, raw := f.scan(f.personTok, z.ID, again)
		reasonIs(t, code, raw, http.StatusConflict, "MEDIA_ALREADY_ATTACHED")

		colleague := f.addPerson(900410, "KOLEGA0002", "Kolega Dua")
		theirs := f.photo(colleague)
		stolen := scanBody(z)
		stolen["actions"] = []map[string]any{{"action_id": action, "media_ids": []int64{theirs}}}
		code, _, raw = f.scan(f.personTok, z.ID, stolen)
		reasonIs(t, code, raw, http.StatusNotFound, "MEDIA_NOT_FOUND")
		require.Equal(t, before, countOf(t, f, "SELECT count(*) FROM patrol_scans"), "the scan rolled back with its photos")
		require.Equal(t, beforeActions, countOf(t, f, "SELECT count(*) FROM patrol_scan_actions"), "and so did its answers")
	})

	t.Run("the same photo twice in one scan is refused", func(t *testing.T) {
		fresh := f.photo(f.person)
		b := scanBody(z)
		b["actions"] = []map[string]any{{"action_id": action, "media_ids": []int64{fresh, fresh}}}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ANSWER_INVALID")
	})
}

func TestPatrolScanInterval(t *testing.T) {
	f := newFlow(t)
	f.exec("UPDATE projects SET patrol_interval_minutes = 30 WHERE code = '1K-IBMI-S'")
	z := f.fencedRound(2, nil)
	one, two := z.all[0], z.all[1]
	other := f.fencedZone(nil, nil)
	ago := func(d time.Duration) time.Time { return time.Now().Add(-d) }

	f.mustScan(f.personTok, z, bodyAt(one, one.payload, ago(90*time.Minute)))

	t.Run("a second scan of the checkpoint inside the interval is refused, on either side of the first", func(t *testing.T) {
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(one, one.payload, ago(70*time.Minute)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
		require.EqualValues(t, 1800, detailsOf(t, raw)["interval_seconds"])

		code, _, raw = f.scan(f.personTok, z.ID, bodyAt(one, one.payload, ago(110*time.Minute)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
	})

	t.Run("the interval is per checkpoint: the next one of the zone is accepted a minute later", func(t *testing.T) {
		f.mustScan(f.personTok, z, bodyAt(two, two.payload, ago(89*time.Minute)))
	})

	t.Run("after the interval the first is accepted again, and so is another zone", func(t *testing.T) {
		f.mustScan(f.personTok, z, bodyAt(one, one.payload, ago(30*time.Minute)))
		f.mustScan(f.personTok, other, scanBody(other))
	})
}

// A project with no patrol_interval_minutes still refuses a rapid second scan of the same checkpoint, even
// with a new local_id, so a double tap cannot make two scans of one visit.
func TestPatrolScanMinimumGap(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(2, nil)
	one, two := z.all[0], z.all[1]
	other := f.fencedZone(nil, nil)

	f.mustScan(f.personTok, z, bodyAt(one, one.payload, time.Now().Add(-10*time.Minute)))

	t.Run("a second scan of it seconds later is refused", func(t *testing.T) {
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(one, one.payload, time.Now().Add(-10*time.Minute+5*time.Second)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
	})

	t.Run("the next checkpoint seconds later, a scan past the gap and a scan of another zone are accepted", func(t *testing.T) {
		f.mustScan(f.personTok, z, bodyAt(two, two.payload, time.Now().Add(-10*time.Minute+5*time.Second)))
		f.mustScan(f.personTok, z, bodyAt(one, one.payload, time.Now()))
		f.mustScan(f.personTok, other, scanBody(other))
	})
}

// accuracy_meters is numeric(8,2) in the database: a value it cannot hold is a validation error, not
// a 500.
func TestPatrolScanAccuracyIsBounded(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)
	b := scanBody(z)
	b["accuracy_meters"] = 2_000_000
	code, _, raw := f.scan(f.personTok, z.ID, b)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	require.Contains(t, string(raw), "accuracy_meters")
}

// The order rule (docs/plan/10.1-patrol-update.md, U3): the first checkpoint can always be scanned, any
// other only once the round has passed the one before it.
func TestPatrolScanOrder(t *testing.T) {
	f := newFlow(t)
	// Yesterday at noon in Jakarta: a series of scans a few minutes apart stays on one work date.
	jakartaTime, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	y := time.Now().In(jakartaTime).AddDate(0, 0, -1)
	base := time.Date(y.Year(), y.Month(), y.Day(), 12, 0, 0, 0, jakartaTime)
	// scans sends the checkpoints by position, from 1, five minutes apart, and says where it first stopped
	// (0 when it never did) and what the refusal wanted.
	scans := func(z patrolZone, positions ...int) (stoppedAt int, expected string) {
		for i, p := range positions {
			cp := z.all[p-1]
			code, _, raw := f.scan(f.personTok, z.ID, bodyAt(cp, cp.payload, base.Add(time.Duration(i)*5*time.Minute)))
			if code == http.StatusCreated {
				continue
			}
			reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
			want, _ := detailsOf(t, raw)["expected_checkpoint_id"].(string)
			return i + 1, want
		}
		return 0, ""
	}

	t.Run("the rounds of the plan", func(t *testing.T) {
		for name, tc := range map[string]struct {
			positions []int
			stoppedAt int
			expected  int // the position the refusal wanted, 0 when none
		}{
			"1,2,3,1,2,3 is two rounds":            {[]int{1, 2, 3, 1, 2, 3}, 0, 0},
			"1,3 skips the 2":                      {[]int{1, 3}, 2, 2},
			"1,2,2 needs a second 1":               {[]int{1, 2, 2}, 3, 1},
			"1,2,1,2,3 restarts a round":           {[]int{1, 2, 1, 2, 3}, 0, 0},
			"starting at the second":               {[]int{2}, 1, 1},
			"starting at the third":                {[]int{3}, 1, 2},
			"1,1,1: a new round may begin at once": {[]int{1, 1, 1}, 0, 0},
			"1,2,3,3 repeats the last":             {[]int{1, 2, 3, 3}, 4, 2},
		} {
			z := f.fencedRound(3, nil)
			stopped, want := scans(z, tc.positions...)
			require.Equal(t, tc.stoppedAt, stopped, name)
			if tc.expected > 0 {
				require.Equal(t, z.all[tc.expected-1].ID, want, name+": the answer names the checkpoint to scan")
			}
		}
	})

	t.Run("a refused scan stores nothing, and the checkpoint can be scanned once the one before is", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(z.all[1], z.all[1].payload, base))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_scans WHERE zone_id = $1", z.ID))
		f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, base.Add(time.Minute)))
		f.mustScan(f.personTok, z, bodyAt(z.all[1], z.all[1].payload, base.Add(2*time.Minute)))
	})

	t.Run("another guard's scans do not count", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		colleague := f.addPerson(900440, "KOLEGA0005", "Kolega Lima")
		f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", colleague)
		tok := f.token(colleague, f.addSession(colleague, "tok-colleague5"))
		f.mustScan(tok, z, bodyAt(z.all[0], z.all[0].payload, base))

		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(z.all[1], z.all[1].payload, base.Add(time.Minute)))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
	})

	t.Run("the same checkpoint scanned in another zone does not count", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		elsewhere := f.newPlainZone("1K-IBMI-S", "Zona Lain "+uuid.NewString()[:6], nil)
		code, _, raw := f.assign(elsewhere.ID, z.all[0].ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		other := patrolZone{zoneOut: elsewhere, checkpoint: z.all[0], payload: z.all[0].payload, all: z.all[:1]}
		f.mustScan(f.personTok, other, bodyAt(z.all[0], z.all[0].payload, base))

		code, _, raw = f.scan(f.personTok, z.ID, bodyAt(z.all[1], z.all[1].payload, base.Add(time.Minute)))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
	})

	t.Run("a declined scan still counts: a decline does not undo the walk", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		first := f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, base))
		f.exec(`UPDATE patrol_scans SET status = 'DECLINED', decided_by_id = $2, decided_at = now() WHERE id = $1`, first.ID, f.person)
		f.mustScan(f.personTok, z, bodyAt(z.all[1], z.all[1].payload, base.Add(10*time.Minute)))
	})

	t.Run("the next work date starts from the first checkpoint again", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		twoDaysAgo := time.Now().AddDate(0, 0, -2).Truncate(time.Hour)
		f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, twoDaysAgo))
		f.mustScan(f.personTok, z, bodyAt(z.all[1], z.all[1].payload, twoDaysAgo.Add(10*time.Minute)))

		yesterday := time.Now().AddDate(0, 0, -1).Truncate(time.Hour)
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(z.all[2], z.all[2].payload, yesterday))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
		f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, yesterday.Add(time.Minute)))
	})

	t.Run("a checkpoint switched off is stepped over", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		code, _, raw := f.cpCall(http.MethodPatch, checkpointPath(z.all[1].ID), f.adminTok, map[string]any{"is_active": false})
		require.Equal(t, http.StatusOK, code, string(raw))
		f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, base))
		f.mustScan(f.personTok, z, bodyAt(z.all[2], z.all[2].payload, base.Add(10*time.Minute)))
	})

	t.Run("an offline queue is judged as it was made: the later ones are refused until the earlier ones arrive, with the same key", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		offline := func(i int) map[string]any {
			b := bodyAt(z.all[i], z.all[i].payload, base.Add(time.Duration(i)*5*time.Minute))
			b["submitted_offline"] = true
			return b
		}
		third, second, first := offline(2), offline(1), offline(0)

		code, _, raw := f.scan(f.personTok, z.ID, third)
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
		require.Equal(t, z.all[1].ID, detailsOf(t, raw)["expected_checkpoint_id"])
		code, _, raw = f.scan(f.personTok, z.ID, second)
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
		require.Equal(t, z.all[0].ID, detailsOf(t, raw)["expected_checkpoint_id"])

		for _, b := range []map[string]any{first, second, third} {
			code, _, raw = f.scan(f.personTok, z.ID, b)
			require.Equal(t, http.StatusCreated, code, "sent again in order: "+string(raw))
		}
	})
}

func TestPatrolScanDeviceCondition(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone([]map[string]any{{"action": "Pagar"}, {"action": "Lampu"}}, nil)
	pagar, lampu := z.checkpoint.Actions[0].ID, z.checkpoint.Actions[1].ID
	conditions := func(scanID int64) map[string]string {
		out := map[string]string{}
		for _, id := range []string{pagar, lampu} {
			var dc *string
			f.query([]any{&dc}, "SELECT device_condition::text FROM patrol_scan_actions WHERE scan_id = $1 AND action_id = $2", scanID, id)
			if dc != nil {
				out[id] = *dc
			}
		}
		return out
	}

	t.Run("the scan's condition lands on every action, and an action's own wins", func(t *testing.T) {
		b := scanBody(z)
		b["device_condition"] = map[string]any{"battery_level": 80, "network_type": "wifi", "thermal_state": "nominal"}
		b["actions"] = []map[string]any{
			{"action_id": pagar},
			{"action_id": lampu, "device_condition": map[string]any{"battery_level": 10, "is_charging": true}},
		}
		s := f.mustScan(f.personTok, z, b)
		got := conditions(s.ID)
		require.JSONEq(t, `{"battery_level":80,"network_type":"wifi","thermal_state":"nominal"}`, got[pagar], "an unknown key is stored as sent")
		require.JSONEq(t, `{"battery_level":10,"is_charging":true}`, got[lampu])
	})

	t.Run("none sent stores NULL", func(t *testing.T) {
		s := f.mustScan(f.personTok, z, scanBody(z))
		require.Empty(t, conditions(s.ID))
		require.Equal(t, 2, countOf(t, f, "SELECT count(*) FROM patrol_scan_actions WHERE scan_id = $1 AND device_condition IS NULL", s.ID))
	})

	t.Run("mock_location is stored and the scan is still accepted: it is evidence, not a gate", func(t *testing.T) {
		b := scanBody(z)
		b["device_condition"] = map[string]any{"mock_location": true, "gps_enabled": false}
		s := f.mustScan(f.personTok, z, b)
		require.JSONEq(t, `{"mock_location":true,"gps_enabled":false}`, conditions(s.ID)[pagar])
	})

	t.Run("a bad one is refused with the field named, and nothing is stored", func(t *testing.T) {
		before := countOf(t, f, "SELECT count(*) FROM patrol_scans")
		big := map[string]any{"note": strings.Repeat("a", 5000)}
		for name, tc := range map[string]struct {
			set   func(map[string]any)
			field string
		}{
			"an array at the top":         {func(b map[string]any) { b["device_condition"] = []any{} }, "device_condition"},
			"a string at the top":         {func(b map[string]any) { b["device_condition"] = "baik" }, "device_condition"},
			"a number at the top":         {func(b map[string]any) { b["device_condition"] = 12 }, "device_condition"},
			"5 KB at the top":             {func(b map[string]any) { b["device_condition"] = big }, "device_condition"},
			"a battery of the wrong type": {func(b map[string]any) { b["device_condition"] = map[string]any{"battery_level": "full"} }, "device_condition"},
			"a battery out of range":      {func(b map[string]any) { b["device_condition"] = map[string]any{"battery_level": 120} }, "device_condition"},
			"a network that is not known": {func(b map[string]any) { b["device_condition"] = map[string]any{"network_type": "5g"} }, "device_condition"},
			"a bad one inside an action": {func(b map[string]any) {
				b["actions"] = []map[string]any{{"action_id": pagar}, {"action_id": lampu, "device_condition": "x"}}
			}, "actions[1].device_condition"},
		} {
			b := scanBody(z)
			tc.set(b)
			code, _, raw := f.scan(f.personTok, z.ID, b)
			reasonIs(t, code, raw, http.StatusUnprocessableEntity, "DEVICE_CONDITION_INVALID")
			_, fields := problem(t, raw)
			require.Equal(t, []string{tc.field}, fields, name)
		}
		require.Equal(t, before, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
	})
}
