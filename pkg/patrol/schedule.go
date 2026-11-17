package patrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Patrol schedules (Phase 10.11, docs/plan/10.1-patrol-update.md): an admin schedules a person to patrol a zone
// in a time window, and the guard and the DANRU read the schedule with a status.

// Statuses of a schedule. They are not stored: see scheduleStatus.
const (
	ScheduleScheduled  = "SCHEDULED"
	ScheduleInProgress = "IN_PROGRESS"
	ScheduleCompleted  = "COMPLETED"
	ScheduleMissed     = "MISSED"
	ScheduleCancelled  = "CANCELLED"
)

// The kinds of change a PATROL_SCHEDULE_CHANGED notice reports (data.change).
const (
	ScheduleChangeCreated   = "CREATED"
	ScheduleChangeUpdated   = "UPDATED"
	ScheduleChangeCancelled = "CANCELLED"
)

const (
	// maxScheduleRows is how many schedules one create request may make (people x slots).
	maxScheduleRows = 200
	// maxScheduleWindow is the longest window of one schedule.
	maxScheduleWindow = 24 * time.Hour
	// defaultScheduleDays is how far ahead the field list looks when it is given no range: today and 7 more days.
	defaultScheduleDays = 7
)

// PatrolRef is the patrol that realizes a schedule (Phase 10.12): only its status decides the schedule's.
type PatrolRef struct {
	Status string // ACTIVE, ENDED or AUTO_ENDED
}

// scheduleTimes is what the status of a schedule is read from.
type scheduleTimes struct {
	EndsAt      time.Time
	CancelledAt *time.Time
}

// scheduleStatus is the status of a schedule at a moment: CANCELLED if it was cancelled; else, when a patrol
// realizes it, IN_PROGRESS (the patrol is ACTIVE) or COMPLETED (it ENDED or was AUTO_ENDED); else MISSED when its
// window is over, else SCHEDULED.
//
// ⚠ Until Phase 10.12 no patrol can be linked to a schedule, so the callers pass nil and only SCHEDULED,
// MISSED and CANCELLED occur.
func scheduleStatus(now time.Time, sc scheduleTimes, patrol *PatrolRef) string {
	switch {
	case sc.CancelledAt != nil:
		return ScheduleCancelled
	case patrol != nil && patrol.Status == "ACTIVE":
		return ScheduleInProgress
	case patrol != nil:
		return ScheduleCompleted
	case now.After(sc.EndsAt):
		return ScheduleMissed
	}
	return ScheduleScheduled
}

// ScheduleSlot is one window of a create request. A time with no offset is not a time: encoding/json refuses it.
type ScheduleSlot struct {
	StartsAt time.Time `json:"starts_at" validate:"required"`
	EndsAt   time.Time `json:"ends_at" validate:"required"`
}

// ScheduleInput is the body of POST /admin/patrol-schedules: one row per person and slot.
type ScheduleInput struct {
	ZoneID       int64          `json:"zone_id" validate:"required,min=1"`
	PersonnelIDs []int64        `json:"personnel_ids" validate:"required,min=1,max=200,dive,min=1"`
	Slots        []ScheduleSlot `json:"slots" validate:"required,min=1,max=200,dive"`
	Notes        *string        `json:"notes" validate:"omitempty,max=2000"`
}

// ScheduleUpdate is the body of PATCH /admin/patrol-schedules/{id}. A field left out keeps its value. The zone
// cannot change: cancel the schedule and make another.
type ScheduleUpdate struct {
	PersonnelID *int64           `json:"personnel_id" validate:"omitempty,min=1"`
	StartsAt    *time.Time       `json:"starts_at"`
	EndsAt      *time.Time       `json:"ends_at"`
	Notes       optional[string] `json:"notes"`
}

// ScheduleFilter narrows the lists. DateFrom and DateTo filter on work_date.
type ScheduleFilter struct {
	ProjectCode string
	ZoneID      int64
	PersonnelID int64
	DateFrom    *timeutil.Date
	DateTo      *timeutil.Date
	Status      string
	Limit       int
	Offset      int
}

