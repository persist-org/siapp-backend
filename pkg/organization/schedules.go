package organization

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Schedule types of a person's assignment (PJ48 shift_groub_type).
const (
	ScheduleFixed    = "FIXED"
	ScheduleFlexible = "FLEXIBLE"
)

const (
	// ScheduleHorizonDays is how far ahead a FIXED schedule is generated. PJ48 generates to the
	// contract end, which SiAPP does not know (Personalia owns it), so SiAPP keeps a rolling
	// window instead and the nightly job moves it forward one day at a time.
	ScheduleHorizonDays = 60
	// maxScheduleEntries bounds one manual request, and maxScheduleRangeDays one list.
	maxScheduleEntries   = 62
	maxScheduleRangeDays = 62
	maxBulkDelete        = 500
)

// TaskExtendSchedules is the worker task that runs ScheduleExtender.Extend.
const TaskExtendSchedules = "organization:extend-schedules"

// ExtendSchedulesSchedule runs it daily at 17:05 UTC: 00:05 in WIB, so the new day is there
// before anyone's shift of it.
const ExtendSchedulesSchedule = "5 17 * * *"

// Assignment is how a person's schedule is made (PJ48 m_employee_work.shift_groub*).
type Assignment struct {
	PersonnelID    int64
	ScheduleType   string
	ShiftGroupID   *int64
	EffectiveDate  timeutil.Date
	GeneratedUntil *timeutil.Date
	UpdatedAt      time.Time
}

// AssignmentInput is a request to set a person's assignment.
type AssignmentInput struct {
	ScheduleType  string        `json:"schedule_type" validate:"required,oneof=FIXED FLEXIBLE"`
	ShiftGroupID  *int64        `json:"shift_group_id" validate:"omitempty,gte=1"`
	EffectiveDate timeutil.Date `json:"effective_date"`
}

// AssignmentResult is an assignment and what setting it did to the schedule.
type AssignmentResult struct {
	Assignment Assignment
	// Deleted and Generated count the schedule rows removed and made. From is the first date
	// that could change: today in the person's timezone, or tomorrow when today's shift has
	// already started. Past dates are never changed.
	Deleted   int64
	Generated int64
	From      timeutil.Date
}

// ScheduleEntry is one date of a manual schedule.
type ScheduleEntry struct {
	WorkDate timeutil.Date `json:"work_date"`
	ShiftID  int64         `json:"shift_id" validate:"required,gte=1"`
}

// ScheduleEntries is a request to schedule a person on some dates by hand.
type ScheduleEntries struct {
	Entries []ScheduleEntry `json:"entries" validate:"required,min=1,max=62,dive"`
}

// ScheduleUpdate is a request to move a schedule to another date or give it another shift.
type ScheduleUpdate struct {
	WorkDate *timeutil.Date `json:"work_date"`
	ShiftID  *int64         `json:"shift_id" validate:"omitempty,gte=1"`
}

// BulkDelete is a request to delete several schedules at once (PJ48 deleteAll).
type BulkDelete struct {
	IDs []int64 `json:"ids" validate:"required,min=1,max=500,dive,gte=1"`
}

// ScheduleFilter narrows a list. From and To are required.
type ScheduleFilter struct {
	From        timeutil.Date
	To          timeutil.Date
	PersonnelID int64
	TeamCode    string
	Q           string
	Limit       int
	Offset      int
}

// ScheduleRow is one schedule with its person and shift.
type ScheduleRow struct {
	ID           int64
	PersonnelID  int64
	FullName     string
	NIK          string
	WorkDate     timeutil.Date
	Shift        Shift
	ShiftGroupID *int64
	CreatedBy    *int64
	UpdatedBy    *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Schedules manages each person's assignment and schedule (PJ48 ScheduleController,
// ShiftGroubController and ScheduleGeneratorService).
//
// A REGION_ADMIN reaches a person through the regu they serve in: for a schedule, a
// membership in a regu of their region in force on the schedule's date; for the assignment
// (which has no date), one that has not ended yet.
type Schedules struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewSchedules returns the Schedules usecase.
func NewSchedules(pool *pgxpool.Pool, c clock.Clock) *Schedules {
	return &Schedules{pool: pool, clock: c}
}

// GetAssignment returns a person's assignment.
func (s *Schedules) GetAssignment(ctx context.Context, actor auth.Principal, scope access.AdminScope, personnelID int64) (Assignment, error) {
	var out Assignment
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := s.reachPerson(ctx, q, scope, personnelID); err != nil {
			return err
		}
		row, err := q.GetShiftAssignment(ctx, personnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShiftAssignmentNotFound
		}
		if err != nil {
			return fmt.Errorf("get shift assignment: %w", err)
		}
		out = toAssignment(row)
		return nil
	})
	return out, err
}

