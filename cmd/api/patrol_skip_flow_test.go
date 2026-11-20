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

// Phase 10.17 (docs/plan/10.3-patrol-mobile-feedback.md, BE-CP1): the position is a flag, not a refusal, and a
// checkpoint that cannot be scanned is skipped with a reason.

// skipBody is a skip of a checkpoint made at a moment.
func skipBody(cp patrolCheckpoint, reason string, at time.Time) map[string]any {
	return map[string]any{
		"local_id": uuid.NewString(), "occurred_at": at.UTC().Format(time.RFC3339Nano), "checkpoint_id": cp.ID, "reason": reason,
	}
}

// skip posts a skip with its local_id as the Idempotency-Key, as the app does.
func (f *flow) skip(token string, zoneID int64, body map[string]any) (int, scanOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(zoneID, 10)+"/skip", token, jsonBody(f.t, body),
		map[string]string{"Idempotency-Key": body["local_id"].(string)})
	var env struct {
		Data scanOut `json:"data"`
	}
	if code < 300 {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) mustSkip(token string, z patrolZone, body map[string]any) scanOut {
	f.t.Helper()
	code, s, raw := f.skip(token, z.ID, body)
	require.Equal(f.t, http.StatusCreated, code, string(raw))
	return s
}

// yesterdayNoon is a moment from which scans five minutes apart stay on one work date.
func yesterdayNoon(t *testing.T) time.Time {
	t.Helper()
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	y := time.Now().In(jakarta).AddDate(0, 0, -1)
	return time.Date(y.Year(), y.Month(), y.Day(), 12, 0, 0, 0, jakarta)
}

func TestPatrolScanLocationFlag(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)

	t.Run("no position is stored as NO_FIX, with no coordinates", func(t *testing.T) {
		b := scanBody(z)
		delete(b, "latitude")
		delete(b, "longitude")
		delete(b, "accuracy_meters")
		s := f.mustScan(f.personTok, z, b)
		require.Equal(t, "NO_FIX", s.LocationFlag)
		require.Equal(t, "SCANNED", s.Outcome)
		require.Nil(t, s.Latitude)
		require.Nil(t, s.DistanceMeters)
	})

	t.Run("half a position is a validation error naming what is missing", func(t *testing.T) {
		b := scanBody(z)
		delete(b, "accuracy_meters")
		code, _, raw := f.scan(f.personTok, z.ID, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Contains(t, fields, "accuracy_meters")
	})

	t.Run("a vague fix inside the radius is LOW_ACCURACY, a sharp one OK", func(t *testing.T) {
		b := scanBody(z)
		b["accuracy_meters"] = 80
		require.Equal(t, "LOW_ACCURACY", f.mustScan(f.personTok, z, b).LocationFlag)
		require.Equal(t, "OK", f.mustScan(f.personTok, z, scanBody(z)).LocationFlag)
	})

	t.Run("outside the radius wins over low accuracy", func(t *testing.T) {
		b := scanBody(z)
		b["latitude"], b["accuracy_meters"] = zoneLat+0.001, 80
		require.Equal(t, "OUTSIDE_RADIUS", f.mustScan(f.personTok, z, b).LocationFlag)
	})

	t.Run("a checkpoint with no geofence is OK from anywhere, and NO_FIX without a position", func(t *testing.T) {
		open := f.newZone(map[string]any{})
		b := scanBody(open)
		b["latitude"] = zoneLat + 1
		require.Equal(t, "OK", f.mustScan(f.personTok, open, b).LocationFlag)
	})

	t.Run("the flag is read back with the scan", func(t *testing.T) {
		b := scanBody(z)
		b["latitude"] = zoneLat + 0.001
		s := f.mustScan(f.personTok, z, b)
		code, got, raw := f.getScan(f.personTok, s.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, "OUTSIDE_RADIUS", got.LocationFlag)
		require.Equal(t, "SCANNED", got.Outcome)
	})
}

// The case of the feedback: five checkpoints scanned offline in a basement, the second with a bad fix. Each
// is stored, none is refused, so the rest of the round is not lost.
func TestPatrolOfflineRoundWithABadFix(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(5, nil)
	base := yesterdayNoon(t)

	want := []string{"OK", "OUTSIDE_RADIUS", "NO_FIX", "OK", "OK"}
	for i, cp := range z.all {
		b := bodyAt(cp, cp.payload, base.Add(time.Duration(i)*5*time.Minute))
		b["submitted_offline"] = true
		switch want[i] {
		case "OUTSIDE_RADIUS":
			b["latitude"] = zoneLat + 0.01
		case "NO_FIX":
			delete(b, "latitude")
			delete(b, "longitude")
			delete(b, "accuracy_meters")
		}
		s := f.mustScan(f.personTok, z, b)
		require.Equal(t, want[i], s.LocationFlag, "checkpoint %d", i+1)
	}
	require.Equal(t, 5, countOf(t, f, "SELECT count(*) FROM patrol_scans WHERE zone_id = $1", z.ID))
}