// Schedule is a patrol schedule as the API shows it.
type Schedule struct {
	ID            string // uuid
	ProjectCode   string
	ZoneID        int64
	ZoneName      string
	ZoneType      string
	PersonnelID   int64
	PersonnelName string
	StartsAt      time.Time
	EndsAt        time.Time
	WorkDate      timeutil.Date
	Notes         *string
	Status        string
	CancelledAt   *time.Time
}

func scheduleOf(now time.Time, sc db.PatrolSchedule, zoneName string, zoneType db.PatrolZoneType, personnelName string) Schedule {
	var cancelled *time.Time
	if sc.CancelledAt.Valid {
		t := sc.CancelledAt.Time
		cancelled = &t
	}
	return Schedule{
		ID: uuidString(sc.ID), ProjectCode: sc.ProjectCode, ZoneID: sc.ZoneID, ZoneName: zoneName, ZoneType: string(zoneType),
		PersonnelID: sc.PersonnelID, PersonnelName: personnelName, StartsAt: sc.StartsAt.Time, EndsAt: sc.EndsAt.Time,
		WorkDate: dateOf(sc.WorkDate), Notes: textPtr(sc.Notes), CancelledAt: cancelled,
		Status: scheduleStatus(now, scheduleTimes{EndsAt: sc.EndsAt.Time, CancelledAt: cancelled}, nil),
	}
}

func scheduleOfRow(now time.Time, r db.ListPatrolSchedulesRow) Schedule {
	sc := db.PatrolSchedule{
		ID: r.ID, OrganizationID: r.OrganizationID, ProjectCode: r.ProjectCode, ZoneID: r.ZoneID, PersonnelID: r.PersonnelID,
		StartsAt: r.StartsAt, EndsAt: r.EndsAt, WorkDate: r.WorkDate, Notes: r.Notes, CreatedByID: r.CreatedByID,
		CancelledAt: r.CancelledAt, CancelledByID: r.CancelledByID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	return scheduleOf(now, sc, r.ZoneName, r.ZoneType, r.PersonnelName)
}

// lockSchedulingOf makes the schedule changes of some people take turns. Two admins scheduling the same person at
// once must not both read "no overlap" before either writes. It is an advisory lock, and not an exclusion
// constraint, because that would need the btree_gist extension, which the database does not have. People are locked
// in id order, so two requests that share two of them cannot wait for each other.
func lockSchedulingOf(ctx context.Context, q *db.Queries, orgID int64, people []int64) error {
	ids := slices.Clone(people)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if err := q.LockPatrolKey(ctx, fmt.Sprintf("patrol:schedule:%d:%d", orgID, id)); err != nil {
			return fmt.Errorf("lock schedules of %d: %w", id, err)
		}
	}
	return nil
}

// checkWindow validates one window: it is at most 24 hours and positive, and (for a new or moved one) not over.
func checkWindow(field string, startsAt, endsAt, now time.Time) []apperr.FieldError {
	var out []apperr.FieldError
	switch {
	case !endsAt.After(startsAt):
		out = append(out, apperr.FieldError{Field: field + ".ends_at", Code: "INVALID", Message: "Harus setelah starts_at."})
	case endsAt.Sub(startsAt) > maxScheduleWindow:
		out = append(out, apperr.FieldError{Field: field + ".ends_at", Code: "INVALID", Message: "Jendela jadwal paling lama 24 jam."})
	case !endsAt.After(now):
		out = append(out, apperr.FieldError{Field: field + ".ends_at", Code: "INVALID", Message: "Jadwal tidak boleh sudah berakhir."})
	}
	return out
}

