package attendance_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/attendance"
)

// Phase 07.5 Part C: what a request of leave, a permit or sick leave does to a check-in. The
// requests are written straight to the tables: deciding them is the leave module's, tested there.

// category adds a leave category with the flags that matter to attendance.
func (e *env) category(w world, code string, excusesLateness, timed bool) {
	e.t.Helper()
	e.exec(w.org, `INSERT INTO leave_categories (code, name, type, excuses_lateness, requires_time_range, makes_leave_days)
VALUES ($1, $1, 'PERMIT', $2, $3, $4) ON CONFLICT DO NOTHING`, code, excusesLateness, timed, !excusesLateness && !timed)
}

// leaveRequest adds a request of the guard in a status, over [from, to]. A "part of day" one has the
// window start..end ("08:00", "08:20"), a normal one has none.
func (e *env) leaveRequest(w world, category, status, from, to string, window ...string) int64 {
	e.t.Helper()
	var start, end any
	if len(window) == 2 {
		start, end = window[0], window[1]
	}
	var id int64
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), `
INSERT INTO leave_requests (number, personnel_id, team_code, project_code, type, category_id, start_date, end_date, start_time, end_time,
                            working_days, reason, status, decided_level, decided_at, cancelled_at)
SELECT 'T' || gen_random_uuid()::text, $1, '1K-IBMI-S1', '1K-IBMI-S', 'PERMIT', c.id, $3::date, $4::date, $5::time, $6::time,
       1, 'Uji', $2::leave_request_status,
       CASE WHEN $2 IN ('APPROVED', 'REJECTED') THEN 'DANRU'::leave_decided_level END,
       CASE WHEN $2 IN ('APPROVED', 'REJECTED') THEN now() END,
       CASE WHEN $2 = 'CANCELLED' THEN now() END
FROM leave_categories c WHERE c.code = $7 RETURNING id`, w.personID, status, from, to, start, end, category).Scan(&id))
	})
	return id
}

func (e *env) rawLate(w world) (raw, late int) {
	e.t.Helper()
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), "SELECT late_minutes_raw, late_minutes FROM attendances WHERE personnel_id = $1", w.personID).Scan(&raw, &late))
	})
	return raw, late
}

func TestACheckInUnderAnApprovedPermitIsHeldToLessLateness(t *testing.T) {
	// The clock is 08:30 on a shift that began at 08:00: 30 minutes late.
	cases := []struct {
		name      string
		category  string
		excuses   bool
		timed     bool
		status    string
		window    []string
		wantLate  int
		whyNoMore string
	}{
		{"nothing covers the day", "", false, false, "", nil, 30, ""},
		{"an on-duty full day excuses it", "ON_DUTY_FULL_DAY", true, false, "APPROVED", nil, 0, ""},
		{"a window of the morning takes its minutes off", "OTHER_PART_DAY", false, true, "APPROVED", []string{"08:00", "08:20"}, 10, ""},
		{"a window after the check-in takes nothing off", "OTHER_PART_DAY", false, true, "APPROVED", []string{"09:00", "10:00"}, 30, ""},
		{"a window that covers all of it can not go below zero", "OTHER_PART_DAY", false, true, "APPROVED", []string{"07:00", "12:00"}, 0, ""},
		{"a request still waiting excuses nothing", "ON_DUTY_FULL_DAY", true, false, "PENDING", nil, 30, ""},
		{"a rejected one neither", "ON_DUTY_FULL_DAY", true, false, "REJECTED", nil, 30, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			w := e.world()
			if c.category != "" {
				e.category(w, c.category, c.excuses, c.timed)
				e.leaveRequest(w, c.category, c.status, workDate, workDate, c.window...)
			}
			r := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
			require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
			require.Equal(t, c.wantLate, data[record](t, r).LateMinutes)
			raw, late := e.rawLate(w)
			require.Equal(t, 30, raw, "what the clock said is kept, whatever the permit says")
			require.Equal(t, c.wantLate, late)
		})
	}
}

func TestALivePermitSettlesThePreviousWorkingDay(t *testing.T) {
	t.Run("a pending one explains a day with no record", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.earlierCheckIn(w)
		e.previousDay(w, "none")
		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedIn)

		e.category(w, "UNPAID", false, false)
		e.leaveRequest(w, "UNPAID", "PENDING", "2026-03-08", "2026-03-09")
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	})

	t.Run("an approved one explains a day left open, too", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.previousDay(w, "open")
		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedOut)

		e.category(w, "UNPAID", false, false)
		e.leaveRequest(w, "UNPAID", "APPROVED", "2026-03-09", "2026-03-09")
		require.Equal(t, http.StatusCreated, e.checkIn(w, ref, siteLat, siteLng).Status)
	})

	t.Run("a rejected or canceled one explains nothing", func(t *testing.T) {
		for _, status := range []string{"REJECTED", "CANCELLED"} {
			e := newEnv(t)
			w := e.world()
			ref := e.onlyShift(w.token).ShiftRef
			e.earlierCheckIn(w)
			e.previousDay(w, "none")
			e.category(w, "UNPAID", false, false)
			e.leaveRequest(w, "UNPAID", status, "2026-03-09", "2026-03-09")
			e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedIn)
		}
	})

	t.Run("a request of another day explains nothing", func(t *testing.T) {
		e := newEnv(t)
		w := e.world()
		ref := e.onlyShift(w.token).ShiftRef
		e.earlierCheckIn(w)
		e.previousDay(w, "none")
		e.category(w, "UNPAID", false, false)
		e.leaveRequest(w, "UNPAID", "PENDING", "2026-03-10", "2026-03-12")
		e.checkIn(w, ref, siteLat, siteLng).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", attendance.ReasonPreviousNotCheckedIn)
	})
}

func TestTheDetailOfALeaveRecordNamesItsRequest(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	e.category(w, "UNPAID", false, false)
	request := e.leaveRequest(w, "UNPAID", "APPROVED", workDate, workDate)
	e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status, leave_request_id)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', $2, $3, '2026-03-10T01:00:00Z', '2026-03-10T10:00:00Z', 'Asia/Jakarta', 'LEAVE', $4)`, w.personID, workDate, w.shiftID, request)
	id := e.count(w.org, "SELECT id FROM attendances WHERE personnel_id = "+strconv.FormatInt(w.personID, 10))

	r := e.api(w.token, http.MethodGet, "/attendances/"+strconv.Itoa(id), nil, "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	got := data[struct {
		Status         string `json:"status"`
		LeaveRequestID *int64 `json:"leave_request_id"`
	}](t, r)
	require.Equal(t, "LEAVE", got.Status)
	require.NotNil(t, got.LeaveRequestID)
	require.Equal(t, request, *got.LeaveRequestID)
}