// SetAssignment sets how a person's schedule is made, in one transaction.
//
// FIXED: the schedule is generated from the group's weekly pattern from effective_date on, up
// to today + ScheduleHorizonDays. Like PJ48's ScheduleGeneratorService it first deletes what
// would be replaced, from the earliest of the new and the old effective date, but never a past
// date, and never today once today's shift has started: those days are history (attendance
// will hang on them). Dates entered by hand in that range are replaced too, as in PJ48; those
// after the horizon were never generated and are left alone.
//
// FLEXIBLE: nothing is generated and nothing is deleted; the admin enters each date.
func (s *Schedules) SetAssignment(ctx context.Context, actor auth.Principal, scope access.AdminScope, personnelID int64, in AssignmentInput) (AssignmentResult, error) {
	if in.EffectiveDate.IsZero() {
		return AssignmentResult{}, validationError("effective_date", "Wajib diisi dengan format YYYY-MM-DD.")
	}
	switch {
	case in.ScheduleType == ScheduleFixed && in.ShiftGroupID == nil:
		return AssignmentResult{}, validationError("shift_group_id", "Wajib diisi untuk jadwal FIXED.")
	case in.ScheduleType == ScheduleFlexible && in.ShiftGroupID != nil:
		return AssignmentResult{}, validationError("shift_group_id", "Harus kosong untuk jadwal FLEXIBLE.")
	}
	now := s.clock.Now()
	var out AssignmentResult
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// ⚠ The person's row is locked first: a first assignment has no row of its own to lock
		// yet, and two at once would otherwise both delete and generate, leaving the schedule
		// of one group under the other's assignment. FOR NO KEY UPDATE, not FOR UPDATE: the
		// nightly extension holds an assignment row while its schedule inserts take the
		// person's KEY SHARE lock (the foreign key), and FOR UPDATE would deadlock with that.
		if _, err := q.LockPersonnelForSchedule(ctx, personnelID); errors.Is(err, pgx.ErrNoRows) {
			return errNoSuchPersonnel
		} else if err != nil {
			return fmt.Errorf("lock personnel: %w", err)
		}
		if err := s.reachPerson(ctx, q, scope, personnelID); err != nil {
			return err
		}
		// Only FIXED generates anything. FLEXIBLE stays possible for someone who has left, so
		// their group can be freed (and deleted).
		if in.ScheduleType == ScheduleFixed {
			if err := mustBeActive(ctx, q, personnelID); err != nil {
				return err
			}
		}
		previous, err := q.GetShiftAssignmentForUpdate(ctx, personnelID)
		hadPrevious := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get shift assignment: %w", err)
		}
		var group *ShiftGroup
		if in.ShiftGroupID != nil {
			// FOR SHARE: a delete of the group in flight is waited for and then seen.
			g, err := loadGroup(ctx, q, *in.ShiftGroupID, lockShare)
			if err != nil {
				return err
			}
			group = &g
		}
		guard, horizonEnd, err := s.guardStart(ctx, q, personnelID, now, group)
		if err != nil {
			return err
		}
		out.From = guard

		var generatedUntil pgtype.Date
		if in.ScheduleType == ScheduleFixed {
			deleteFrom := in.EffectiveDate
			if hadPrevious && previous.ScheduleType == db.ScheduleTypeFIXED && dateValue(previous.EffectiveDate).Before(deleteFrom) {
				deleteFrom = dateValue(previous.EffectiveDate)
			}
			deleteFrom = laterDate(deleteFrom, guard)
			// ⚠ The delete and the generation below reach every date from deleteFrom to the
			// horizon, including manual entries, so a REGION_ADMIN needs the person in their
			// region on each of them (as for one entry): someone moving in from another region
			// has days that are not theirs.
			if err := s.reachRange(ctx, q, scope, personnelID, deleteFrom, horizonEnd); err != nil {
				return err
			}
			// Every date about to be generated must have one timezone, as a manual entry must.
			if genFrom := laterDate(in.EffectiveDate, guard); !horizonEnd.Before(genFrom) {
				zones, err := zonesBetween(ctx, q, personnelID, genFrom, horizonEnd)
				if err != nil {
					return err
				}
				for d := genFrom; !horizonEnd.Before(d); d = d.AddDays(1) {
					if zones[d].ambiguous != nil {
						return zones[d].ambiguous
					}
				}
			}
			if out.Deleted, err = q.DeleteSchedulesOfPersonnelBetween(ctx, db.DeleteSchedulesOfPersonnelBetweenParams{PersonnelID: personnelID, FromDate: dateParam(deleteFrom), ToDate: dateParam(horizonEnd)}); err != nil {
				return fmt.Errorf("delete schedules: %w", err)
			}
			if out.Generated, err = generate(ctx, q, personnelID, *in.ShiftGroupID, in.EffectiveDate, laterDate(in.EffectiveDate, guard), horizonEnd, personnelRef(actor), now); err != nil {
				return err
			}
			generatedUntil = dateParam(horizonEnd)
		}
		row, err := q.UpsertShiftAssignment(ctx, db.UpsertShiftAssignmentParams{
			PersonnelID: personnelID, ScheduleType: db.ScheduleType(in.ScheduleType), ShiftGroupID: int8OrNull(in.ShiftGroupID),
			EffectiveDate: dateParam(in.EffectiveDate), GeneratedUntil: generatedUntil, Actor: personnelRef(actor), Now: ts(now),
		})
		if err != nil {
			return fmt.Errorf("save shift assignment: %w", err)
		}
		out.Assignment = toAssignment(row)
		meta := map[string]any{
			"personnel_id": personnelID, "schedule_type": in.ScheduleType, "shift_group_id": in.ShiftGroupID,
			"effective_date": in.EffectiveDate.String(), "from": guard.String(), "deleted": out.Deleted, "generated": out.Generated,
		}
		var before map[string]any
		if hadPrevious {
			p := toAssignment(previous)
			before = map[string]any{"schedule_type": p.ScheduleType, "shift_group_id": p.ShiftGroupID, "effective_date": p.EffectiveDate.String()}
		}
		return recordIDAudit(ctx, q, actor, "shift_assignment.set", "personnel", personnelID, before, meta)
	})
	return out, err
}

