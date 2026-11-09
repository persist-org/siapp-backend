package leave

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// minutesOf is a time of day as minutes after midnight.
func minutesOf(t pgtype.Time) int {
	return int(t.Microseconds / int64(time.Minute/time.Microsecond))
}

// EffectiveLateness is the lateness a person is held to for a check-in, given what the clock said
// (raw: whole minutes after the start of the shift, 0 when on time) and the approved requests that
// cover the work date (KPI attendance/logic.go:182-213):
//
//   - a category that excuses lateness (an on-duty full day) zeroes it;
//   - a "part of day" category takes its own window off: the minutes of the window that fall in
//     [shiftStart, checkIn) no longer count as late.
//
// It never goes below 0. It is computed from the raw figure every time, never from a previous result,
// so a request revoked later gives the lateness back (RecomputeLateness). Check-in calls it with the
// requests there are then; approving and revoking call RecomputeLateness for the days they touch.
func EffectiveLateness(ctx context.Context, q *db.Queries, personnelID int64, workDate timeutil.Date, shiftStart, checkIn time.Time, raw int, loc *time.Location) (int, error) {
	if raw <= 0 {
		return 0, nil
	}
	covering, err := q.ListApprovedLeaveCovering(ctx, db.ListApprovedLeaveCoveringParams{PersonnelID: personnelID, OnDate: dateParam(workDate)})
	if err != nil {
		return 0, fmt.Errorf("list approved leave covering: %w", err)
	}
	late := raw
	for _, c := range covering {
		if c.ExcusesLateness {
			return 0, nil
		}
		if !c.RequiresTimeRange || !c.StartTime.Valid || !c.EndTime.Valid {
			continue
		}
		from := timeutil.At(workDate, minutesOf(c.StartTime), loc)
		to := timeutil.At(workDate, minutesOf(c.EndTime), loc)
		if from.Before(shiftStart) {
			from = shiftStart
		}
		if to.After(checkIn) {
			to = checkIn
		}
		if to.After(from) {
			late -= int(to.Sub(from) / time.Minute)
		}
	}
	return max(0, late), nil
}

// RecomputeLateness holds the people's checked-in records of [from, to] to the lateness the
// approved requests now give them, from the raw figure of each: after an approval it lowers what
// was counted, after a revoke it puts it back. Only rows whose figure changes are written.
func RecomputeLateness(ctx context.Context, q *db.Queries, now time.Time, personnelID int64, from, to timeutil.Date) error {
	rows, err := q.ListCheckedInAttendancesBetween(ctx, db.ListCheckedInAttendancesBetweenParams{
		PersonnelID: personnelID, DateFrom: dateParam(from), DateTo: dateParam(to),
	})
	if err != nil {
		return fmt.Errorf("list checked-in attendances: %w", err)
	}
	for _, r := range rows {
		loc, err := time.LoadLocation(r.Timezone)
		if err != nil {
			return fmt.Errorf("load location %s: %w", r.Timezone, err)
		}
		var workDate timeutil.Date
		_ = workDate.ScanDate(r.WorkDate)
		late, err := EffectiveLateness(ctx, q, personnelID, workDate, r.ShiftStartsAt.Time, r.CheckInAt.Time, int(r.LateMinutesRaw), loc)
		if err != nil {
			return err
		}
		if late == int(r.LateMinutes) {
			continue
		}
		if err := q.SetAttendanceLateMinutes(ctx, db.SetAttendanceLateMinutesParams{ID: r.ID, LateMinutes: int32(late), Now: ts(now)}); err != nil { //nolint:gosec // a number of minutes in a day
			return fmt.Errorf("set late minutes: %w", err)
		}
	}
	return nil
}