// allowedToPatrol says whether the person may patrol the zone on the day: the permission of the zone's type and a
// membership in the zone's project (and regu, when the zone has one), judged on the work date as a scan is.
func (s *Service) allowedToPatrol(ctx context.Context, q *db.Queries, personnelID int64, zone db.PatrolZone, day timeutil.Date) (bool, error) {
	if s.perms == nil {
		return false, errors.New("patrol: permissions are not plugged in")
	}
	perm := access.PatrolScan
	if zone.Type == db.PatrolZoneTypeGUARD {
		perm = access.GuardScan
	}
	res, err := s.perms.Resolve(ctx, q, personnelID, perm, day)
	if err != nil {
		return false, err
	}
	if !res.Allowed {
		return false, nil
	}
	if _, err := s.membershipIn(ctx, q, personnelID, zone, day); err != nil {
		if errors.Is(err, ErrNotInProject) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// conflictOf is one overlap, as details.conflicts lists it.
func conflictOf(personnelID int64, startsAt, endsAt time.Time, existing string) map[string]any {
	c := map[string]any{"personnel_id": personnelID, "starts_at": startsAt.UTC().Format(time.RFC3339), "ends_at": endsAt.UTC().Format(time.RFC3339)}
	if existing != "" {
		c["schedule_id"] = existing
	}
	return c
}

func overlaps(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}

// CreateSchedules makes one schedule per person and slot, all or nothing, and returns them. A person who may not
// patrol the zone on a slot's day, or who is already scheduled in the window, rejects the whole request, and every
// overlap is reported, not only the first.
func (s *Service) CreateSchedules(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ScheduleInput) ([]Schedule, error) {
	people := dedupe(in.PersonnelIDs)
	if len(people)*len(in.Slots) > maxScheduleRows {
		return nil, ErrScheduleTooMany.WithDetails(map[string]any{"max": maxScheduleRows, "requested": len(people) * len(in.Slots)})
	}
	now := s.clock.Now()
	var problems []apperr.FieldError
	for i, sl := range in.Slots {
		problems = append(problems, checkWindow(fmt.Sprintf("slots[%d]", i), sl.StartsAt, sl.EndsAt, now)...)
	}
	if len(problems) > 0 {
		return nil, apperr.ErrValidation.WithFields(problems...)
	}

	var out []Schedule
	var notices []Notice
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		zone, err := q.GetPatrolZone(ctx, in.ZoneID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && zone.DeletedAt.Valid) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		if err := assertRegion(scope, zone.ProjectCode); err != nil {
			return err
		}
		if !zone.Active {
			return apperr.ErrValidation.WithFields(apperr.FieldError{Field: "zone_id", Code: "INVALID", Message: "Zona tidak aktif."})
		}
		project, err := q.GetProject(ctx, zone.ProjectCode)
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		loc, err := time.LoadLocation(project.Timezone)
		if err != nil {
			return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
		}
		names, err := personnelNames(ctx, q, people)
		if err != nil {
			return err
		}
		for i, id := range in.PersonnelIDs {
			if _, ok := names[id]; !ok {
				return apperr.ErrValidation.WithFields(apperr.FieldError{
					Field: fmt.Sprintf("personnel_ids[%d]", i), Code: "INVALID", Message: "Personel tidak ditemukan.",
				})
			}
		}

		if err := lockSchedulingOf(ctx, q, actor.OrganizationID, people); err != nil {
			return err
		}
		if err := s.checkPeopleAndOverlaps(ctx, q, zone, loc, people, names, in.Slots, nil); err != nil {
			return err
		}

		for _, person := range people {
			var ids []string
			for _, sl := range in.Slots {
				row, err := q.InsertPatrolSchedule(ctx, db.InsertPatrolScheduleParams{
					ProjectCode: zone.ProjectCode, ZoneID: zone.ID, PersonnelID: person, StartsAt: ts(sl.StartsAt), EndsAt: ts(sl.EndsAt),
					WorkDate: pgtype.Date{Time: timeutil.BusinessDate(sl.StartsAt, loc).Time(), Valid: true},
					Notes:    textOrNull(in.Notes), CreatedByID: actor.PersonnelID, Now: ts(now),
				})
				if err != nil {
					return fmt.Errorf("insert schedule: %w", err)
				}
				sc := scheduleOf(now, row, zone.Name, zone.Type, names[person])
				out = append(out, sc)
				ids = append(ids, sc.ID)
				if err := auditSchedule(ctx, q, actor, "patrol.schedule.create", sc, map[string]any{"after": scheduleMeta(sc)}); err != nil {
					return err
				}
			}
			notices = append(notices, s.scheduleNotice(project, zone, actor, person, ScheduleChangeCreated, ids))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.notify(ctx, notices...)
	return out, nil
}

// personnelNames maps the ids to their names; an id with no person of the organization is left out.
func personnelNames(ctx context.Context, q *db.Queries, ids []int64) (map[int64]string, error) {
	rows, err := q.ListPersonnelNames(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list names: %w", err)
	}
	out := make(map[int64]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.FullName
	}
	return out, nil
}

// checkPeopleAndOverlaps is the two rules of a new window: every person may patrol the zone on its work date
// (422 SCHEDULE_PERSON_NOT_ALLOWED), and none of them is scheduled in it already, by another schedule or by
// another slot of the same request (409 PATROL_SCHEDULE_OVERLAP, listing every overlap). The caller holds
// lockSchedulingOf for the people. exceptID leaves out the schedule being edited.
func (s *Service) checkPeopleAndOverlaps(ctx context.Context, q *db.Queries, zone db.PatrolZone, loc *time.Location,
	people []int64, names map[int64]string, slots []ScheduleSlot, exceptID *pgtype.UUID,
) error {
	type key struct {
		person int64
		day    timeutil.Date
	}
	checked := map[key]bool{}
	for _, person := range people {
		for _, sl := range slots {
			day := timeutil.BusinessDate(sl.StartsAt, loc)
			k := key{person, day}
			if checked[k] {
				continue
			}
			ok, err := s.allowedToPatrol(ctx, q, person, zone, day)
			if err != nil {
				return err
			}
			if !ok {
				return errSchedulePersonNotAllowed(person, names[person], day)
			}
			checked[k] = true
		}
	}

	var conflicts []map[string]any
	for _, person := range people {
		first, last := slots[0].StartsAt, slots[0].EndsAt
		for _, sl := range slots {
			if sl.StartsAt.Before(first) {
				first = sl.StartsAt
			}
			if sl.EndsAt.After(last) {
				last = sl.EndsAt
			}
		}
		var except pgtype.UUID
		if exceptID != nil {
			except = *exceptID
		}
		existing, err := q.ListOverlappingSchedules(ctx, db.ListOverlappingSchedulesParams{
			PersonnelID: person, WindowStart: ts(first), WindowEnd: ts(last), ExceptID: except,
		})
		if err != nil {
			return fmt.Errorf("list overlapping schedules: %w", err)
		}
		for i, sl := range slots {
			for _, e := range existing {
				if overlaps(sl.StartsAt, sl.EndsAt, e.StartsAt.Time, e.EndsAt.Time) {
					conflicts = append(conflicts, conflictOf(person, e.StartsAt.Time, e.EndsAt.Time, uuidString(e.ID)))
				}
			}
			for _, other := range slots[i+1:] {
				if overlaps(sl.StartsAt, sl.EndsAt, other.StartsAt, other.EndsAt) {
					conflicts = append(conflicts, conflictOf(person, other.StartsAt, other.EndsAt, ""))
				}
			}
		}
	}
	if len(conflicts) > 0 {
		return ErrScheduleOverlap.WithDetails(map[string]any{"conflicts": conflicts})
	}
	return nil
}

// scheduleNotice is the one notice a person gets for a request: change says what happened to count schedules.
func (s *Service) scheduleNotice(project db.Project, zone db.PatrolZone, _ auth.Principal, person int64, change string, ids []string) Notice {
	return Notice{
		Kind: NoticeScheduleChanged, RecipientIDs: []int64{person}, ZoneType: string(zone.Type), BusinessLine: string(project.BusinessLine),
		Subject: zone.Name, ZoneID: zone.ID, ProjectCode: zone.ProjectCode, ScheduleChange: change, ScheduleIDs: ids,
	}
}

// loadSchedule reads a schedule for update and checks the admin's region. A schedule that is not there is
// ErrScheduleNotFound; one that is no longer SCHEDULED is ErrScheduleLocked.
func (s *Service) loadSchedule(ctx context.Context, q *db.Queries, scope access.AdminScope, id string, now time.Time) (db.PatrolSchedule, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return db.PatrolSchedule{}, ErrScheduleNotFound
	}
	row, err := q.GetPatrolScheduleForUpdate(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PatrolSchedule{}, ErrScheduleNotFound
	}
	if err != nil {
		return db.PatrolSchedule{}, fmt.Errorf("get schedule: %w", err)
	}
	if err := assertRegion(scope, row.ProjectCode); err != nil {
		return db.PatrolSchedule{}, err
	}
	var cancelled *time.Time
	if row.CancelledAt.Valid {
		t := row.CancelledAt.Time
		cancelled = &t
	}
	if status := scheduleStatus(now, scheduleTimes{EndsAt: row.EndsAt.Time, CancelledAt: cancelled}, nil); status != ScheduleScheduled {
		return db.PatrolSchedule{}, ErrScheduleLocked.WithDetails(map[string]any{"status": status})
	}
	return row, nil
}

// UpdateSchedule changes the person, the window or the notes of a schedule that is still SCHEDULED. Moving it to
// another person tells the old one that it was cancelled and the new one that it was made.
func (s *Service) UpdateSchedule(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string, in ScheduleUpdate) (Schedule, error) {
	if f := in.Notes.resolve("notes"); f != nil {
		return Schedule{}, apperr.ErrValidation.WithFields(*f)
	}
	if n := in.Notes.ptr(); n != nil && len(*n) > 2000 {
		return Schedule{}, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "notes", Code: "TOO_LONG", Message: "Paling banyak 2000 karakter."})
	}
	now := s.clock.Now()

	var out Schedule
	var notices []Notice
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The person is locked before the row is read, so the lock order is the same as a create's.
		uid, ok := parseUUID(id)
		if !ok {
			return ErrScheduleNotFound
		}
		peek, err := q.GetPatrolSchedule(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduleNotFound
		}
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		people := []int64{peek.PersonnelID}
		if in.PersonnelID != nil {
			people = append(people, *in.PersonnelID)
		}
		if err := lockSchedulingOf(ctx, q, actor.OrganizationID, people); err != nil {
			return err
		}
		row, err := s.loadSchedule(ctx, q, scope, id, now)
		if err != nil {
			return err
		}
		zone, err := q.GetPatrolZone(ctx, row.ZoneID)
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		project, err := q.GetProject(ctx, row.ProjectCode)
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		loc, err := time.LoadLocation(project.Timezone)
		if err != nil {
			return fmt.Errorf("project %s: timezone %q: %w", project.Code, project.Timezone, err)
		}

		next := row
		if in.PersonnelID != nil {
			next.PersonnelID = *in.PersonnelID
		}
		if in.StartsAt != nil {
			next.StartsAt = ts(*in.StartsAt)
		}
		if in.EndsAt != nil {
			next.EndsAt = ts(*in.EndsAt)
		}
		if in.Notes.set {
			next.Notes = textOrNull(in.Notes.ptr())
		}
		moved := next.PersonnelID != row.PersonnelID || !next.StartsAt.Time.Equal(row.StartsAt.Time) || !next.EndsAt.Time.Equal(row.EndsAt.Time)
		if moved {
			if problems := checkWindow("", next.StartsAt.Time, next.EndsAt.Time, now); len(problems) > 0 {
				for i := range problems {
					problems[i].Field = strings.TrimPrefix(problems[i].Field, ".")
				}
				return apperr.ErrValidation.WithFields(problems...)
			}
			names, err := personnelNames(ctx, q, []int64{next.PersonnelID, row.PersonnelID})
			if err != nil {
				return err
			}
			if _, ok := names[next.PersonnelID]; !ok {
				return apperr.ErrValidation.WithFields(apperr.FieldError{Field: "personnel_id", Code: "INVALID", Message: "Personel tidak ditemukan."})
			}
			slot := []ScheduleSlot{{StartsAt: next.StartsAt.Time, EndsAt: next.EndsAt.Time}}
			if err := s.checkPeopleAndOverlaps(ctx, q, zone, loc, []int64{next.PersonnelID}, names, slot, &row.ID); err != nil {
				return err
			}
			next.WorkDate = pgtype.Date{Time: timeutil.BusinessDate(next.StartsAt.Time, loc).Time(), Valid: true}
		}
		saved, err := q.UpdatePatrolSchedule(ctx, db.UpdatePatrolScheduleParams{
			ID: row.ID, PersonnelID: next.PersonnelID, StartsAt: next.StartsAt, EndsAt: next.EndsAt, WorkDate: next.WorkDate,
			Notes: next.Notes, Now: ts(now),
		})
		if err != nil {
			return fmt.Errorf("update schedule: %w", err)
		}
		names, err := personnelNames(ctx, q, []int64{saved.PersonnelID, row.PersonnelID})
		if err != nil {
			return err
		}
		before := scheduleOf(now, row, zone.Name, zone.Type, names[row.PersonnelID])
		out = scheduleOf(now, saved, zone.Name, zone.Type, names[saved.PersonnelID])
		if err := auditSchedule(ctx, q, actor, "patrol.schedule.update", out, map[string]any{"before": scheduleMeta(before), "after": scheduleMeta(out)}); err != nil {
			return err
		}
		if saved.PersonnelID != row.PersonnelID {
			notices = append(notices,
				s.scheduleNotice(project, zone, actor, row.PersonnelID, ScheduleChangeCancelled, []string{out.ID}),
				s.scheduleNotice(project, zone, actor, saved.PersonnelID, ScheduleChangeCreated, []string{out.ID}))
		} else {
			notices = append(notices, s.scheduleNotice(project, zone, actor, saved.PersonnelID, ScheduleChangeUpdated, []string{out.ID}))
		}
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	s.notify(ctx, notices...)
	return out, nil
}