// List returns one page of schedules of a date range (at most 62 days), by date then name.
func (s *Schedules) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ScheduleFilter) ([]ScheduleRow, int, error) {
	if f.From.IsZero() || f.To.IsZero() {
		return nil, 0, validationError("date_from", "date_from dan date_to wajib diisi (YYYY-MM-DD).")
	}
	if f.To.Before(f.From) {
		return nil, 0, validationError("date_to", "Tidak boleh sebelum date_from.")
	}
	if f.From.AddDays(maxScheduleRangeDays - 1).Before(f.To) {
		return nil, 0, validationError("date_to", fmt.Sprintf("Rentang paling lama %d hari.", maxScheduleRangeDays))
	}
	if f.TeamCode != "" && !scope.AllowsRegion(RegionOfCode(f.TeamCode)) {
		return nil, 0, access.ErrOutsideAdminScope
	}
	var out []ScheduleRow
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		var personnel pgtype.Int8
		if f.PersonnelID > 0 {
			personnel = pgtype.Int8{Int64: f.PersonnelID, Valid: true}
		}
		rows, err := q.ListSchedules(ctx, db.ListSchedulesParams{
			DateFrom: dateParam(f.From), DateTo: dateParam(f.To), PersonnelID: personnel, Q: textOrNull(f.Q),
			TeamCode: textOrNull(f.TeamCode), ScopeRegion: scopeFilter(scope),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list schedules: %w", err)
		}
		for _, r := range rows {
			out = append(out, toScheduleRow(db.GetScheduleViewRow(r)))
		}
		total, err = q.CountSchedules(ctx, db.CountSchedulesParams{
			DateFrom: dateParam(f.From), DateTo: dateParam(f.To), PersonnelID: personnel, Q: textOrNull(f.Q),
			TeamCode: textOrNull(f.TeamCode), ScopeRegion: scopeFilter(scope),
		})
		if err != nil {
			return fmt.Errorf("count schedules: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one schedule.
func (s *Schedules) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (ScheduleRow, error) {
	var out ScheduleRow
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetScheduleView(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduleNotFound
		}
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		out = toScheduleRow(row)
		return s.reachPersonOn(ctx, q, scope, out.PersonnelID, out.WorkDate)
	})
	return out, err
}

// Create schedules a person by hand on some dates (PJ48 ScheduleController::store). Like PJ48
// it refuses a date that already has a schedule (change that one instead), and a date twice in
// one request. All dates or none are saved.
func (s *Schedules) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, personnelID int64, in ScheduleEntries) ([]ScheduleRow, error) {
	seen := map[timeutil.Date]bool{}
	for i, e := range in.Entries {
		field := fmt.Sprintf("entries[%d].work_date", i)
		if e.WorkDate.IsZero() {
			return nil, validationError(field, "Wajib diisi dengan format YYYY-MM-DD.")
		}
		if seen[e.WorkDate] {
			return nil, validationError(field, "Tanggal "+e.WorkDate.String()+" muncul dua kali dalam permintaan.")
		}
		seen[e.WorkDate] = true
	}
	now := s.clock.Now()
	var out []ScheduleRow
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// Scope first: a REGION_ADMIN learns nothing (exists? active?) about people outside it.
		for _, e := range in.Entries {
			if err := s.reachPersonOn(ctx, q, scope, personnelID, e.WorkDate); err != nil {
				return err
			}
		}
		if err := mustBeActive(ctx, q, personnelID); err != nil {
			return err
		}
		shifts, err := entryShifts(ctx, q, in.Entries)
		if err != nil {
			return err
		}
		for i, e := range in.Entries {
			if z, err := zoneOn(ctx, q, personnelID, e.WorkDate); err != nil {
				return err
			} else if z.ambiguous != nil {
				return z.ambiguous
			}
			id, err := q.InsertSchedule(ctx, db.InsertScheduleParams{
				PersonnelID: personnelID, WorkDate: dateParam(e.WorkDate), ShiftID: e.ShiftID, Actor: personnelRef(actor), Now: ts(now),
			})
			if database.IsUniqueViolation(err) {
				return ErrScheduleExists.WithMessage("Personil sudah memiliki jadwal pada " + e.WorkDate.String() + ". Ubah jadwal tanggal tersebut.")
			}
			if err != nil {
				return fmt.Errorf("insert schedule: %w", err)
			}
			if err := recordIDAudit(ctx, q, actor, "schedule.create", "schedule", id, nil, map[string]any{
				"personnel_id": personnelID, "work_date": e.WorkDate.String(), "shift_id": shifts[i].ID,
			}); err != nil {
				return err
			}
			row, err := q.GetScheduleView(ctx, id)
			if err != nil {
				return fmt.Errorf("get schedule: %w", err)
			}
			out = append(out, toScheduleRow(row))
		}
		return nil
	})
	if err != nil {
		return nil, wrapUnexpected(err, "create schedules")
	}
	return out, nil
}

