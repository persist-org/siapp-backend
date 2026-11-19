package main

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/patrol"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/testkit"
)

// Phase 10.12, continued: the window of a schedule, whose schedule it is, two starts at once, the device
// condition and what a schedule counts.

func TestPatrolStartWindowOfASchedule(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	now := time.Now()

	t.Run("twenty minutes before the window is refused, ten minutes before is accepted", func(t *testing.T) {
		early := f.insertSchedule(z.ID, f.person, now.Add(20*time.Minute), time.Hour)
		code, raw := f.start(f.personTok, scheduledStart(early, now))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "PATROL_OUTSIDE_SCHEDULE")
		require.Contains(t, string(raw), "earliest_start")

		near := f.insertSchedule(z.ID, f.person, now.Add(10*time.Minute), time.Hour)
		code, raw = f.start(f.personTok, scheduledStart(near, now))
		got := routeOf(t, code, raw, http.StatusCreated)
		code, raw = f.end(f.personTok, got.ID, time.Now())
		require.Equal(t, http.StatusOK, code, string(raw))
		read := f.patrolRead(f.personTok, got.ID)
		require.NotNil(t, read.StartDelayMinutes)
		require.Zero(t, *read.StartDelayMinutes, "an early start has no delay")
	})

	t.Run("after the window is over is refused", func(t *testing.T) {
		over := f.insertSchedule(z.ID, f.person, now.Add(-3*time.Hour), time.Hour)
		code, raw := f.start(f.personTok, scheduledStart(over, now))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "PATROL_OUTSIDE_SCHEDULE")
	})

	t.Run("an offline start made inside the window and synced after it is accepted", func(t *testing.T) {
		past := f.insertSchedule(z.ID, f.person, now.Add(-3*time.Hour), 2*time.Hour)
		body := scheduledStart(past, now.Add(-150*time.Minute))
		body["submitted_offline"] = true
		code, raw := f.start(f.personTok, body)
		got := routeOf(t, code, raw, http.StatusCreated)
		require.Equal(t, 30, *f.patrolRead(f.personTok, got.ID).StartDelayMinutes, "judged on started_at: thirty minutes in")
	})
}

func TestPatrolStartFromSomeoneElsesOrCancelledSchedule(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	other, _ := f.addMember("1K-IBMI-S1", "MEMBER", 900811, "STARTJDW01", "Anggota Mulai")
	now := time.Now()
	theirs := f.makeSchedule(z.ID, other, now, 2*time.Hour)
	mine := f.makeSchedule(z.ID, f.person, now.Add(3*time.Hour), time.Hour)

	t.Run("someone else's schedule is 404", func(t *testing.T) {
		code, raw := f.start(f.personTok, scheduledStart(theirs.ID, now))
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")
	})
	t.Run("a cancelled schedule, an unknown one and a non-uuid are 404", func(t *testing.T) {
		code, raw := f.do(http.MethodDelete, schedulesPath+"/"+mine.ID, f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		for _, id := range []string{mine.ID, uuid.NewString(), "nope"} {
			code, raw = f.start(f.personTok, scheduledStart(id, now))
			reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")
		}
	})
	t.Run("another project than the schedule's is refused", func(t *testing.T) {
		body := scheduledStart(f.makeSchedule(z.ID, f.person, now.Add(6*time.Hour), time.Hour).ID, now)
		body["project_code"] = "1K-IBMI-C"
		code, raw := f.start(f.personTok, body)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Contains(t, fields, "project_code")
	})
}

func TestPatrolStartOfTheSameScheduleTwiceAtOnce(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	now := time.Now()
	sc := f.makeSchedule(z.ID, f.person, now.Add(-5*time.Minute), 2*time.Hour)

	bodies := []map[string]any{scheduledStart(sc.ID, time.Now()), scheduledStart(sc.ID, time.Now())}
	t.Run("two different starts at once: one patrol", func(t *testing.T) {
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[i], _ = f.start(f.personTok, bodies[i])
			}()
		}
		wg.Wait()
		require.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, codes)
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_routes WHERE schedule_id = $1", sc.ID))
	})

	t.Run("a replay of the winning start, when the key is gone, returns the stored patrol", func(t *testing.T) {
		var localID string
		f.query([]any{&localID}, "SELECT local_id::text FROM patrol_routes WHERE schedule_id = $1", sc.ID)
		winner := bodies[0]
		if bodies[1]["local_id"] == localID {
			winner = bodies[1]
		}
		f.exec("DELETE FROM idempotency_keys")
		code, raw := f.start(f.personTok, winner)
		got := routeOf(t, code, raw, http.StatusOK)
		require.Equal(t, localID, got.LocalID)
	})
}