func TestPatrolSkip(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	base := yesterdayNoon(t)

	t.Run("a skipped checkpoint lets the round go on", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, base))

		// Without the skip the third is behind the second.
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(z.all[2], z.all[2].payload, base.Add(10*time.Minute)))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")

		s := f.mustSkip(f.personTok, z, skipBody(z.all[1], "STICKER_DAMAGED", base.Add(5*time.Minute)))
		require.Equal(t, "SKIPPED", s.Outcome)
		require.NotNil(t, s.SkipReason)
		require.Equal(t, "STICKER_DAMAGED", *s.SkipReason)
		require.Equal(t, z.all[1].ID, s.Checkpoint.ID)
		require.Empty(t, s.Actions)

		f.mustScan(f.personTok, z, bodyAt(z.all[2], z.all[2].payload, base.Add(10*time.Minute)))
	})

	t.Run("a skip is judged by the order too", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		code, _, raw := f.skip(f.personTok, z.ID, skipBody(z.all[2], "ACCESS_BLOCKED", base))
		reasonIs(t, code, raw, http.StatusConflict, "CHECKPOINT_OUT_OF_ORDER")
		require.Equal(t, z.all[1].ID, detailsOf(t, raw)["expected_checkpoint_id"])
	})

	t.Run("it needs no QR and no position, and a reason that is OTHER needs a note", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		b := skipBody(z.all[0], "OTHER", base)
		code, _, raw := f.skip(f.personTok, z.ID, b)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Contains(t, fields, "note")

		// A corrected request is a new one: the same key with another body is IDEMPOTENCY_KEY_REUSED.
		b["note"], b["local_id"] = "ruangan direnovasi", uuid.NewString()
		s := f.mustSkip(f.personTok, z, b)
		require.Equal(t, "NO_FIX", s.LocationFlag)
		require.Equal(t, "OTHER", *s.SkipReason)

		bad := skipBody(z.all[1], "BORED", base.Add(5*time.Minute))
		code, _, raw = f.skip(f.personTok, z.ID, bad)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a checkpoint of another zone is refused", func(t *testing.T) {
		z, other := f.fencedRound(2, nil), f.fencedRound(2, nil)
		code, _, raw := f.skip(f.personTok, z.ID, skipBody(other.all[0], "STICKER_DAMAGED", base))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_NOT_IN_ZONE")
	})

	t.Run("a replay returns the stored skip and stores nothing more", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		b := skipBody(z.all[0], "STICKER_DAMAGED", base)
		first := f.mustSkip(f.personTok, z, b)
		// The Idempotency-Key middleware answers the replay with the stored answer (201); the service's own
		// replay (a new key, the same local_id) is 200. Either way it is the same skip.
		code, again, raw := f.skip(f.personTok, z.ID, b)
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, code, string(raw))
		require.Equal(t, first.ID, again.ID)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_scans WHERE zone_id = $1", z.ID))
	})

	t.Run("a double tap or a retry with a new local_id does not store a second skip", func(t *testing.T) {
		z := f.fencedRound(3, nil)
		f.mustSkip(f.personTok, z, skipBody(z.all[0], "STICKER_DAMAGED", base))

		// Ten seconds later, either side of the first: a new local_id, so not a replay.
		for _, at := range []time.Time{base.Add(10 * time.Second), base.Add(-10 * time.Second)} {
			code, _, raw := f.skip(f.personTok, z.ID, skipBody(z.all[0], "STICKER_DAMAGED", at))
			reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
			require.Equal(t, float64(30), detailsOf(t, raw)["interval_seconds"])
			require.NotEmpty(t, detailsOf(t, raw)["next_allowed_at"])
		}
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_scans WHERE zone_id = $1", z.ID))

		// The round is not left a step behind: the checkpoints after it follow, and the next one is the second.
		f.mustSkip(f.personTok, z, skipBody(z.all[1], "ACCESS_BLOCKED", base.Add(5*time.Minute)))
		f.mustScan(f.personTok, z, bodyAt(z.all[2], z.all[2].payload, base.Add(10*time.Minute)))

		// A skip 20 minutes later is another row, as a second round's would be.
		f.mustSkip(f.personTok, z, skipBody(z.all[0], "STICKER_DAMAGED", base.Add(20*time.Minute)))
	})

	t.Run("a skip is not a visit: the interval ignores it, and so do the circles", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		f.mustSkip(f.personTok, z, skipBody(z.all[0], "STICKER_DAMAGED", base))
		// The same checkpoint scanned a few seconds later: a skip does not trip the interval.
		s := f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, base.Add(5*time.Second)))
		require.Equal(t, "SCANNED", s.Outcome)

		_, list, _ := f.progress(f.personTok, "?work_date="+base.Format("2006-01-02"))
		p := progressOf(t, list, z.ID)
		require.Equal(t, 1, p.Submitted, "only the scan")
		require.Equal(t, 1, p.Skipped)
		require.Equal(t, 1, p.Checkpoints[0].Submitted)
		require.Equal(t, 1, p.Checkpoints[0].Skipped)
		require.Zero(t, p.Checkpoints[1].Skipped)
	})

	t.Run("the DANRU can decline a skip; the order still counts it, the skipped counter does not", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		s := f.mustSkip(f.personTok, z, skipBody(z.all[0], "ACCESS_BLOCKED", base))
		code, raw := f.decline(danruTok, s.ID, "pintunya tidak terkunci")
		require.Equal(t, http.StatusOK, code, string(raw))

		f.mustScan(f.personTok, z, bodyAt(z.all[1], z.all[1].payload, base.Add(5*time.Minute)))
		_, list, _ := f.progress(f.personTok, "?work_date="+base.Format("2006-01-02"))
		p := progressOf(t, list, z.ID)
		require.Zero(t, p.Skipped)
		require.Zero(t, p.Declined, "a declined skip is not a declined visit")
	})

	t.Run("its photos are read back with the scan", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		photo := f.photo(f.person)
		b := skipBody(z.all[0], "STICKER_DAMAGED", base)
		b["media_ids"] = []int64{photo}
		s := f.mustSkip(f.personTok, z, b)
		code, got, raw := f.getScan(f.personTok, s.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, "SKIPPED", got.Outcome)
		require.Len(t, got.Photos, 1)
		require.Equal(t, photo, got.Photos[0].ID)

		dup := skipBody(z.all[1], "STICKER_DAMAGED", base.Add(5*time.Minute))
		dup["media_ids"] = []int64{photo, photo}
		code, _, raw = f.skip(f.personTok, z.ID, dup)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("the DANRU is told, and the patrol shows the skip without counting it", func(t *testing.T) {
		z := f.fencedRound(2, nil)
		now := time.Now()
		started := f.mustStart(f.personTok, now.Add(-20*time.Minute))
		b := skipBody(z.all[0], "STICKER_DAMAGED", now.Add(-10*time.Minute))
		s := f.mustSkip(f.personTok, z, b)

		var patrolID *int64
		f.query([]any{&patrolID}, "SELECT patrol_id FROM patrol_scans WHERE id = $1", s.ID)
		require.NotNil(t, patrolID)
		require.Equal(t, started.ID, *patrolID)
		require.Zero(t, countOf(t, f, "SELECT scan_count FROM patrol_routes WHERE id = $1", started.ID), "not a visit")

		code, raw := f.getPatrol(f.personTok, started.ID)
		r := routeOf(t, code, raw, http.StatusOK)
		require.Len(t, r.Scans, 1)
		require.Equal(t, "SKIPPED", r.Scans[0].Outcome)

		code, raw = f.end(f.personTok, started.ID, now.Add(-5*time.Minute))
		r = routeOf(t, code, raw, http.StatusOK)
		require.Zero(t, r.ScanCount)
	})
}

func TestPatrolSkipSchema(t *testing.T) {
	f := newFlow(t)
	z := f.fencedZone(nil, nil)
	s := f.mustScan(f.personTok, z, scanBody(z))

	for name, sql := range map[string]string{
		"a reason without a skip": "UPDATE patrol_scans SET skip_reason = 'OTHER' WHERE id = $1",
		"a skip without a reason": "UPDATE patrol_scans SET outcome = 'SKIPPED' WHERE id = $1",
		"half a position":         "UPDATE patrol_scans SET accuracy_meters = NULL WHERE id = $1",
		"no position but a fix":   "UPDATE patrol_scans SET latitude = NULL, longitude = NULL, accuracy_meters = NULL WHERE id = $1",
		"photos on a scan":        "UPDATE patrol_scans SET skip_media_ids = '{1}' WHERE id = $1",
	} {
		require.Error(t, f.tryExec(sql, s.ID), name)
	}
}
