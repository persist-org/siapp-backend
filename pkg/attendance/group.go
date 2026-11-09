package attendance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/leave"
	"siapp/pkg/organization"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Phase 07.6 Part B: the DANRU's group-present muster. One submission marks the whole roster for
// the shift happening right now: PRESENT entries become a GROUP check-in, ABSENT entries go
// through the same path Part A's MarkAbsent uses. A bad entry (not scheduled, already recorded)
// does not fail the batch: every entry gets its own result, like pkg/leave.SkippedDay.

// GroupStatus is what a group-present entry does to a person's record.
type GroupStatus string

const (
	GroupStatusPresent GroupStatus = "PRESENT"
	GroupStatusAbsent  GroupStatus = "ABSENT"
)

// GroupEntryInput is one person of POST /attendances/group.
type GroupEntryInput struct {
	PersonnelID int64  `json:"personnel_id" validate:"required,min=1"`
	Status      string `json:"status" validate:"required,oneof=PRESENT ABSENT"`
	// Reason is kept only for a PRESENT entry turned ABSENT; ignored otherwise.
	Reason string `json:"reason" validate:"omitempty,max=500"`
}

// GroupInput is the body of POST /attendances/group.
type GroupInput struct {
	TeamCode string            `json:"team_code" validate:"required"`
	Entries  []GroupEntryInput `json:"entries" validate:"required,min=1,max=200,dive"`
}

// GroupEntryResult is one entry's outcome.
type GroupEntryResult struct {
	PersonnelID int64
	Status      GroupStatus
	// Attendance is nil when Error is set.
	Attendance *Attendance
	Error      error
}

// currentInstanceFor is instanceFor for "now" rather than a fixed date: the shift instance of
// personnelID in teamCode whose window (organization.Shifts.Current) contains now. Group-present
// acts on the shift happening right now, never a backdated one; Current already leaves out OFF
// days. The project code comes from TeamWithProject, not index-matched into
// ShiftInstance.ProjectCodes — see instanceFor's comment on why that would be unsound.
func (s *Service) currentInstanceFor(ctx context.Context, organizationID, personnelID int64, teamCode string, now time.Time) (inst organization.ShiftInstance, projectCode string, ok bool, err error) {
	instances, err := s.shifts.Current(ctx, organizationID, personnelID, now)
	if err != nil {
		return organization.ShiftInstance{}, "", false, fmt.Errorf("current shifts: %w", err)
	}
	for _, in := range instances {
		if !slices.Contains(in.TeamCodes, teamCode) {
			continue
		}
		site, err := s.sites.TeamWithProject(ctx, organizationID, teamCode)
		if errors.Is(err, organization.ErrTeamNotFound) {
			return organization.ShiftInstance{}, "", false, nil
		}
		if err != nil {
			return organization.ShiftInstance{}, "", false, fmt.Errorf("team %s: %w", teamCode, err)
		}
		return in, site.Project.Code, true, nil
	}
	return organization.ShiftInstance{}, "", false, nil
}

// GroupPresent is POST /attendances/group: the DANRU mustering their regu's current shift in one
// call. Guarded by attendance.group.view, resolved for "today" (the same simplification the
// monitor list makes): a muster is always about the shift happening now.
func (s *Service) GroupPresent(ctx context.Context, p auth.Principal, in GroupInput) ([]GroupEntryResult, error) {
	now := s.clock.Now()
	out := make([]GroupEntryResult, 0, len(in.Entries))
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceGroupView, timeutil.BusinessDate(now, wib))
		if err != nil {
			return err
		}
		if !res.HasTeam(in.TeamCode) {
			return access.ErrPermissionDenied
		}
		for _, e := range in.Entries {
			r := GroupEntryResult{PersonnelID: e.PersonnelID, Status: GroupStatus(e.Status)}
			inst, projectCode, found, err := s.currentInstanceFor(ctx, p.OrganizationID, e.PersonnelID, in.TeamCode, now)
			switch {
			case err != nil:
				return err // an infra error aborts the whole batch; a business refusal does not
			case !found:
				r.Error = ErrPersonnelNotScheduled
			case r.Status == GroupStatusAbsent:
				row, aerr := s.insertAbsent(ctx, q, p.PersonnelID, e.PersonnelID, in.TeamCode, projectCode, inst, e.Reason, now)
				if aerr != nil {
					r.Error = aerr
				} else {
					a := toAttendance(row)
					r.Attendance = &a
				}
			default:
				row, aerr := s.groupCheckIn(ctx, q, p.PersonnelID, e.PersonnelID, in.TeamCode, projectCode, inst, now)
				if aerr != nil {
					r.Error = aerr
				} else {
					a := toAttendance(row)
					r.Attendance = &a
				}
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// groupCheckIn writes a GROUP-method check-in for one entry: no selfie, no GPS, timestamped at the
// muster's call time. Guards are the same as a self check-in (an existing record refuses, the
// previous working day must be settled) and lateness is computed the same way (an approved permit
// can still lower it), so GROUP is a capture method like any other, not a shortcut around them.
func (s *Service) groupCheckIn(ctx context.Context, q *db.Queries, markedByID, personnelID int64, teamCode, projectCode string, inst organization.ShiftInstance, now time.Time) (db.Attendance, error) {
	key := shiftKey{TeamCode: teamCode, WorkDate: inst.WorkDate, ShiftID: inst.ShiftID}
	existing, err := existingRecord(ctx, q, personnelID, key)
	if err != nil {
		return db.Attendance{}, err
	}
	if existing != nil {
		return db.Attendance{}, refusalFor(*existing)
	}
	if err := requirePreviousSettled(ctx, q, personnelID, inst.WorkDate); err != nil {
		return db.Attendance{}, err
	}
	raw := lateMinutes(inst.StartsAt, now, inst.ToleranceMinutes)
	late, err := leave.EffectiveLateness(ctx, q, personnelID, inst.WorkDate, inst.StartsAt, now, raw, inst.Location)
	if err != nil {
		return db.Attendance{}, fmt.Errorf("effective lateness: %w", err)
	}
	row, err := q.InsertAttendanceCheckIn(ctx, db.InsertAttendanceCheckInParams{
		PersonnelID: personnelID, TeamCode: teamCode, ProjectCode: projectCode,
		WorkDate: dateParam(inst.WorkDate), ShiftID: inst.ShiftID,
		ShiftStartsAt: ts(inst.StartsAt), ShiftEndsAt: ts(inst.EndsAt), Timezone: inst.Location.String(),
		Now: ts(now), Method: nullMethod(db.AttendanceMethodGROUP),
		LateMinutes:    int32(min(late, math.MaxInt32)), //nolint:gosec // clamped
		LateMinutesRaw: int32(min(raw, math.MaxInt32)),  //nolint:gosec // clamped
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Attendance{}, s.whyNotCheckedIn(ctx, q, personnelID, key)
	}
	if err != nil {
		return db.Attendance{}, fmt.Errorf("insert group check-in: %w", err)
	}
	if err := writeEvent(ctx, q, row, eventGroupPresent, now); err != nil {
		return db.Attendance{}, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &markedByID, Action: "attendance.group.check_in", EntityType: "attendance", EntityID: row.ID,
		Metadata: map[string]any{"personnel_id": personnelID, "team_code": teamCode},
	}); err != nil {
		return db.Attendance{}, err
	}
	return row, nil
}