// CancelSchedule cancels a schedule that is still SCHEDULED: it is kept, with cancelled_at, because a patrol
// may point at it (Phase 10.12).
func (s *Service) CancelSchedule(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string) error {
	now := s.clock.Now()
	var notices []Notice
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		uid, ok := parseUUID(id)
		if !ok {
			return ErrScheduleNotFound
		}
		peek, err := q.GetPatrolSchedule(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduleNotFound
		}
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		if err := lockSchedulingOf(ctx, q, actor.OrganizationID, []int64{peek.PersonnelID}); err != nil {
			return err
		}
		row, err := s.loadSchedule(ctx, q, scope, id, now)
		if err != nil {
			return err
		}
		saved, err := q.CancelPatrolSchedule(ctx, db.CancelPatrolScheduleParams{ID: row.ID, CancelledByID: pgtype.Int8{Int64: actor.PersonnelID, Valid: true}, Now: ts(now)})
		if err != nil {
			return fmt.Errorf("cancel schedule: %w", err)
		}
		zone, err := q.GetPatrolZone(ctx, saved.ZoneID)
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		project, err := q.GetProject(ctx, saved.ProjectCode)
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		names, err := personnelNames(ctx, q, []int64{saved.PersonnelID})
		if err != nil {
			return err
		}
		sc := scheduleOf(now, saved, zone.Name, zone.Type, names[saved.PersonnelID])
		if err := auditSchedule(ctx, q, actor, "patrol.schedule.cancel", sc, map[string]any{"before": scheduleMeta(sc)}); err != nil {
			return err
		}
		notices = append(notices, s.scheduleNotice(project, zone, actor, saved.PersonnelID, ScheduleChangeCancelled, []string{sc.ID}))
		return nil
	})
	if err != nil {
		return err
	}
	s.notify(ctx, notices...)
	return nil
}