// Update moves a schedule to another date and/or gives it another shift (PJ48
// ScheduleController::update). A date that already has a schedule is SCHEDULE_EXISTS. The row
// then counts as entered by hand (its shift_group_id is cleared). Attendance already recorded
// against it is re-checked from Phase 07 on.
func (s *Schedules) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ScheduleUpdate) (ScheduleRow, error) {
	if in.WorkDate != nil && in.WorkDate.IsZero() {
		return ScheduleRow{}, validationError("work_date", "Harus berformat YYYY-MM-DD.")
	}
	now := s.clock.Now()
	var out ScheduleRow
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetScheduleForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduleNotFound
		}
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		oldDate := dateValue(row.WorkDate)
		if err := s.reachPersonOn(ctx, q, scope, row.PersonnelID, oldDate); err != nil {
			return err
		}
		newDate, shiftID := oldDate, row.ShiftID
		if in.WorkDate != nil {
			newDate = *in.WorkDate
		}
		if in.ShiftID != nil {
			shiftID = *in.ShiftID
		}
		if newDate == oldDate && shiftID == row.ShiftID {
			// Nothing changes: the row stays the group's (shift_group_id is kept) and no audit
			// row says it was changed.
			view, err := q.GetScheduleView(ctx, id)
			if err != nil {
				return fmt.Errorf("get schedule: %w", err)
			}
			out = toScheduleRow(view)
			return nil
		}
		// Only a NEW shift must be active: moving a date keeps the shift it already has, even
		// one deactivated since.
		if shiftID != row.ShiftID {
			if _, err := entryShifts(ctx, q, []ScheduleEntry{{ShiftID: shiftID}}); err != nil {
				return relabel(err, "entries[0].shift_id", "shift_id")
			}
		} else if newDate != oldDate && !newDate.Before(earliestToday(now)) {
			// ...but not one that was deleted since: moved to today or later, the row would be
			// a coming shift nobody can see (a deleted shift only stays on past dates).
			if _, err := q.GetShift(ctx, shiftID); errors.Is(err, pgx.ErrNoRows) {
				return validationError("shift_id", "Shift jadwal ini sudah dihapus. Pilih shift lain untuk tanggal ini.")
			} else if err != nil {
				return fmt.Errorf("get shift: %w", err)
			}
		}
		if newDate != oldDate {
			if err := s.reachPersonOn(ctx, q, scope, row.PersonnelID, newDate); err != nil {
				return err
			}
			if z, err := zoneOn(ctx, q, row.PersonnelID, newDate); err != nil {
				return err
			} else if z.ambiguous != nil {
				return z.ambiguous
			}
		}
		err = q.UpdateSchedule(ctx, db.UpdateScheduleParams{ID: id, WorkDate: dateParam(newDate), ShiftID: shiftID, Actor: personnelRef(actor), Now: ts(now)})
		if database.IsUniqueViolation(err) {
			return ErrScheduleExists.WithMessage("Personil sudah memiliki jadwal pada " + newDate.String() + ". Ubah jadwal tanggal tersebut.")
		}
		if err != nil {
			return fmt.Errorf("update schedule: %w", err)
		}
		view, err := q.GetScheduleView(ctx, id)
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		out = toScheduleRow(view)
		return recordIDAudit(ctx, q, actor, "schedule.update", "schedule", id,
			map[string]any{"personnel_id": row.PersonnelID, "work_date": oldDate.String(), "shift_id": row.ShiftID},
			map[string]any{"personnel_id": row.PersonnelID, "work_date": newDate.String(), "shift_id": shiftID})
	})
	if err != nil {
		return ScheduleRow{}, wrapUnexpected(err, "update schedule")
	}
	return out, nil
}

