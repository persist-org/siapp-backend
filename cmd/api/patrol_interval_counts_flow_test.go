package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 10.19: when a checkpoint may be scanned again, where the guard is checked in, and how much of a zone
// a schedule covered.

func checkpointOf(t *testing.T, z zoneOut, id string) zoneCheckpointOut {
	t.Helper()
	for _, c := range z.Checkpoints {
		if c.ID == id {
			return c
		}
	}
	require.FailNow(t, "checkpoint not in the zone read", id)
	return zoneCheckpointOut{}
}

func TestPatrolNextAllowedAt(t *testing.T) {
	f := newFlow(t)
	f.exec("UPDATE projects SET patrol_interval_minutes = 30 WHERE code = '1K-IBMI-S'")
	z := f.fencedRound(2, nil)
	one, two := z.all[0], z.all[1]
	_, danruTok := f.danru()
	now := time.Now()
	first := now.Add(-10 * time.Minute)

	t.Run("nothing is returned before a scan", func(t *testing.T) {
		cp := checkpointOf(t, f.fieldZone(z.ID, f.personTok), one.ID)
		require.Nil(t, cp.LastScannedAt)
		require.Nil(t, cp.NextAllowedAt)
	})

	scan := f.mustScan(f.personTok, z, bodyAt(one, one.payload, first))

	t.Run("after a scan: last_scanned_at and last + the project interval", func(t *testing.T) {
		cp := checkpointOf(t, f.fieldZone(z.ID, f.personTok), one.ID)
		require.NotNil(t, cp.LastScannedAt)
		require.WithinDuration(t, first, *cp.LastScannedAt, time.Millisecond)
		require.NotNil(t, cp.NextAllowedAt)
		require.WithinDuration(t, first.Add(30*time.Minute), *cp.NextAllowedAt, time.Millisecond)
		require.Nil(t, checkpointOf(t, f.fieldZone(z.ID, f.personTok), two.ID).NextAllowedAt, "the other checkpoint has its own")
	})

	t.Run("the bundle says the same as the detail", func(t *testing.T) {
		code, list, raw := f.zoneList("/api/v1/zones?include=checkpoints", f.personTok)
		require.Equal(t, http.StatusOK, code, string(raw))
		var got *zoneOut
		for i := range list {
			if list[i].ID == z.ID {
				got = &list[i]
			}
		}
		require.NotNil(t, got)
		want := checkpointOf(t, f.fieldZone(z.ID, f.personTok), one.ID)
		have := checkpointOf(t, *got, one.ID)
		require.NotNil(t, have.NextAllowedAt)
		require.True(t, want.NextAllowedAt.Equal(*have.NextAllowedAt))
		require.True(t, want.LastScannedAt.Equal(*have.LastScannedAt))
	})

	t.Run("the refusal carries the same moment", func(t *testing.T) {
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(one, one.payload, first.Add(5*time.Minute)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "ZONE_INTERVAL_NOT_ELAPSED")
		d := detailsOf(t, raw)
		require.EqualValues(t, 1800, d["interval_seconds"])
		at, err := time.Parse(time.RFC3339, d["next_allowed_at"].(string))
		require.NoError(t, err)
		require.WithinDuration(t, first.Add(30*time.Minute), at, time.Second)
	})

	t.Run("a skip does not move it", func(t *testing.T) {
		f.mustSkip(f.personTok, z, skipBody(two, "STICKER_DAMAGED", now.Add(-5*time.Minute)))
		cp := checkpointOf(t, f.fieldZone(z.ID, f.personTok), two.ID)
		require.Nil(t, cp.LastScannedAt, "a skip is not a visit")
		require.Nil(t, cp.NextAllowedAt)
	})

	t.Run("a scan the DANRU declined does not count", func(t *testing.T) {
		code, raw := f.decline(danruTok, scan.ID, "foto buram")
		require.Equal(t, http.StatusOK, code, string(raw))
		cp := checkpointOf(t, f.fieldZone(z.ID, f.personTok), one.ID)
		require.Nil(t, cp.LastScannedAt)
		require.Nil(t, cp.NextAllowedAt)
		f.mustScan(f.personTok, z, bodyAt(one, one.payload, first.Add(time.Minute)))
	})
}

