package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPatrolOfflineScan(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone([]map[string]any{{"label": "Aman?", "answer_type": "BOOLEAN"}}, nil)

	b := scanBody(z)
	b["occurred_at"] = time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
	b["submitted_offline"] = true
	b["answers"] = []map[string]any{{"item_id": z.Checklist[0].ID, "value": true}}
	first := f.mustScan(f.personTok, z, b)
	require.True(t, first.SubmittedOffline)

	t.Run("the author is told once that the offline scan arrived, and so is the DANRU", func(t *testing.T) {
		require.Equal(t, 1, f.countInbox(f.personTok, "OFFLINE_SUBMISSION_SENT"))
		require.Equal(t, 1, f.countInbox(danruTok, "PATROL_SUBMITTED"))
	})

	t.Run("the same request again is the stored answer and tells nobody", func(t *testing.T) {
		code, again, raw := f.scan(f.personTok, z.ID, b)
		require.Less(t, code, 300, string(raw))
		require.Equal(t, first.ID, again.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
		require.Equal(t, 1, f.countInbox(f.personTok, "OFFLINE_SUBMISSION_SENT"))
		require.Equal(t, 1, f.countInbox(danruTok, "PATROL_SUBMITTED"))
	})

	t.Run("when the Idempotency-Key has been forgotten, local_id still stops a second scan: 200 and no notice", func(t *testing.T) {
		f.exec("DELETE FROM idempotency_keys")
		code, again, raw := f.scan(f.personTok, z.ID, b)
		require.Equal(t, http.StatusOK, code, "a replay is 200, not 201: "+string(raw))
		require.Equal(t, first.ID, again.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_scans"))
		require.Equal(t, 1, f.countInbox(f.personTok, "OFFLINE_SUBMISSION_SENT"))
		require.Equal(t, 1, f.countInbox(danruTok, "PATROL_SUBMITTED"))
	})

	t.Run("a local_id that belongs to someone else's scan is not a replay", func(t *testing.T) {
		colleague := f.addPerson(900420, "KOLEGA0003", "Kolega Tiga")
		f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", colleague)
		tok := f.token(colleague, f.addSession(colleague, "tok-colleague3"))
		code, _, raw := f.scan(tok, z.ID, b)
		reasonIs(t, code, raw, http.StatusConflict, "SCAN_LOCAL_ID_TAKEN")
	})

	t.Run("occurred_at and local_id are checked", func(t *testing.T) {
		future := scanBody(z)
		future["occurred_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		code, _, raw := f.scan(f.personTok, z.ID, future)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "OCCURRED_AT_IN_FUTURE")

		old := scanBody(z)
		old["occurred_at"] = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
		code, _, raw = f.scan(f.personTok, z.ID, old)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "OCCURRED_AT_TOO_OLD")

		mismatch := scanBody(z)
		code, raw = f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(z.ID, 10)+"/scan", f.personTok, jsonBody(t, mismatch),
			map[string]string{"Idempotency-Key": uuid.NewString()})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))

		code, raw = f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(z.ID, 10)+"/scan", f.personTok, jsonBody(t, scanBody(z)), nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, "no Idempotency-Key: "+string(raw))
	})

	t.Run("a bad body is a validation error naming the field", func(t *testing.T) {
		for field, mutate := range map[string]func(map[string]any){
			"latitude":        func(m map[string]any) { m["latitude"] = 95 },
			"accuracy_meters": func(m map[string]any) { m["accuracy_meters"] = -1 },
			"subject":         func(m map[string]any) { m["subject"] = "  " },
			"qr_payload":      func(m map[string]any) { delete(m, "qr_payload") },
			"local_id":        func(m map[string]any) { m["local_id"] = "not-a-uuid" },
		} {
			m := scanBody(z)
			mutate(m)
			code, raw := f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(z.ID, 10)+"/scan", f.personTok, jsonBody(t, m),
				map[string]string{"Idempotency-Key": uuid.NewString()})
			require.Equal(t, http.StatusUnprocessableEntity, code, field+": "+string(raw))
			_, fields := problem(t, raw)
			require.Contains(t, fields, field)
		}
	})
}

