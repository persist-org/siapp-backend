package organization

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Check-in may start this long before a shift, and check-out may end this long after it.
const (
	checkInEarly = 120 * time.Minute
	checkOutLate = 240 * time.Minute
)

// ShiftInstance is one scheduled shift of a person turned into real instants.
type ShiftInstance struct {
	ScheduleID       int64
	WorkDate         timeutil.Date
	ShiftID          int64
	ShiftName        string
	IsOff            bool
	ToleranceMinutes int
	// Location is the timezone the shift is worked in: its project's (DL-03).
	Location *time.Location
	// ProjectCodes are the projects of the person's regu on WorkDate (usually one). Empty
	// when the person is in no regu that day: the instance is then in WIB, and has no site.
	ProjectCodes []string
	// StartsAt and EndsAt are UTC. An overnight shift ends on WorkDate + 1. Both are zero for
	// an OFF day.
	StartsAt time.Time
	EndsAt   time.Time
}

// Shifts resolves a person's concrete shifts: what attendance and patrol ask "when is this
// person working". It is an interface because D1 may later move the source to Personalia's
// m_harikerja; LocalShifts reads SiAPP's own schedules.
type Shifts interface {
	// Instances returns the person's shifts with a work date in [from, to], OFF days included.
	Instances(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]ShiftInstance, error)
	// Current returns the working shifts whose check-in/check-out window contains at.
	Current(ctx context.Context, organizationID, personnelID int64, at time.Time) ([]ShiftInstance, error)
}

// LocalShifts is Shifts over SiAPP's schedules table.
type LocalShifts struct {
	pool *pgxpool.Pool
}

var _ Shifts = (*LocalShifts)(nil)

// NewShifts returns the local Shifts.
func NewShifts(pool *pgxpool.Pool) *LocalShifts { return &LocalShifts{pool: pool} }

// Instances turns each schedule of [from, to] into instants.
//
// ⚠ This is the core time logic. A schedule's work_date is the business date the shift STARTS
// on, in the timezone of the project the person serves in that date. StartsAt is
// work_date + start in that zone; when the end is not after the start the shift is overnight
// and ends on work_date + 1. A day with projects of two timezones is
// SCHEDULE_TIMEZONE_AMBIGUOUS (writes refuse it, but a membership added later can still cause
// it, so it is checked here too).
func (s *LocalShifts) Instances(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]ShiftInstance, error) {
	var out []ShiftInstance
	err := database.WithTenant(ctx, s.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListSchedulesOfPersonnelBetween(ctx, db.ListSchedulesOfPersonnelBetweenParams{
			PersonnelID: personnelID, DateFrom: dateParam(from), DateTo: dateParam(to),
		})
		if err != nil {
			return fmt.Errorf("list schedules: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		zones, err := zonesBetween(ctx, q, personnelID, from, to)
		if err != nil {
			return err
		}
		for _, r := range rows {
			day := dateValue(r.WorkDate)
			z := zones[day]
			inst := ShiftInstance{
				ScheduleID: r.ID, WorkDate: day, ShiftID: r.ShiftID, ShiftName: r.ShiftName, IsOff: r.IsOff,
				ToleranceMinutes: int(r.ToleranceMinutes), Location: z.loc, ProjectCodes: z.projects,
			}
			if !r.IsOff {
				inst.StartsAt = timeutil.At(day, int(r.StartMinute), z.loc)
				endDay := day
				if r.EndMinute <= r.StartMinute {
					endDay = day.AddDays(1)
				}
				inst.EndsAt = timeutil.At(endDay, int(r.EndMinute), z.loc)
			}
			out = append(out, inst)
		}
		return nil
	})
	return out, err
}

// Current returns the working shifts whose window [StartsAt - 2h, EndsAt + 4h] contains at.
//
// It looks at the work dates of yesterday, today and tomorrow around at: yesterday so that an
// overnight shift can still be checked out after midnight, tomorrow so that an early check-in
// before midnight finds a shift starting just after it (and because "today" differs between
// the three zones).
func (s *LocalShifts) Current(ctx context.Context, organizationID, personnelID int64, at time.Time) ([]ShiftInstance, error) {
	today := earliestToday(at)
	instances, err := s.Instances(ctx, organizationID, personnelID, today.AddDays(-1), today.AddDays(1))
	if err != nil {
		return nil, err
	}
	var out []ShiftInstance
	for _, inst := range instances {
		if inst.IsOff {
			continue
		}
		if !at.Before(inst.StartsAt.Add(-checkInEarly)) && !at.After(inst.EndsAt.Add(checkOutLate)) {
			out = append(out, inst)
		}
	}
	return out, nil
}