// ListSchedulesAdmin returns one page of the schedules of the admin's region, and the total.
func (s *Service) ListSchedulesAdmin(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ScheduleFilter) ([]Schedule, int, error) {
	if f.ProjectCode != "" {
		if err := assertRegion(scope, f.ProjectCode); err != nil {
			return nil, 0, err
		}
	}
	region := pgtype.Text{String: scope.RegionCode, Valid: !scope.All}
	return s.listSchedules(ctx, actor, f, db.ListPatrolSchedulesParams{ScopeRegion: region})
}

// ListSchedules returns one page of the schedules the caller may read: their own, and (for a DANRU) those of the
// people of the regu they decide for, with the range of the filter or, when it has none, today and the next 7 days
// in the timezone of the caller's project.
func (s *Service) ListSchedules(ctx context.Context, p auth.Principal, f ScheduleFilter) ([]Schedule, int, error) {
	if s.perms == nil {
		return nil, 0, errors.New("patrol: permissions are not plugged in")
	}
	var base db.ListPatrolSchedulesParams
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		reach, err := s.scheduleReach(ctx, q, p.PersonnelID)
		if err != nil {
			return err
		}
		base = db.ListPatrolSchedulesParams{
			ViewerID: pgtype.Int8{Int64: p.PersonnelID, Valid: true}, PatrolTeams: reach[db.PatrolZoneTypePATROL], GuardTeams: reach[db.PatrolZoneTypeGUARD],
		}
		if f.DateFrom == nil && f.DateTo == nil {
			loc, err := s.callerLocation(ctx, q, p.PersonnelID)
			if err != nil {
				return err
			}
			from := timeutil.BusinessDate(s.clock.Now(), loc)
			to := from.AddDays(defaultScheduleDays)
			f.DateFrom, f.DateTo = &from, &to
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return s.listSchedules(ctx, p, f, base)
}

// scheduleReach is, per zone type, the regu whose people the caller may see the schedules of: the regu of their
// approve permission, as the DANRU's queue uses (none for a member).
func (s *Service) scheduleReach(ctx context.Context, q *db.Queries, personnelID int64) (map[db.PatrolZoneType][]string, error) {
	reach := map[db.PatrolZoneType][]string{}
	for _, t := range []db.PatrolZoneType{db.PatrolZoneTypePATROL, db.PatrolZoneTypeGUARD} {
		res, err := s.perms.ResolveToday(ctx, q, personnelID, approvePermission(t))
		if err != nil {
			return nil, err
		}
		reach[t] = []string{}
		if res.Allowed && !res.SelfOnly {
			reach[t] = res.TeamCodes
		}
	}
	return reach, nil
}

// callerLocation is the timezone of the first project the person serves in now; the business timezone of the
// organization's biggest zone when they serve in none.
func (s *Service) callerLocation(ctx context.Context, q *db.Queries, personnelID int64) (*time.Location, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{PersonnelID: personnelID, Now: ts(s.clock.Now())})
	if err != nil {
		return nil, fmt.Errorf("list memberships: %w", err)
	}
	name := "Asia/Jakarta"
	if len(rows) > 0 {
		project, err := q.GetProject(ctx, rows[0].ProjectCode)
		if err != nil {
			return nil, fmt.Errorf("get project: %w", err)
		}
		name = project.Timezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", name, err)
	}
	return loc, nil
}