func TestPatrolScanHistoryIsNotRewrittenByChecklistEdits(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone([]map[string]any{
		{"label": "Pagar terkunci?", "answer_type": "BOOLEAN"},
		{"label": "Lampu menyala?", "answer_type": "BOOLEAN"},
	}, nil)
	pagar, lampu := z.Checklist[0].ID, z.Checklist[1].ID

	b := scanBody(z)
	// Ten minutes ago: the scans below that are made "now" must be after the edit and still more
	// than the minimum gap away from this one.
	b["occurred_at"] = time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
	b["answers"] = []map[string]any{{"item_id": pagar, "value": true}, {"item_id": lampu, "value": false}}
	scan := f.mustScan(f.personTok, z, b)

	// The admin rewrites the form: a new wording, one question gone, a new required one.
	put := func(items []map[string]any) {
		code, _, raw := f.zoneCall(http.MethodPut, zonePath(z.ID)+"/checklist", f.adminTok, map[string]any{"checklist": items})
		require.Equal(t, http.StatusOK, code, string(raw))
	}
	put([]map[string]any{
		{"id": pagar, "label": "Pagar utama terkunci dan utuh?", "answer_type": "BOOLEAN"},
		{"label": "Alarm aktif?", "answer_type": "BOOLEAN"},
	})

	t.Run("the stored scan still holds the questions as they were", func(t *testing.T) {
		var answers string
		f.query([]any{&answers}, "SELECT answers::text FROM patrol_scans WHERE id = $1", scan.ID)
		var got []answerIn
		require.NoError(t, json.Unmarshal([]byte(answers), &got))
		require.Len(t, got, 2)
		require.Equal(t, "Pagar terkunci?", got[0].Label)
		require.Equal(t, "Lampu menyala?", got[1].Label, "a question that was removed since is still there")
		require.Equal(t, false, got[1].Value)
	})

	t.Run("an offline scan made before the edit is still accepted after it", func(t *testing.T) {
		// Made 30 minutes ago, before the edit: it answers the old questions. The removed one is
		// ignored, the new required one was not there to answer.
		old := scanBody(z)
		old["occurred_at"] = time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339Nano)
		old["submitted_offline"] = true
		old["answers"] = []map[string]any{{"item_id": pagar, "value": true}, {"item_id": lampu, "value": true}}
		s := f.mustScan(f.personTok, z, old)
		require.Len(t, s.Answers, 1, "only the question that is still on the form is kept")
		require.Equal(t, pagar, s.Answers[0].ItemID)
	})

	t.Run("a scan made after the edit has to answer the new question", func(t *testing.T) {
		live := scanBody(z)
		live["occurred_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		live["answers"] = []map[string]any{{"item_id": pagar, "value": true}}
		code, _, raw := f.scan(f.personTok, z.ID, live)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ANSWER_REQUIRED")
	})

	t.Run("answering a question that was removed before the scan is refused", func(t *testing.T) {
		now := scanBody(z)
		now["occurred_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		now["answers"] = []map[string]any{{"item_id": lampu, "value": true}}
		code, _, raw := f.scan(f.personTok, z.ID, now)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKLIST_ITEM_INVALID")
	})
}

func TestPatrolNightShiftWorkDate(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)
	loc, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)

	// A night shift of three days ago: 22:00 to 06:00 the next morning.
	day := time.Now().In(loc).AddDate(0, 0, -3)
	start := time.Date(day.Year(), day.Month(), day.Day(), 22, 0, 0, 0, loc)
	f.scheduleShift(f.person, start, start.Add(8*time.Hour))

	t.Run("a scan after midnight counts for the day the shift started", func(t *testing.T) {
		b := scanBody(z)
		b["occurred_at"] = start.Add(3 * time.Hour).UTC().Format(time.RFC3339) // 01:00 the next day
		b["submitted_offline"] = true
		s := f.mustScan(f.personTok, z, b)
		require.Equal(t, start.Format(time.DateOnly), s.WorkDate)
		require.Equal(t, "1K-IBMI-S1", s.TeamCode)
	})

	t.Run("a scan outside any shift uses the calendar day", func(t *testing.T) {
		b := scanBody(z)
		noon := time.Date(day.Year(), day.Month(), day.Day(), 12, 0, 0, 0, loc)
		b["occurred_at"] = noon.UTC().Format(time.RFC3339)
		s := f.mustScan(f.personTok, z, b)
		require.Equal(t, noon.Format(time.DateOnly), s.WorkDate)
	})
}