// Delete removes one schedule.
func (s *Schedules) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		return s.deleteOne(ctx, q, actor, scope, id)
	})
}

// DeleteMany removes several schedules (PJ48 deleteAll), all or none: one that does not exist
// or is out of reach stops the whole request.
func (s *Schedules) DeleteMany(ctx context.Context, actor auth.Principal, scope access.AdminScope, in BulkDelete) (int, error) {
	ids := slices.Compact(slices.Sorted(slices.Values(in.IDs)))
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		for _, id := range ids {
			if err := s.deleteOne(ctx, q, actor, scope, id); err != nil {
				if errors.Is(err, ErrScheduleNotFound) {
					return ErrScheduleNotFound.WithMessage(fmt.Sprintf("Jadwal %d tidak ditemukan.", id))
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (s *Schedules) deleteOne(ctx context.Context, q *db.Queries, actor auth.Principal, scope access.AdminScope, id int64) error {
	row, err := q.GetScheduleForUpdate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrScheduleNotFound
	}
	if err != nil {
		return fmt.Errorf("get schedule: %w", err)
	}
	if err := s.reachPersonOn(ctx, q, scope, row.PersonnelID, dateValue(row.WorkDate)); err != nil {
		return err
	}
	if _, err := q.DeleteSchedule(ctx, id); err != nil {
		return fmt.Errorf("delete schedule: %w", err)
	}
	return recordIDAudit(ctx, q, actor, "schedule.delete", "schedule", id,
		map[string]any{"personnel_id": row.PersonnelID, "work_date": dateValue(row.WorkDate).String(), "shift_id": row.ShiftID}, nil)
}

// reachPerson checks that the person exists and, for a REGION_ADMIN, serves (or will serve)
// in a regu of their region.
func (s *Schedules) reachPerson(ctx context.Context, q *db.Queries, scope access.AdminScope, personnelID int64) error {
	if _, err := q.GetPersonnel(ctx, personnelID); errors.Is(err, pgx.ErrNoRows) {
		return errNoSuchPersonnel
	} else if err != nil {
		return fmt.Errorf("get personnel: %w", err)
	}
	if scope.All {
		return nil
	}
	ok, err := q.PersonnelInRegionFromNow(ctx, db.PersonnelInRegionFromNowParams{PersonnelID: personnelID, RegionCode: scope.RegionCode, Now: ts(s.clock.Now())})
	if err != nil {
		return fmt.Errorf("check region of personnel: %w", err)
	}
	if !ok {
		return access.ErrOutsideAdminScope
	}
	return nil
}

// mustBeActive checks that the person exists and is active: someone who has left gets no new
// assignment or schedule.
func mustBeActive(ctx context.Context, q *db.Queries, personnelID int64) error {
	person, err := q.GetPersonnel(ctx, personnelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNoSuchPersonnel
	}
	if err != nil {
		return fmt.Errorf("get personnel: %w", err)
	}
	if person.Status == db.PersonnelStatusINACTIVE {
		return errInactiveNotScheduled
	}
	return nil
}

// reachPersonOn checks, for a REGION_ADMIN, that the person serves in a regu of their region
// on the date.
func (s *Schedules) reachPersonOn(ctx context.Context, q *db.Queries, scope access.AdminScope, personnelID int64, on timeutil.Date) error {
	if scope.All {
		return nil
	}
	ok, err := q.PersonnelInRegionOn(ctx, db.PersonnelInRegionOnParams{PersonnelID: personnelID, RegionCode: scope.RegionCode, OnDate: dateParam(on)})
	if err != nil {
		return fmt.Errorf("check region of personnel: %w", err)
	}
	if !ok {
		return access.ErrOutsideAdminScope
	}
	return nil
}

// reachRange checks, for a REGION_ADMIN, that the person serves in a regu of their region on
// every date of [from, to]. An empty range passes.
func (s *Schedules) reachRange(ctx context.Context, q *db.Queries, scope access.AdminScope, personnelID int64, from, to timeutil.Date) error {
	if scope.All || to.Before(from) {
		return nil
	}
	_, err := q.FirstDateOutsideRegion(ctx, db.FirstDateOutsideRegionParams{
		FromDate: dateParam(from), ToDate: dateParam(to), PersonnelID: personnelID, RegionCode: scope.RegionCode,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no date outside the region
	}
	if err != nil {
		return fmt.Errorf("check region of personnel over range: %w", err)
	}
	return access.ErrOutsideAdminScope
}

// guardStart is the first date a FIXED (re)generation may change, and the end of the window it
// fills (today + ScheduleHorizonDays), both in the person's timezone.
//
// ⚠ This is PJ48's getUpdateGuardStart: past dates are never touched, and today is locked once
// its shift has started, because attendance hangs on it. (PJ48 also locks today once the
// person has checked in; SiAPP's attendance arrives in Phase 07 and adds that check here.)
// Today is also left alone when the new group's shift of today has already started: the
// person would otherwise get a shift that began hours ago, and be late for it.
func (s *Schedules) guardStart(ctx context.Context, q *db.Queries, personnelID int64, now time.Time, group *ShiftGroup) (timeutil.Date, timeutil.Date, error) {
	loc, err := currentZone(ctx, q, personnelID, now)
	if err != nil {
		return timeutil.Date{}, timeutil.Date{}, err
	}
	today := timeutil.BusinessDate(now, loc)
	guard := today
	started := func(isOff bool, startMinute int) bool {
		return !isOff && !now.Before(timeutil.At(today, startMinute, loc))
	}
	rows, err := q.ListSchedulesOfPersonnelBetween(ctx, db.ListSchedulesOfPersonnelBetweenParams{PersonnelID: personnelID, DateFrom: dateParam(today), DateTo: dateParam(today)})
	if err != nil {
		return timeutil.Date{}, timeutil.Date{}, fmt.Errorf("get today's schedule: %w", err)
	}
	for _, r := range rows {
		if started(r.IsOff, int(r.StartMinute)) {
			guard = today.AddDays(1)
		}
	}
	if group != nil {
		weekday := int(today.Time().Weekday()+6)%7 + 1 // ISO: Monday 1 .. Sunday 7
		for _, d := range group.Days {
			if d.Weekday == weekday && started(d.Shift.IsOff, d.Shift.StartMinute) {
				guard = today.AddDays(1)
			}
		}
	}
	return guard, today.AddDays(ScheduleHorizonDays), nil
}

// generate fills [from, to] from the group's pattern, never before effective. Dates that
// already have a schedule keep it.
func generate(ctx context.Context, q *db.Queries, personnelID, groupID int64, effective, from, to timeutil.Date, actor pgtype.Int8, now time.Time) (int64, error) {
	from = laterDate(from, effective)
	if to.Before(from) {
		return 0, nil
	}
	n, err := q.GenerateSchedules(ctx, db.GenerateSchedulesParams{
		PersonnelID: personnelID, ShiftGroupID: groupID, FromDate: dateParam(from), ToDate: dateParam(to), Actor: actor, Now: ts(now),
	})
	if err != nil {
		return 0, fmt.Errorf("generate schedules: %w", err)
	}
	return n, nil
}

// entryShifts returns the shift of each entry, refusing one that does not exist or is not
// active.
func entryShifts(ctx context.Context, q *db.Queries, entries []ScheduleEntry) ([]Shift, error) {
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ShiftID)
	}
	// FOR SHARE: a deactivation or delete of one of them in flight is waited for and then seen.
	rows, err := q.GetShiftsByIDsForShare(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("get shifts: %w", err)
	}
	byID := map[int64]Shift{}
	for _, r := range rows {
		byID[r.ID] = toShift(r)
	}
	out := make([]Shift, 0, len(entries))
	for i, e := range entries {
		shift, ok := byID[e.ShiftID]
		field := fmt.Sprintf("entries[%d].shift_id", i)
		if !ok {
			return nil, validationError(field, "Shift tidak ditemukan.")
		}
		if !shift.Active {
			return nil, validationError(field, "Shift tidak aktif.")
		}
		out = append(out, shift)
	}
	return out, nil
}

// zone is where a person works on a date: the timezone, and the projects of their regu.
type zone struct {
	loc      *time.Location
	projects []string
	// ambiguous is SCHEDULE_TIMEZONE_AMBIGUOUS when the projects of that date are in different
	// timezones (loc is then the first one's). Only a caller that puts a working shift on the
	// date has to refuse it: an OFF day, or a date nothing is written to, has no instant.
	ambiguous *apperr.Error
}

// zonesBetween returns the zone of each date of [from, to] from the person's memberships. A
// date without a membership is in the group's default timezone (WIB) with no project. Two
// projects with different timezones on one date mark it ambiguous: the local shift times
// would mean two different instants.
func zonesBetween(ctx context.Context, q *db.Queries, personnelID int64, from, to timeutil.Date) (map[timeutil.Date]zone, error) {
	rows, err := q.ListMembershipZonesOfPersonnelBetween(ctx, db.ListMembershipZonesOfPersonnelBetweenParams{
		PersonnelID: personnelID, DateFrom: dateParam(from), DateTo: dateParam(to),
	})
	if err != nil {
		return nil, fmt.Errorf("list membership zones: %w", err)
	}
	fallback, err := time.LoadLocation(timeutil.SupportedTimezones[0])
	if err != nil {
		return nil, fmt.Errorf("load default timezone: %w", err)
	}
	out := map[timeutil.Date]zone{}
	for d := from; !to.Before(d); d = d.AddDays(1) {
		var tz string
		var projects []string
		var ambiguous *apperr.Error
		for _, r := range rows {
			if dateValue(r.StartDate).After(d) || (r.EndDate.Valid && dateValue(r.EndDate).Before(d)) {
				continue
			}
			if tz == "" {
				tz = r.Timezone
			} else if r.Timezone != tz && ambiguous == nil {
				ambiguous = ErrScheduleTimezoneAmbiguous.WithMessage("Pada " + d.String() + " personil bertugas di proyek dengan zona waktu berbeda (" + tz + " dan " + r.Timezone + ").")
			}
			if !slices.Contains(projects, r.ProjectCode) {
				projects = append(projects, r.ProjectCode)
			}
		}
		z := zone{loc: fallback, projects: projects, ambiguous: ambiguous}
		if tz != "" {
			if z.loc, err = time.LoadLocation(tz); err != nil {
				return nil, fmt.Errorf("load timezone %q: %w", tz, err)
			}
		}
		out[d] = z
	}
	return out, nil
}

func zoneOn(ctx context.Context, q *db.Queries, personnelID int64, on timeutil.Date) (zone, error) {
	zones, err := zonesBetween(ctx, q, personnelID, on, on)
	if err != nil {
		return zone{}, err
	}
	return zones[on], nil
}

// currentZone is the timezone a person works in now. The calendar date differs by at most a
// day between the three zones, so the memberships of yesterday..tomorrow in WIB decide it.
func currentZone(ctx context.Context, q *db.Queries, personnelID int64, now time.Time) (*time.Location, error) {
	wibToday := earliestToday(now)
	zones, err := zonesBetween(ctx, q, personnelID, wibToday, wibToday.AddDays(1))
	if err != nil {
		return nil, err
	}
	z := zones[wibToday]
	// Already tomorrow there? Then tomorrow's membership is the one in force.
	if next := zones[wibToday.AddDays(1)]; timeutil.BusinessDate(now, next.loc).After(wibToday) && len(next.projects) > 0 {
		z = next
	}
	return z.loc, nil
}

func laterDate(a, b timeutil.Date) timeutil.Date {
	if a.Before(b) {
		return b
	}
	return a
}

func toAssignment(r db.PersonnelShiftAssignment) Assignment {
	a := Assignment{
		PersonnelID: r.PersonnelID, ScheduleType: string(r.ScheduleType), ShiftGroupID: int64Value(r.ShiftGroupID),
		EffectiveDate: dateValue(r.EffectiveDate), UpdatedAt: r.UpdatedAt.Time,
	}
	if r.GeneratedUntil.Valid {
		d := dateValue(r.GeneratedUntil)
		a.GeneratedUntil = &d
	}
	return a
}

func toScheduleRow(r db.GetScheduleViewRow) ScheduleRow {
	out := ScheduleRow{
		ID: r.ID, PersonnelID: r.PersonnelID, FullName: r.FullName, WorkDate: dateValue(r.WorkDate),
		Shift:        Shift{ID: r.ShiftID, Name: r.ShiftName, StartMinute: int(r.StartMinute), EndMinute: int(r.EndMinute), IsOff: r.IsOff},
		ShiftGroupID: int64Value(r.ShiftGroupID), CreatedBy: int64Value(r.CreatedBy), UpdatedBy: int64Value(r.UpdatedBy),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.Nik.Valid {
		out.NIK = r.Nik.String
	}
	return out
}

// relabel renames the field of a single-field validation error (an entry check reused for a
// patch).
func relabel(err error, from, to string) error {
	var appErr *apperr.Error
	if errors.As(err, &appErr) && len(appErr.Fields) == 1 && appErr.Fields[0].Field == from {
		return validationError(to, appErr.Fields[0].Message)
	}
	return err
}

// ScheduleExtender is the nightly job that keeps every FIXED schedule generated up to
// today + ScheduleHorizonDays (PJ48's schedule:generate-due, for a rolling window).
type ScheduleExtender struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewScheduleExtender returns a ScheduleExtender.
func NewScheduleExtender(pool *pgxpool.Pool, c clock.Clock) *ScheduleExtender {
	return &ScheduleExtender{pool: pool, clock: c}
}

// Extend generates, for every FIXED assignment of every organization, the dates between its
// generated_until and today + ScheduleHorizonDays. A date that already has a schedule (an
// admin's own entry) keeps it. It returns how many rows it made. A failure for one person (or
// organization) does not stop the others; the first error is returned at the end, so the
// scheduler retries.
func (e *ScheduleExtender) Extend(ctx context.Context) (int64, error) {
	var orgs []int64
	if err := database.WithoutTenant(ctx, e.pool, func(q *db.Queries) (err error) {
		orgs, err = q.ListOrganizationIDs(ctx)
		return err
	}); err != nil {
		return 0, fmt.Errorf("extend schedules: list organizations: %w", err)
	}
	now := e.clock.Now()
	// The latest "today" of the three zones is WIT's; WIB's plus one day covers it.
	target := earliestToday(now).AddDays(ScheduleHorizonDays + 1)
	// ⚠ Never today or earlier: a window left behind (someone inactive for a while, a worker
	// outage) is caught up from tomorrow on. The past is history, and today may be under way.
	floor := earliestToday(now).AddDays(1)
	var total int64
	var firstErr error
	for _, org := range orgs {
		var people []int64
		if err := database.WithTenant(ctx, e.pool, org, func(q *db.Queries) (err error) {
			people, err = q.ListAssignmentsToExtend(ctx, dateParam(target))
			return err
		}); err != nil {
			firstErr = cmp.Or(firstErr, fmt.Errorf("extend schedules of organization %d: list assignments: %w", org, err))
			continue
		}
		for _, personnelID := range people {
			made, err := e.extendOne(ctx, org, personnelID, floor, target, now)
			if err != nil {
				firstErr = cmp.Or(firstErr, fmt.Errorf("extend schedules of personnel %d in organization %d: %w", personnelID, org, err))
				continue
			}
			total += made
		}
	}
	return total, firstErr
}

// extendOne extends one person's FIXED window in a transaction of its own, so one person's
// failure does not undo the others and no lock is held across the whole run. It takes the
// locks in SetAssignment's order (the person, then the assignment) and re-reads the
// assignment, which may have changed since it was listed.
func (e *ScheduleExtender) extendOne(ctx context.Context, org, personnelID int64, floor, target timeutil.Date, now time.Time) (int64, error) {
	var made int64
	err := database.WithTenant(ctx, e.pool, org, func(q *db.Queries) error {
		person, err := q.LockPersonnelForSchedule(ctx, personnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock personnel: %w", err)
		}
		if person.Status == db.PersonnelStatusINACTIVE {
			return nil
		}
		a, err := q.GetShiftAssignmentForUpdate(ctx, personnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get shift assignment: %w", err)
		}
		if a.ScheduleType != db.ScheduleTypeFIXED || !a.ShiftGroupID.Valid || (a.GeneratedUntil.Valid && !dateValue(a.GeneratedUntil).Before(target)) {
			return nil
		}
		from := laterDate(dateValue(a.EffectiveDate), floor)
		if a.GeneratedUntil.Valid {
			from = laterDate(from, dateValue(a.GeneratedUntil).AddDays(1))
		}
		if made, err = generate(ctx, q, personnelID, a.ShiftGroupID.Int64, dateValue(a.EffectiveDate), from, target, pgtype.Int8{}, now); err != nil {
			return err
		}
		if err := q.SetGeneratedUntil(ctx, db.SetGeneratedUntilParams{PersonnelID: personnelID, GeneratedUntil: dateParam(target)}); err != nil {
			return fmt.Errorf("set generated until: %w", err)
		}
		return nil
	})
	return made, err
}

// RegisterScheduleJob adds the extension task handler to the worker's mux.
func RegisterScheduleJob(mux *asynq.ServeMux, extender *ScheduleExtender, logger *slog.Logger) {
	mux.HandleFunc(TaskExtendSchedules, func(ctx context.Context, _ *asynq.Task) error {
		made, err := extender.Extend(ctx)
		if err != nil {
			return err
		}
		logger.InfoContext(ctx, "schedule extension finished", slog.Int64("schedules", made))
		return nil
	})
}

// RegisterScheduleSchedule adds the nightly extension to the scheduler.
func RegisterScheduleSchedule(scheduler *asynq.Scheduler) error {
	if _, err := scheduler.Register(ExtendSchedulesSchedule, asynq.NewTask(TaskExtendSchedules, nil),
		asynq.MaxRetry(3), asynq.Timeout(30*time.Minute)); err != nil {
		return fmt.Errorf("register %s: %w", TaskExtendSchedules, err)
	}
	return nil
}