func (s *Service) listSchedules(ctx context.Context, actor auth.Principal, f ScheduleFilter, base db.ListPatrolSchedulesParams) ([]Schedule, int, error) {
	if f.DateFrom != nil && f.DateTo != nil && f.DateTo.Before(*f.DateFrom) {
		return nil, 0, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "date_to", Code: "INVALID", Message: "Tidak boleh sebelum date_from."})
	}
	now := s.clock.Now()
	base.ProjectCode, base.ZoneID, base.PersonnelID = text(f.ProjectCode), int8Param(positive(f.ZoneID)), int8Param(positive(f.PersonnelID))
	base.DateFrom, base.DateTo, base.Status, base.Now = dateParam(f.DateFrom), dateParam(f.DateTo), text(f.Status), ts(now)
	if base.PatrolTeams == nil {
		base.PatrolTeams, base.GuardTeams = []string{}, []string{}
	}
	base.PageLimit, base.PageOffset = int32(f.Limit), int32(f.Offset) //nolint:gosec // bounded by ParseListQuery

	var out []Schedule
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListPatrolSchedules(ctx, base)
		if err != nil {
			return fmt.Errorf("list schedules: %w", err)
		}
		total, err = q.CountPatrolSchedules(ctx, db.CountPatrolSchedulesParams{
			ScopeRegion: base.ScopeRegion, ViewerID: base.ViewerID, PatrolTeams: base.PatrolTeams, GuardTeams: base.GuardTeams,
			ProjectCode: base.ProjectCode, ZoneID: base.ZoneID, PersonnelID: base.PersonnelID, DateFrom: base.DateFrom, DateTo: base.DateTo,
			Status: base.Status, Now: base.Now,
		})
		if err != nil {
			return fmt.Errorf("count schedules: %w", err)
		}
		out = make([]Schedule, 0, len(rows))
		for _, r := range rows {
			out = append(out, scheduleOfRow(now, r))
		}
		return nil
	})
	return out, int(total), err
}