func zoneURL(zoneID int64, suffix string) string {
	return "/api/v1/zones/" + strconv.FormatInt(zoneID, 10) + suffix
}

type checkInOut struct {
	ID          int64   `json:"id"`
	PersonnelID int64   `json:"personnel_id"`
	FullName    *string `json:"full_name"`
	TeamCode    string  `json:"team_code"`
	CheckInAt   string  `json:"check_in_at"`
	CheckOutAt  *string `json:"check_out_at"`
}

func (f *flow) presence(token string, zone patrolZone, action string, lat float64) (int, checkInOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodPost, zoneURL(zone.ID, "/"+action), token, jsonBody(f.t, map[string]any{
		"qr_payload": zone.payload, "latitude": lat, "longitude": zoneLng, "accuracy_meters": 5,
	}), map[string]string{"Idempotency-Key": uuid.NewString()})
	var env struct {
		Data checkInOut `json:"data"`
	}
	if code < 300 {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) checkIns(token, path string) (int, []checkInOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, path, token, nil, nil)
	var env struct {
		Data []checkInOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func TestPatrolZoneCheckInOut(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone(nil, map[string]any{"type": "GUARD"})
	other := f.fencedZone(nil, map[string]any{"type": "GUARD"})

	t.Run("a check-out without a check-in is refused", func(t *testing.T) {
		code, _, raw := f.presence(f.personTok, z, "check-out", zoneLat)
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_NOT_CHECKED_IN")
	})

	t.Run("the QR and the geofence are checked like a scan", func(t *testing.T) {
		code, _, raw := f.presence(f.personTok, z, "check-in", zoneLat+0.001)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_OUTSIDE_RADIUS")
		bad := z
		bad.payload = other.payload
		code, _, raw = f.presence(f.personTok, bad, "check-in", zoneLat)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_QR_INVALID")
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM zone_checkins"))
	})

	var checkIn checkInOut
	t.Run("a check-in is stored with the guard's regu", func(t *testing.T) {
		code, got, raw := f.presence(f.personTok, z, "check-in", zoneLat)
		require.Equal(t, http.StatusCreated, code, string(raw))
		require.Equal(t, f.person, got.PersonnelID)
		require.Equal(t, "1K-IBMI-S1", got.TeamCode)
		require.Nil(t, got.CheckOutAt)
		checkIn = got
	})

	t.Run("checking in twice, here or in another zone, fails until check-out", func(t *testing.T) {
		code, _, raw := f.presence(f.personTok, z, "check-in", zoneLat)
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_ALREADY_CHECKED_IN")
		code, _, raw = f.presence(f.personTok, other, "check-in", zoneLat)
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_ALREADY_CHECKED_IN")
		code, _, raw = f.presence(f.personTok, other, "check-out", zoneLat)
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_NOT_CHECKED_IN")
	})

	t.Run("live shows the open check-in to the DANRU, with the name, and to the guard themself", func(t *testing.T) {
		code, live, raw := f.checkIns(danruTok, zoneURL(z.ID, "/live"))
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, live, 1)
		require.Equal(t, checkIn.ID, live[0].ID)
		require.Equal(t, "Anggota Satu A", *live[0].FullName)

		code, live, raw = f.checkIns(f.personTok, zoneURL(z.ID, "/live"))
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, live, 1)

		_, empty, _ := f.checkIns(danruTok, zoneURL(other.ID, "/live"))
		require.Empty(t, empty)
	})

	t.Run("a colleague sees only their own check-ins, a stranger nothing", func(t *testing.T) {
		colleague := f.addPerson(900430, "KOLEGA0004", "Kolega Empat")
		f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", colleague)
		tok := f.token(colleague, f.addSession(colleague, "tok-colleague4"))
		code, live, _ := f.checkIns(tok, zoneURL(z.ID, "/live"))
		require.Equal(t, http.StatusOK, code)
		require.Empty(t, live, "the other guard's presence is not theirs to see")

		stranger := f.addPerson(900431, "ORANGLAIN3", "Orang Lain Tiga")
		strangerTok := f.token(stranger, f.addSession(stranger, "tok-stranger3"))
		code, _, raw := f.checkIns(strangerTok, zoneURL(z.ID, "/live"))
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})

	t.Run("a check-out closes it, and live is empty again", func(t *testing.T) {
		code, got, raw := f.presence(f.personTok, z, "check-out", zoneLat)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, checkIn.ID, got.ID)
		require.NotNil(t, got.CheckOutAt)
		_, live, _ := f.checkIns(danruTok, zoneURL(z.ID, "/live"))
		require.Empty(t, live)
		code, _, raw = f.presence(f.personTok, z, "check-out", zoneLat)
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_NOT_CHECKED_IN")
	})

	t.Run("after a check-out the guard may check in again, anywhere", func(t *testing.T) {
		code, _, raw := f.presence(f.personTok, other, "check-in", zoneLat)
		require.Equal(t, http.StatusCreated, code, string(raw))
		code, _, raw = f.presence(f.personTok, other, "check-out", zoneLat)
		require.Equal(t, http.StatusOK, code, string(raw))
	})

	t.Run("tracking lists the check-ins of a date range, within the caller's reach", func(t *testing.T) {
		today := time.Now().UTC().Format(time.DateOnly)
		tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
		yesterday := time.Now().UTC().AddDate(0, 0, -2).Format(time.DateOnly)
		path := zoneURL(z.ID, "/tracking")

		code, got, raw := f.checkIns(danruTok, path+"?from="+yesterday+"&to="+tomorrow)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, got, 1)
		require.NotNil(t, got[0].CheckOutAt)
		require.Equal(t, "Anggota Satu A", *got[0].FullName)

		_, got, _ = f.checkIns(f.personTok, path+"?from="+yesterday+"&to="+tomorrow)
		require.Len(t, got, 1, "the guard sees their own")

		_, got, _ = f.checkIns(danruTok, path+"?from="+yesterday+"&to="+yesterday)
		require.Empty(t, got, "a range with no check-in")

		code, got, raw = f.checkIns(danruTok, path+"?from="+today+"&to="+tomorrow+"&personnel_id="+strconv.FormatInt(f.person, 10))
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, got, 1)

		code, _, raw = f.checkIns(f.personTok, path+"?from="+today+"&to="+tomorrow+"&personnel_id="+strconv.FormatInt(f.person+1000, 10))
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = f.checkIns(danruTok, path+"?from="+today+"&to="+tomorrow+"&team_code=1K-IBMI-S2")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})

	t.Run("tracking validates its range", func(t *testing.T) {
		path := zoneURL(z.ID, "/tracking")
		for name, q := range map[string]string{
			"no dates":          "",
			"a bad date":        "?from=01-01-2026&to=2026-01-02",
			"to before from":    "?from=2026-02-01&to=2026-01-01",
			"more than a month": "?from=2026-01-01&to=2026-03-01",
		} {
			code, _, raw := f.checkIns(danruTok, path+q)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
		}
	})

	t.Run("a zone that does not exist, or another organization's, is 404", func(t *testing.T) {
		code, _, raw := f.checkIns(danruTok, zoneURL(99999999, "/live"))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
		foreign, _, err := f.tokens.NewAccessToken(900999, 2, 5999)
		require.NoError(t, err)
		code, _, raw = f.checkIns(foreign, zoneURL(z.ID, "/live"))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
	})

	t.Run("a retried check-in with the same key returns the same answer", func(t *testing.T) {
		key := uuid.NewString()
		body := jsonBody(t, map[string]any{"qr_payload": z.payload, "latitude": zoneLat, "longitude": zoneLng, "accuracy_meters": 5})
		headers := map[string]string{"Idempotency-Key": key}
		code1, raw1 := f.do(http.MethodPost, zoneURL(z.ID, "/check-in"), f.personTok, body, headers)
		code2, raw2 := f.do(http.MethodPost, zoneURL(z.ID, "/check-in"), f.personTok, body, headers)
		require.Equal(t, http.StatusCreated, code1, string(raw1))
		require.Equal(t, http.StatusCreated, code2, string(raw2))
		var a, b struct {
			Data struct {
				ID int64 `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw1, &a))
		require.NoError(t, json.Unmarshal(raw2, &b))
		require.Equal(t, a.Data.ID, b.Data.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM zone_checkins WHERE check_out_at IS NULL"))
	})
}