func TestPatrolStartAdHocStillWorks(t *testing.T) {
	f := newFlow(t)
	started := f.mustStart(f.personTok, time.Now())
	got := f.patrolRead(f.personTok, started.ID)
	require.Nil(t, got.Zone)
	require.Nil(t, got.Schedule)
	require.Nil(t, got.StartDelayMinutes)
	require.Nil(t, got.DeviceCondition, "no condition was sent")

	f.end(f.personTok, started.ID, time.Now())
	body := startBody(time.Now())
	delete(body, "project_code")
	code, raw := f.start(f.personTok, body)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	_, fields := problem(t, raw)
	require.Contains(t, fields, "project_code", "an ad-hoc patrol needs its project")
}

func TestPatrolDeviceCondition(t *testing.T) {
	f := newFlow(t)
	startCond := map[string]any{"battery_level": 80, "is_charging": false, "network_type": "wifi", "gps_enabled": true, "mock_location": true, "vendor_flag": "x"}
	endCond := map[string]any{"battery_level": 55, "network_type": "cellular"}

	bad := startBody(time.Now())
	bad["device_condition"] = map[string]any{"battery_level": 150}
	code, raw := f.start(f.personTok, bad)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	reason, _ := problem(t, raw)
	require.Equal(t, "DEVICE_CONDITION_INVALID", reason)
	require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_routes"), "a bad shape starts nothing")

	body := startBody(time.Now().Add(-10 * time.Minute))
	body["device_condition"] = startCond
	code, raw = f.start(f.personTok, body)
	started := routeOf(t, code, raw, http.StatusCreated)

	endedAt := time.Now()
	t.Run("a bad condition at the end is refused and records nothing", func(t *testing.T) {
		code, raw := f.patrolDo(http.MethodPost, patrolURL(started.ID, "/end"), f.personTok,
			map[string]any{"ended_at": endedAt.UTC().Format(time.RFC3339Nano), "device_condition": []int{1}}, uuid.NewString())
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM patrol_route_metadata"))
	})

	t.Run("the end records its own condition and the detail reads both back", func(t *testing.T) {
		code, raw := f.patrolDo(http.MethodPost, patrolURL(started.ID, "/end"), f.personTok,
			map[string]any{"ended_at": endedAt.UTC().Format(time.RFC3339Nano), "device_condition": endCond}, uuid.NewString())
		require.Equal(t, http.StatusOK, code, string(raw))
		got := f.patrolRead(f.personTok, started.ID)
		require.NotNil(t, got.DeviceCondition)
		require.Equal(t, true, got.DeviceCondition.Start["mock_location"], "mock_location is recorded, not a gate")
		require.Equal(t, "x", got.DeviceCondition.Start["vendor_flag"], "unknown keys are kept")
		require.EqualValues(t, 55, got.DeviceCondition.End["battery_level"])
	})

	t.Run("a replayed end keeps the first END row", func(t *testing.T) {
		code, raw := f.patrolDo(http.MethodPost, patrolURL(started.ID, "/end"), f.personTok,
			map[string]any{"ended_at": endedAt.UTC().Format(time.RFC3339Nano), "device_condition": map[string]any{"battery_level": 1}}, uuid.NewString())
		require.Equal(t, http.StatusOK, code, string(raw))
		require.EqualValues(t, 55, f.patrolRead(f.personTok, started.ID).DeviceCondition.End["battery_level"])
		require.Equal(t, 2, countOf(t, f, "SELECT count(*) FROM patrol_route_metadata"))
	})

	t.Run("the list does not carry it, and a DANRU reads the detail", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, "/api/v1/patrols", f.personTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotContains(t, string(raw), "device_condition")
		_, danruTok := f.danru()
		require.NotNil(t, f.patrolRead(danruTok, started.ID).DeviceCondition)
	})

	t.Run("the rows are evidence: the app role cannot change or delete them", func(t *testing.T) {
		require.Error(t, f.tryExec("UPDATE patrol_route_metadata SET device_condition = '{}'"))
		require.Error(t, f.tryExec("DELETE FROM patrol_route_metadata"))
		require.Error(t, f.tryExec(`INSERT INTO patrol_route_metadata (patrol_id, kind, device_condition, recorded_at)
		    VALUES ($1, 'START', '[]', now())`, started.ID), "a condition is an object")
	})
}

