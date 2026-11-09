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
	"siapp/pkg/platform/geo"
)

// Phase 07.6 Part B: the DANRU's group check-in. One shift, one location, a list of people, all
// marked PRESENT at once with method GROUP — off unless the project has opted in
// (allow_group_attendance), since it records presence without each guard's own evidence, which
// weakens the anti-fraud purpose of QR and selfie (manual §2.6.1). A bad entry (not scheduled,
// already recorded) does not fail the batch: every entry gets its own result.

// GroupInput is the body of POST /attendances/group.
type GroupInput struct {
	TeamCode string `json:"team_code" validate:"required"`
	// ShiftRef is the shift the whole group is checked into, from the same opaque format self
	// check-in uses (team_code|work_date|shift_id); its team must equal TeamCode.
	ShiftRef     string   `json:"shift_ref" validate:"required"`
	PersonnelIDs []int64  `json:"personnel_ids" validate:"required,min=1,max=200,dive,min=1"`
	Latitude     *float64 `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude    *float64 `json:"longitude" validate:"required,gte=-180,lte=180"`
}

// GroupEntryResult is one person's outcome.
type GroupEntryResult struct {
	PersonnelID int64
	// Attendance is nil when Error is set.
	Attendance *Attendance
	Error      error
}

// instanceAt is instanceFor narrowed to one specific shift (the group check-in's shift_ref)
// instead of any shift of the team: the one a group-present entry turns into a record. ok is
// false when the person is not scheduled for exactly that shift instance of that regu on that
// date.
func (s *Service) instanceAt(ctx context.Context, organizationID, personnelID int64, key shiftKey) (organization.ShiftInstance, bool, error) {
	instances, err := s.shifts.Instances(ctx, organizationID, personnelID, key.WorkDate, key.WorkDate)
	if err != nil {
		return organization.ShiftInstance{}, false, fmt.Errorf("shift instances: %w", err)
	}
	for _, in := range instances {
		if in.IsOff || in.ShiftID != key.ShiftID {
			continue
		}
		if slices.Contains(in.TeamCodes, key.TeamCode) {
			return in, true, nil
		}
	}
	return organization.ShiftInstance{}, false, nil
}

// GroupPresent is POST /attendances/group: the DANRU checking in their regu's current shift in one
// call, from their own location. Guarded by attendance.group.view (resolved for the shift's own
// work date, not necessarily today: a DANRU may be catching up on an earlier shift) and by the
// project's allow_group_attendance toggle.
func (s *Service) GroupPresent(ctx context.Context, p auth.Principal, in GroupInput) ([]GroupEntryResult, error) {
	key, ok := parseShiftRef(in.ShiftRef)
	if !ok || key.TeamCode != in.TeamCode {
		return nil, ErrShiftNotActive
	}
	now := s.clock.Now()
	out := make([]GroupEntryResult, 0, len(in.PersonnelIDs))
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceGroupView, key.WorkDate)
		if err != nil {
			return err
		}
		if !res.HasTeam(in.TeamCode) {
			return access.ErrPermissionDenied
		}
		site, err := s.sites.TeamWithProject(ctx, p.OrganizationID, in.TeamCode)
		if errors.Is(err, organization.ErrTeamNotFound) {
			return ErrShiftNotActive
		}
		if err != nil {
			return fmt.Errorf("team %s: %w", in.TeamCode, err)
		}
		if !site.Project.AllowGroupAttendance {
			return ErrGroupAttendanceOff
		}
		m, err := measure(site.Project, &geo.Point{Lat: *in.Latitude, Lng: *in.Longitude})
		if err != nil {
			return err
		}
		for _, personnelID := range in.PersonnelIDs {
			r := GroupEntryResult{PersonnelID: personnelID}
			inst, found, ierr := s.instanceAt(ctx, p.OrganizationID, personnelID, key)
			switch {
			case ierr != nil:
				return ierr // an infra error aborts the whole batch; a business refusal does not
			case !found:
				r.Error = ErrNoShift
			default:
				row, cerr := s.groupCheckIn(ctx, q, p.PersonnelID, personnelID, in.TeamCode, site.Project.Code, inst, in.Latitude, in.Longitude, m, now)
				if cerr != nil {
					r.Error = cerr
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

// groupCheckIn writes a GROUP-method check-in for one entry: the DANRU's own fix (measured against
// the project's geofence once, before the loop, and applied to every entry), timestamped at the
// muster's call time. Guards are the same as a self check-in (an existing record refuses, the
// previous working day must be settled) and lateness is computed the same way (an approved permit
// can still lower it), so GROUP is a capture method like any other, not a shortcut around them.
func (s *Service) groupCheckIn(ctx context.Context, q *db.Queries, markedByID, personnelID int64, teamCode, projectCode string, inst organization.ShiftInstance, lat, lng *float64, m measurement, now time.Time) (db.Attendance, error) {
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
		Now: ts(now), Method: nullMethod(db.AttendanceMethodGROUP), Latitude: lat, Longitude: lng,
		DistanceMeters: m.distance, WithinArea: m.inArea,
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
