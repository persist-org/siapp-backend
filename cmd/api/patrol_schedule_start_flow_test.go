package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Phase 10.12: a patrol started from a schedule, and the device condition.

// scheduledStart is the body of a start from a schedule: no project_code, the phone's condition when given.
func scheduledStart(scheduleID string, when time.Time) map[string]any {
	b := startBody(when)
	delete(b, "project_code")
	b["schedule_id"] = scheduleID
	return b
}

// makeSchedule makes a live schedule of the person through the API and returns it.
func (f *flow) makeSchedule(zone, person int64, start time.Time, d time.Duration) scheduleOut {
	f.t.Helper()
	code, got, raw := f.createSchedules(zone, []int64{person}, slotAt(start, d))
	require.Equal(f.t, http.StatusCreated, code, string(raw))
	return got[0]
}

// insertSchedule writes a schedule straight to the table: the API refuses a window that is over, and a test
// needs one (a late start, an offline start synced after the window).
func (f *flow) insertSchedule(zone, person int64, start time.Time, d time.Duration) string {
	f.t.Helper()
	var id string
	f.query([]any{&id}, `INSERT INTO patrol_schedules (project_code, zone_id, personnel_id, starts_at, ends_at, work_date, created_by_id)
	    VALUES ('1K-IBMI-S', $1, $2, $3::timestamptz, $3::timestamptz + $4::interval, ($3::timestamptz AT TIME ZONE 'Asia/Jakarta')::date, $2)
	    RETURNING id::text`, zone, person, start.UTC().Format(time.RFC3339Nano), d.String())
	return id
}

func (f *flow) scheduleOf(token, id string) scheduleOut {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, "/api/v1/patrol-schedules/"+id, token, nil, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	var env struct {
		Data scheduleOut `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

type patrolRead struct {
	routeOut
	Zone *struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"zone"`
	Schedule *struct {
		ID       string `json:"id"`
		StartsAt string `json:"starts_at"`
		EndsAt   string `json:"ends_at"`
	} `json:"schedule"`
	StartDelayMinutes *int `json:"start_delay_minutes"`
	DeviceCondition   *struct {
		Start map[string]any `json:"start"`
		End   map[string]any `json:"end"`
	} `json:"device_condition"`
}

func (f *flow) patrolRead(token string, id int64) patrolRead {
	f.t.Helper()
	code, raw := f.getPatrol(token, id)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	var env struct {
		Data patrolRead `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	return env.Data
}

func TestPatrolStartFromSchedule(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	now := time.Now()
	sc := f.makeSchedule(z.ID, f.person, now.Add(-10*time.Minute), 2*time.Hour)
	var started routeOut

	t.Run("starting inside the window sets the zone and the schedule, and reports the delay", func(t *testing.T) {
		code, raw := f.start(f.personTok, scheduledStart(sc.ID, now))
		started = routeOf(t, code, raw, http.StatusCreated)
		require.Equal(t, "1K-IBMI-S", started.ProjectCode, "the schedule's project, though the app sent none")

		got := f.patrolRead(f.personTok, started.ID)
		require.NotNil(t, got.Zone)
		require.Equal(t, z.ID, got.Zone.ID)
		require.Equal(t, z.Name, got.Zone.Name)
		require.NotNil(t, got.Schedule)
		require.Equal(t, sc.ID, got.Schedule.ID)
		require.NotNil(t, got.StartDelayMinutes)
		require.Equal(t, 10, *got.StartDelayMinutes, "ten minutes late")
	})

	t.Run("the schedule is IN_PROGRESS, then COMPLETED after the end, and keeps the delay", func(t *testing.T) {
		got := f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, "IN_PROGRESS", got.Status)
		code, raw := f.end(f.personTok, started.ID, time.Now())
		require.Equal(t, http.StatusOK, code, string(raw))
		got = f.scheduleOf(f.personTok, sc.ID)
		require.Equal(t, "COMPLETED", got.Status)
		code, list, _ := f.scheduleList(f.adminTok, schedulesPath+"?status=COMPLETED")
		require.Equal(t, http.StatusOK, code)
		require.Len(t, list, 1)
		_, none, _ := f.scheduleList(f.adminTok, schedulesPath+"?status=SCHEDULED")
		require.Empty(t, none)
	})

	t.Run("a schedule with a patrol can no longer be edited or cancelled", func(t *testing.T) {
		code, raw := f.do(http.MethodPatch, schedulesPath+"/"+sc.ID, f.adminTok, jsonBody(t, map[string]any{"notes": "x"}), nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_LOCKED")
		code, raw = f.do(http.MethodDelete, schedulesPath+"/"+sc.ID, f.adminTok, nil, nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_LOCKED")
	})

	t.Run("a second patrol from the same schedule is refused", func(t *testing.T) {
		code, raw := f.start(f.personTok, scheduledStart(sc.ID, time.Now()))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_ALREADY_STARTED")
	})

	t.Run("the list narrows to a schedule or a zone", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, "/api/v1/patrols?schedule_id="+sc.ID, f.personTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		var env struct {
			Data []patrolRead `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Len(t, env.Data, 1)
		require.NotNil(t, env.Data[0].Schedule)
		require.NotNil(t, env.Data[0].Zone)
		require.Nil(t, env.Data[0].DeviceCondition, "the list never carries the device condition")

		for _, q := range []string{"?zone_id=99999999", "?schedule_id=" + uuid.NewString()} {
			code, raw = f.do(http.MethodGet, "/api/v1/patrols"+q, f.personTok, nil, nil)
			require.Equal(t, http.StatusOK, code, string(raw))
			require.NoError(t, json.Unmarshal(raw, &env))
			require.Empty(t, env.Data)
		}
		code, raw = f.do(http.MethodGet, "/api/v1/patrols?schedule_id=nope", f.personTok, nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, raw = f.do(http.MethodGet, "/api/v1/patrols?zone_id=0", f.personTok, nil, nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})
}
