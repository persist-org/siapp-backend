package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/patrol"
)

// The centre of the zones these tests make.
const zoneLat, zoneLng = -6.2, 106.8

type scanOut struct {
	ID               int64      `json:"id"`
	LocalID          string     `json:"local_id"`
	ZoneID           int64      `json:"zone_id"`
	ZoneType         string     `json:"zone_type"`
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

type answerIn struct {
	ItemID   int64   `json:"item_id"`
	Label    string  `json:"label"`
	Value    any     `json:"value"`
	Note     *string `json:"note"`
	MediaIDs []int64 `json:"media_ids"`
}

// patrolZone is a zone of the test world and what a scan of it needs.
type patrolZone struct {
	zoneOut
	payload string
}

// newZone creates a zone through the admin API and reads its QR payload the way a printed sticker
// would carry it (from the database: the API never returns the secret).
func (f *flow) newZone(body map[string]any) patrolZone {
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
	return patrolZone{zoneOut: z, payload: f.payload(z.ID)}
}

func (f *flow) payload(zoneID int64) string {
	f.t.Helper()
	var secret string
	f.query([]any{&secret}, "SELECT qr_secret FROM patrol_zones WHERE id = $1", zoneID)
	return patrol.Payload(f.org, zoneID, secret)
}

// fencedZone is a zone at the test centre with a 50 m radius and the given checklist.
func (f *flow) fencedZone(checklist []map[string]any, extra map[string]any) patrolZone {
	body := map[string]any{"latitude": zoneLat, "longitude": zoneLng, "radius_meters": 50, "photo_required": false}
	if checklist != nil {
		body["checklist"] = checklist
	}
	for k, v := range extra {
		body[k] = v
	}
	return f.newZone(body)
}

// photo registers an uploaded patrol photo of the person and returns its id.
func (f *flow) photo(owner int64) int64 {
	f.t.Helper()
	var id int64
	f.query([]any{&id}, `INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
		VALUES ($1, 'PATROL_SCAN', 'image/jpeg', 1000, 'test/' || gen_random_uuid(), 'UPLOADED', 'CAMERA') RETURNING id`, owner)
	return id
}

// scanBody is a valid scan of the zone made now, at its centre.
func scanBody(z patrolZone) map[string]any {
	return map[string]any{
		"local_id": uuid.NewString(), "occurred_at": time.Now().UTC().Format(time.RFC3339Nano), "qr_payload": z.payload,
		"latitude": zoneLat, "longitude": zoneLng, "accuracy_meters": 5, "subject": "Patroli malam",
	}
}

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

func TestPatrolScanHappyPath(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone([]map[string]any{
		{"label": "Pagar terkunci?", "answer_type": "BOOLEAN"},
		{"label": "Kondisi lampu", "answer_type": "CHOICE", "required": false,
			"options": []map[string]string{{"value": "ok", "label": "Menyala"}, {"value": "mati", "label": "Mati"}}},
	}, nil)
	body := scanBody(z)
	body["answers"] = []map[string]any{{"item_id": z.Checklist[0].ID, "value": true}}
	body["notes"] = "  semua aman "

	t.Run("an in-radius scan is stored with the shift rule's regu and date, and the DANRU is told", func(t *testing.T) {
		s := f.mustScan(f.personTok, z, body)
		require.Equal(t, z.ID, s.ZoneID)
		require.Equal(t, "PATROL", s.ZoneType)
		require.Equal(t, f.person, s.PersonnelID)
		require.Equal(t, "1K-IBMI-S1", s.TeamCode, "the regu comes from the membership, not from the app")
		require.Equal(t, "1K-IBMI-S", s.ProjectCode)
		require.Equal(t, "SUBMITTED", s.Status)
		require.False(t, s.SubmittedOffline)
		require.NotNil(t, s.DistanceMeters)
		require.Zero(t, *s.DistanceMeters)
		loc, _ := time.LoadLocation("Asia/Jakarta")
		require.Equal(t, time.Now().In(loc).Format(time.DateOnly), s.WorkDate, "no shift: the business date in the project's timezone")

		require.Len(t, s.Answers, 2, "one entry per question, the unanswered optional one too")
		require.Equal(t, "Pagar terkunci?", s.Answers[0].Label)
		require.Equal(t, true, s.Answers[0].Value)
		require.Nil(t, s.Answers[1].Value)

		require.Equal(t, 1, f.countInbox(danruTok, "PATROL_SUBMITTED"), "the DANRU is notified")
		require.Zero(t, f.countInbox(f.personTok, "OFFLINE_SUBMISSION_SENT"), "an online scan has no offline notice")
	})

	t.Run("a guard zone notifies with the guard type", func(t *testing.T) {
		g := f.fencedZone(nil, map[string]any{"type": "GUARD"})
		f.mustScan(f.personTok, g, scanBody(g))
		require.Equal(t, 1, f.countInbox(danruTok, "GUARD_SUBMITTED"))
	})

	t.Run("a zone without a geofence takes a scan from anywhere", func(t *testing.T) {
		open := f.newZone(map[string]any{"photo_required": false})
		b := scanBody(open)
		b["latitude"], b["longitude"] = -7.5, 110.1
		s := f.mustScan(f.personTok, open, b)
		require.Nil(t, s.DistanceMeters)
	})

	t.Run("the notes are kept trimmed", func(t *testing.T) {
		var notes string
		f.query([]any{&notes}, "SELECT notes FROM patrol_scans WHERE zone_id = $1 ORDER BY id LIMIT 1", z.ID)
		require.Equal(t, "semua aman", notes)
	})
}

func TestPatrolScanQRAndGeofence(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)
	other := f.fencedZone(nil, nil)

	t.Run("a bad QR, another zone's, another organization's and an old secret are all refused", func(t *testing.T) {
		secret := z.payload[len(z.payload)-22:]
		for _, payload := range map[string]string{
			"garbage":              "hello",
			"another zone's code":  other.payload,
			"another organization": patrol.Payload(f.org+1, z.ID, secret),
			"a wrong secret":       patrol.Payload(f.org, z.ID, "ZZZZZZZZZZZZZZZZZZZZZZ"),
		} {
			b := scanBody(z)
			b["qr_payload"] = payload
			code, _, raw := f.scan(f.personTok, z.ID, b)
			reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_QR_INVALID")
		}
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
	})

	t.Run("a rotated QR stops working, the new one works", func(t *testing.T) {
		code, _, raw := f.zoneCall(http.MethodPost, zonePath(z.ID)+"/rotate-qr", f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.scan(f.personTok, z.ID, scanBody(z))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_QR_INVALID")
		z.payload = f.payload(z.ID)
		f.mustScan(f.personTok, z, scanBody(z))
	})

	t.Run("a scan outside the radius is refused, inside it accepted, and the tolerance is capped", func(t *testing.T) {
		far := scanBody(z)
		far["latitude"] = zoneLat + 0.001 // about 111 m
		code, _, raw := f.scan(f.personTok, z.ID, far)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_OUTSIDE_RADIUS")

		hopeful := scanBody(z)
		hopeful["latitude"], hopeful["accuracy_meters"] = zoneLat+0.001, 5000 // a poor fix does not unlock the zone
		code, _, raw = f.scan(f.personTok, z.ID, hopeful)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_OUTSIDE_RADIUS")

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
		missing := patrolZone{zoneOut: zoneOut{ID: 99999999}, payload: z.payload}
		code, _, raw = f.scan(f.personTok, missing.ID, scanBody(missing))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
	})
}

func TestPatrolScanChecklistRules(t *testing.T) {
	f := newFlow(t)
	photoItem := func(extra map[string]any) map[string]any {
		item := map[string]any{"label": "Item", "answer_type": "BOOLEAN"}
		for k, v := range extra {
			item[k] = v
		}
		return item
	}
	type tc struct {
		item    map[string]any
		answer  map[string]any // merged into the answer of item 0; nil leaves the question unanswered
		photos  int
		wantErr string
	}
	for name, c := range map[string]tc{
		"a required answer is missing":    {photoItem(nil), nil, 0, "CHECKLIST_ANSWER_REQUIRED"},
		"a wrong value type":              {photoItem(nil), map[string]any{"value": "ya"}, 0, "CHECKLIST_ANSWER_INVALID"},
		"a choice outside its options":    {photoItem(map[string]any{"answer_type": "CHOICE", "options": []map[string]string{{"value": "a", "label": "A"}, {"value": "b", "label": "B"}}}), map[string]any{"value": "c"}, 0, "CHECKLIST_ANSWER_INVALID"},
		"a missing REQUIRED photo":        {photoItem(map[string]any{"photo_mode": "REQUIRED"}), map[string]any{"value": true}, 0, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"a missing REQUIRED note":         {photoItem(map[string]any{"note_mode": "REQUIRED"}), map[string]any{"value": true}, 0, "CHECKLIST_ATTACHMENT_REQUIRED"},
		"too many photos":                 {photoItem(map[string]any{"photo_mode": "OPTIONAL", "photo_max": 1}), map[string]any{"value": true}, 2, "CHECKLIST_ANSWER_INVALID"},
		"a photo for a NONE item":         {photoItem(nil), map[string]any{"value": true}, 1, "CHECKLIST_ANSWER_INVALID"},
		"a note for a NONE item":          {photoItem(nil), map[string]any{"value": true, "note": "x"}, 0, "CHECKLIST_ANSWER_INVALID"},
		"an optional item may be skipped": {photoItem(map[string]any{"required": false}), nil, 0, ""},
		"an optional photo may be left":   {photoItem(map[string]any{"photo_mode": "OPTIONAL"}), map[string]any{"value": false}, 0, ""},
	} {
		z := f.fencedZone([]map[string]any{c.item}, nil)
		b := scanBody(z)
		if c.answer != nil {
			answer := map[string]any{"item_id": z.Checklist[0].ID}
			for k, v := range c.answer {
				answer[k] = v
			}
			if c.photos > 0 {
				var ids []int64
				for range c.photos {
					ids = append(ids, f.photo(f.person))
				}
				answer["media_ids"] = ids
			}
			b["answers"] = []map[string]any{answer}
		}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		if c.wantErr == "" {
			require.Equal(t, http.StatusCreated, code, name+": "+string(raw))
			continue
		}
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, c.wantErr)
		var details struct {
			Error struct {
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(raw, &details))
		require.EqualValues(t, z.Checklist[0].ID, details.Error.Details["item_id"], name+": the error names the item")
	}

	t.Run("an item of another zone is refused", func(t *testing.T) {
		z := f.fencedZone([]map[string]any{{"label": "Q", "answer_type": "BOOLEAN"}}, nil)
		other := f.fencedZone([]map[string]any{{"label": "Q", "answer_type": "BOOLEAN"}}, nil)
		b := scanBody(z)
		b["answers"] = []map[string]any{
			{"item_id": z.Checklist[0].ID, "value": true}, {"item_id": other.Checklist[0].ID, "value": true},
		}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ITEM_INVALID")
	})

	t.Run("ON_NO: yes needs no photo, no needs one", func(t *testing.T) {
		z := f.fencedZone([]map[string]any{{"label": "Aman?", "answer_type": "BOOLEAN", "photo_mode": "REQUIRED", "attach_when": "ON_NO"}}, nil)
		item := z.Checklist[0].ID
		yes := scanBody(z)
		yes["answers"] = []map[string]any{{"item_id": item, "value": true}}
		f.mustScan(f.personTok, z, yes)

		no := scanBody(z)
		no["answers"] = []map[string]any{{"item_id": item, "value": false}}
		code, _, raw := f.scan(f.personTok, z.ID, no)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ATTACHMENT_REQUIRED")

		no = scanBody(z)
		no["answers"] = []map[string]any{{"item_id": item, "value": false, "media_ids": []int64{f.photo(f.person)}}}
		f.mustScan(f.personTok, z, no)
	})

	t.Run("photo_required: false takes a scan without photos, true refuses one", func(t *testing.T) {
		free := f.fencedZone(nil, map[string]any{"photo_required": false})
		f.mustScan(f.personTok, free, scanBody(free))

		strict := f.fencedZone(nil, map[string]any{"photo_required": true})
		code, _, raw := f.scan(f.personTok, strict.ID, scanBody(strict))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "SCAN_PHOTO_REQUIRED")

		withPhoto := scanBody(strict)
		withPhoto["media_ids"] = []int64{f.photo(f.person)}
		f.mustScan(f.personTok, strict, withPhoto)
	})

	t.Run("a photo inside an answer counts for photo_required", func(_ *testing.T) {
		z := f.fencedZone([]map[string]any{{"label": "Foto lampu", "answer_type": "BOOLEAN", "photo_mode": "OPTIONAL"}}, map[string]any{"photo_required": true})
		b := scanBody(z)
		b["answers"] = []map[string]any{{"item_id": z.Checklist[0].ID, "value": true, "media_ids": []int64{f.photo(f.person)}}}
		f.mustScan(f.personTok, z, b)
	})
}

func TestPatrolScanPhotos(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, map[string]any{"photo_required": true})
	photo := f.photo(f.person)

	b := scanBody(z)
	b["media_ids"] = []int64{photo}
	s := f.mustScan(f.personTok, z, b)

	t.Run("the photos are attached to the scan", func(t *testing.T) {
		var typ string
		var id int64
		f.query([]any{&typ, &id}, "SELECT attached_entity_type, attached_entity_id FROM media WHERE id = $1", photo)
		require.Equal(t, "PATROL_SCAN", typ)
		require.Equal(t, s.ID, id)
	})

	t.Run("a photo that is already used, or someone else's, is refused and nothing is saved", func(t *testing.T) {
		before := countOf(t, f, "SELECT count(*) FROM patrol_scans")
		again := scanBody(z)
		again["media_ids"] = []int64{photo}
		code, _, raw := f.scan(f.personTok, z.ID, again)
		reasonIs(t, code, raw, http.StatusConflict, "MEDIA_ALREADY_ATTACHED")

		colleague := f.addPerson(900410, "KOLEGA0002", "Kolega Dua")
		theirs := f.photo(colleague)
		stolen := scanBody(z)
		stolen["media_ids"] = []int64{theirs}
		code, _, raw = f.scan(f.personTok, z.ID, stolen)
		reasonIs(t, code, raw, http.StatusNotFound, "MEDIA_NOT_FOUND")
		require.Equal(t, before, countOf(t, f, "SELECT count(*) FROM patrol_scans"), "the scan rolled back with its photos")
	})

	t.Run("the same photo twice in one scan is refused", func(t *testing.T) {
		fresh := f.photo(f.person)
		b := scanBody(z)
		b["media_ids"] = []int64{fresh, fresh}
		code, _, raw := f.scan(f.personTok, z.ID, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})
}

func TestPatrolScanInterval(t *testing.T) {
	f := newFlow(t)
	f.exec("UPDATE projects SET patrol_interval_minutes = 30 WHERE code = '1K-IBMI-S'")
	z := f.fencedZone(nil, nil)
	other := f.fencedZone(nil, nil)

	first := scanBody(z)
	first["occurred_at"] = time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339Nano)
	f.mustScan(f.personTok, z, first)

	t.Run("a second scan inside the interval is refused, on either side of the first", func(t *testing.T) {
		soon := scanBody(z)
		soon["occurred_at"] = time.Now().Add(-70 * time.Minute).UTC().Format(time.RFC3339Nano)
		code, _, raw := f.scan(f.personTok, z.ID, soon)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")

		earlier := scanBody(z)
		earlier["occurred_at"] = time.Now().Add(-110 * time.Minute).UTC().Format(time.RFC3339Nano)
		code, _, raw = f.scan(f.personTok, z.ID, earlier)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
	})

	t.Run("after the interval it is accepted, and so is another zone", func(_ *testing.T) {
		f.mustScan(f.personTok, z, scanBody(z))
		f.mustScan(f.personTok, other, scanBody(other))
	})
}