func TestPatrolAutoEndedHasOnlyAStartCondition(t *testing.T) {
	f := newFlow(t)
	svc := patrol.NewService(testkit.Pool(t), clock.System{})
	body := startBody(time.Now().Add(-time.Hour))
	body["device_condition"] = map[string]any{"battery_level": 20}
	code, raw := f.start(f.personTok, body)
	started := routeOf(t, code, raw, http.StatusCreated)
	f.exec(`UPDATE patrol_routes SET started_at = now() - interval '8 hours', last_point_at = now() - interval '7 hours' WHERE id = $1`, started.ID)

	ended, err := svc.AutoEndStale(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, ended)
	got := f.patrolRead(f.personTok, started.ID)
	require.Equal(t, "AUTO_ENDED", got.Status)
	require.NotNil(t, got.DeviceCondition)
	require.NotNil(t, got.DeviceCondition.Start)
	require.Nil(t, got.DeviceCondition.End)
}

func TestPatrolScheduleCountsOnlyScansInItsZone(t *testing.T) {
	f := newFlow(t)
	zone := f.fencedRound(1, nil)
	elsewhere := f.fencedZone(nil, map[string]any{"name": "Zona Lain Jadwal"})
	now := time.Now()
	sc := f.makeSchedule(zone.ID, f.person, now.Add(-10*time.Minute), 2*time.Hour)
	code, raw := f.start(f.personTok, scheduledStart(sc.ID, now.Add(-5*time.Minute)))
	started := routeOf(t, code, raw, http.StatusCreated)

	f.mustScan(f.personTok, zone, scanBody(zone))
	f.mustScan(f.personTok, elsewhere, scanBody(elsewhere))

	end, endRaw := f.end(f.personTok, started.ID, time.Now())
	require.Equal(t, http.StatusOK, end, string(endRaw))
	require.Equal(t, 2, f.patrolRead(f.personTok, started.ID).ScanCount, "both scans joined the patrol")
	require.Equal(t, 1, f.scheduleOf(f.personTok, sc.ID).ScanCount, "the schedule counts the scan in its own zone")
}

func TestPatrolRouteScheduleLinkSchema(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")
	zone := f.addZone("1K-IBMI-S", "Zona Skema Rute")
	otherProject := f.addZone("1K-IBMI-C", "Zona Skema Rute C")
	sc := f.insertSchedule(zone, f.person, time.Now(), time.Hour)
	insert := func(localID string, zoneID any, scheduleID any) error {
		return f.tryExec(`INSERT INTO patrol_routes (local_id, personnel_id, team_code, project_code, started_at, work_date, last_point_at, zone_id, schedule_id)
		    VALUES ($1, $2, '1K-IBMI-S1', '1K-IBMI-S', now(), current_date, now(), $3, $4)`, localID, f.person, zoneID, scheduleID)
	}

	require.NoError(t, insert(uuid.NewString(), nil, nil), "an ad-hoc patrol has neither")
	require.Error(t, insert(uuid.NewString(), nil, sc), "a schedule needs a zone")
	require.Error(t, insert(uuid.NewString(), otherProject, nil), "a zone of another project than the patrol's")
	require.NoError(t, f.tryExec("UPDATE patrol_routes SET status = 'ENDED', ended_at = now(), path = '[]'"))
	require.NoError(t, insert(uuid.NewString(), zone, sc))
	require.NoError(t, f.tryExec("UPDATE patrol_routes SET status = 'ENDED', ended_at = now(), path = '[]'"))
	require.Error(t, insert(uuid.NewString(), zone, sc), "one patrol per schedule")
}