// GetSchedule returns one schedule to someone who may read it: its person, or a DANRU of the regu the person was in
// on the schedule's work date (by the approve permission of the zone's type). Anyone else gets 404.
func (s *Service) GetSchedule(ctx context.Context, p auth.Principal, id string) (Schedule, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return Schedule{}, ErrScheduleNotFound
	}
	var out Schedule
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolScheduleView(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrScheduleNotFound
		}
		if err != nil {
			return fmt.Errorf("get schedule: %w", err)
		}
		if row.PersonnelID != p.PersonnelID {
			reach, err := s.approveReach(ctx, q, p.PersonnelID, row.ZoneType, dateOf(row.WorkDate))
			if err != nil {
				return err
			}
			memberships, err := q.ListMembershipsOfPersonnelOn(ctx, db.ListMembershipsOfPersonnelOnParams{PersonnelID: row.PersonnelID, OnDate: row.WorkDate})
			if err != nil {
				return fmt.Errorf("list memberships: %w", err)
			}
			seen := false
			for _, m := range memberships {
				if m.ProjectCode == row.ProjectCode && slices.Contains(reach, m.TeamCode) {
					seen = true
				}
			}
			if !seen {
				return ErrScheduleNotFound
			}
		}
		out = scheduleOf(s.clock.Now(), db.PatrolSchedule{
			ID: row.ID, OrganizationID: row.OrganizationID, ProjectCode: row.ProjectCode, ZoneID: row.ZoneID, PersonnelID: row.PersonnelID,
			StartsAt: row.StartsAt, EndsAt: row.EndsAt, WorkDate: row.WorkDate, Notes: row.Notes, CreatedByID: row.CreatedByID,
			CancelledAt: row.CancelledAt, CancelledByID: row.CancelledByID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, row.ZoneName, row.ZoneType, row.PersonnelName)
		return nil
	})
	return out, err
}

func auditSchedule(ctx context.Context, q *db.Queries, actor auth.Principal, action string, sc Schedule, meta map[string]any) error {
	actorID := actor.PersonnelID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["project_code"] = sc.ProjectCode
	// A schedule is named by its uuid, so it goes in EntityCode: EntityID is a number.
	return audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &actorID, Action: action, EntityType: "patrol_schedule", EntityCode: sc.ID, Metadata: meta,
	})
}

// scheduleMeta is what the audit log keeps of a schedule: where, who and when, not the person's name.
func scheduleMeta(sc Schedule) map[string]any {
	return map[string]any{
		"zone_id": sc.ZoneID, "personnel_id": sc.PersonnelID, "starts_at": sc.StartsAt.UTC().Format(time.RFC3339),
		"ends_at": sc.EndsAt.UTC().Format(time.RFC3339), "work_date": sc.WorkDate.String(), "notes": sc.Notes,
	}
}