func TestPatrolOpenCheckIn(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	other := f.fencedZone(nil, nil)

	t.Run("none open is an empty list, not an error", func(t *testing.T) {
		code, got, raw := f.checkIns(f.personTok, "/api/v1/zones/check-ins/open")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotNil(t, got)
		require.Empty(t, got)
		require.Contains(t, string(raw), `"data":[]`)
	})

	code, in, raw := f.presence(f.personTok, z, "check-in", zoneLat)
	require.Equal(t, http.StatusCreated, code, string(raw))

	t.Run("the open check-in is listed", func(t *testing.T) {
		code, got, raw := f.checkIns(f.personTok, "/api/v1/zones/check-ins/open")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, got, 1)
		require.Equal(t, in.ID, got[0].ID)
		require.Nil(t, got[0].CheckOutAt)
	})

	t.Run("it is the caller's own: a colleague sees none", func(t *testing.T) {
		_, danruTok := f.danru()
		_, got, _ := f.checkIns(danruTok, "/api/v1/zones/check-ins/open")
		require.Empty(t, got)
	})

	t.Run("a second check-in names the zone and the checkpoint, here or in another zone", func(t *testing.T) {
		for _, target := range []patrolZone{z, other} {
			code, _, raw := f.presence(f.personTok, target, "check-in", zoneLat)
			reasonIs(t, code, raw, http.StatusConflict, "ZONE_ALREADY_CHECKED_IN")
			d := detailsOf(t, raw)
			require.EqualValues(t, z.ID, d["zone_id"], "the zone the guard is in, not the one they asked for")
			require.Equal(t, z.checkpoint.ID, d["checkpoint_id"])
		}
	})

	t.Run("after check-out the list is empty again", func(t *testing.T) {
		code, _, raw := f.presence(f.personTok, z, "check-out", zoneLat)
		require.Equal(t, http.StatusOK, code, string(raw))
		_, got, _ := f.checkIns(f.personTok, "/api/v1/zones/check-ins/open")
		require.Empty(t, got)
	})
}

func TestPatrolScheduleCheckpointCounts(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedRound(3, nil)
	now := time.Now()
	sc := f.makeSchedule(z.ID, f.person, now.Add(-10*time.Minute), 2*time.Hour)

	t.Run("a schedule starts at 0 of the zone's active checkpoints", func(t *testing.T) {
		require.Equal(t, 3, sc.CheckpointsTotal, "the answer to the write has the total too")
		got := f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, 0, got.CheckpointsScanned)
		require.Equal(t, 3, got.CheckpointsTotal)
	})

	code, raw := f.start(f.personTok, scheduledStart(sc.ID, now.Add(-5*time.Minute)))
	started := routeOf(t, code, raw, http.StatusCreated)

	f.mustScan(f.personTok, z, bodyAt(z.all[0], z.all[0].payload, now.Add(-2*time.Minute)))
	second := f.mustScan(f.personTok, z, bodyAt(z.all[1], z.all[1].payload, now.Add(-time.Minute)))

	t.Run("2 of 3 scanned", func(t *testing.T) {
		got := f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, 2, got.CheckpointsScanned)
		require.Equal(t, 3, got.CheckpointsTotal)
		require.Equal(t, 2, got.ScanCount)
	})

	t.Run("a declined scan does not count", func(t *testing.T) {
		code, raw := f.decline(danruTok, second.ID, "tidak jelas")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, 1, f.scheduleOf(f.personTok, sc.ID).CheckpointsScanned)
		// The order rule still counts the declined visit (a decline does not undo the walk), so the guard goes on
		// to the third checkpoint, and that makes 2 again.
		f.mustScan(f.personTok, z, bodyAt(z.all[2], z.all[2].payload, now.Add(-30*time.Second)))
		require.Equal(t, 2, f.scheduleOf(f.personTok, sc.ID).CheckpointsScanned)
	})

	t.Run("the status is still COMPLETED once the patrol ends, at 2/3", func(t *testing.T) {
		end, endRaw := f.end(f.personTok, started.ID, time.Now())
		require.Equal(t, http.StatusOK, end, string(endRaw))
		got := f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, "COMPLETED", got.Status)
		require.Equal(t, 2, got.CheckpointsScanned)
		require.Equal(t, 3, got.CheckpointsTotal)
	})

	t.Run("a checkpoint switched off leaves the total and the scanned count together", func(t *testing.T) {
		f.exec("UPDATE patrol_checkpoints SET active = false WHERE id = $1::uuid", z.all[0].ID)
		got := f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, 2, got.CheckpointsTotal)
		require.Equal(t, 1, got.CheckpointsScanned, "scanned never exceeds the total (cp 3 only; cp 1 is off, cp 2 declined)")
	})
}
