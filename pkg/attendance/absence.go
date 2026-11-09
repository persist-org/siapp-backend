package attendance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/organization"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Phase 07.6 Part A: the DANRU marking a team member absent, and the regu monitor list.

// wib is the zone "today" is read in where no single project is meant (a list over several regu,
// each possibly in its own timezone): the same choice pkg/leave's admin list makes.
var wib = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Jakarta"); err == nil {
		return loc
	}
	return time.FixedZone("WIB", 7*60*60)
}()

// textOrNull is "" as SQL NULL, anything else as itself.
func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// instanceFor finds personnelID's shift instance of teamCode on date: the one a mark-absent or a
// group-present entry turns into a record. ok is false when the person has no working shift of
// that regu that day (an OFF day, a day they are not scheduled, or a regu they do not serve in).
//
// ⚠ The project code comes from TeamWithProject, not by indexing ShiftInstance.ProjectCodes at
// TeamCodes' position: the two lists are deduped independently (schedules.go's zonesBetween), so
// they are NOT index-aligned when a person serves two regu of the same project, or other such
// overlaps. Looking the team up by code is the only safe way to its project.
func (s *Service) instanceFor(ctx context.Context, organizationID, personnelID int64, teamCode string, date timeutil.Date) (inst organization.ShiftInstance, projectCode string, ok bool, err error) {
	instances, err := s.shifts.Instances(ctx, organizationID, personnelID, date, date)
	if err != nil {
		return organization.ShiftInstance{}, "", false, fmt.Errorf("shift instances: %w", err)
	}
	for _, in := range instances {
		if in.IsOff || !slices.Contains(in.TeamCodes, teamCode) {
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

// insertAbsent writes a fresh ABSENT record for personnelID's shift instance and its outbox event,
// inside the caller's transaction. Shared by MarkAbsent and GroupPresent's ABSENT entries.
//
// ⚠ A place that already has a record is not the caller's to turn into an absence: refusalFor gives
// the same answer a conflicting check-in would (ALREADY_CHECKED_IN for a PRESENT record,
// NOT_ALLOWED for an existing LEAVE or ABSENT). An admin correcting an existing record instead goes
// through Correct (Part C), which locks and updates the row rather than inserting one.
func (s *Service) insertAbsent(ctx context.Context, q *db.Queries, markedByID, personnelID int64, teamCode, projectCode string, inst organization.ShiftInstance, reason string, now time.Time) (db.Attendance, error) {
	existing, err := existingRecord(ctx, q, personnelID, shiftKey{TeamCode: teamCode, WorkDate: inst.WorkDate, ShiftID: inst.ShiftID})
	if err != nil {
		return db.Attendance{}, err
	}
	if existing != nil {
		return db.Attendance{}, refusalFor(*existing)
	}
	row, err := q.InsertAttendanceAbsent(ctx, db.InsertAttendanceAbsentParams{
		PersonnelID: personnelID, TeamCode: teamCode, ProjectCode: projectCode, WorkDate: dateParam(inst.WorkDate),
		ShiftID: inst.ShiftID, ShiftStartsAt: ts(inst.StartsAt), ShiftEndsAt: ts(inst.EndsAt),
		Timezone: inst.Location.String(), MarkedAbsentByID: pgtype.Int8{Int64: markedByID, Valid: true},
		AbsentReason: textOrNull(reason), Now: ts(now),
	})
	if err != nil {
		return db.Attendance{}, fmt.Errorf("insert absent: %w", err)
	}
	if err := writeEvent(ctx, q, row, eventAbsent, now); err != nil {
		return db.Attendance{}, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &markedByID, Action: "attendance.absent.create", EntityType: "attendance", EntityID: row.ID,
		Metadata: map[string]any{
			"personnel_id": personnelID, "team_code": teamCode, "work_date": dateValue(row.WorkDate).String(), "reason": reason,
		},
	}); err != nil {
		return db.Attendance{}, err
	}
	return row, nil
}

// AbsentInput is the body of POST /attendances/absent.
type AbsentInput struct {
	PersonnelID int64  `json:"personnel_id" validate:"required,min=1"`
	TeamCode    string `json:"team_code" validate:"required"`
	// Date is the work date (YYYY-MM-DD) the person is marked absent for: their shift instance of
	// TeamCode on that date, which may be a different regu's shift than the caller's own today.
	Date   string `json:"date" validate:"required"`
	Reason string `json:"reason" validate:"omitempty,max=500"`
}

// MarkAbsent is POST /attendances/absent: the DANRU recording a team member's mangkir directly,
// without that member ever checking in. Guarded by attendance.absence.create, resolved for the
// work date the absence is about (not necessarily today: a DANRU may be catching up on yesterday).
func (s *Service) MarkAbsent(ctx context.Context, p auth.Principal, in AbsentInput) (Attendance, error) {
	date, err := timeutil.ParseDate(in.Date)
	if err != nil {
		return Attendance{}, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "date", Code: "INVALID", Message: "date harus berformat YYYY-MM-DD.",
		})
	}
	now := s.clock.Now()
	var out Attendance
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceAbsenceNew, date)
		if err != nil {
			return err
		}
		if !res.HasTeam(in.TeamCode) {
			return access.ErrPermissionDenied
		}
		inst, projectCode, ok, err := s.instanceFor(ctx, p.OrganizationID, in.PersonnelID, in.TeamCode, date)
		if err != nil {
			return err
		}
		if !ok {
			return ErrPersonnelNotScheduled
		}
		row, err := s.insertAbsent(ctx, q, p.PersonnelID, in.PersonnelID, in.TeamCode, projectCode, inst, in.Reason, now)
		if err != nil {
			return err
		}
		out = toAttendance(row)
		return nil
	})
	return out, err
}

// ---------- the monitor list (GET /attendances, GET /admin/attendances) ----------

// Filter narrows a monitor list. TeamCode and RegionCode are mutually exclusive in practice: the
// DANRU monitor (List) never sets RegionCode, the admin monitor (ListForAdmin, Part C) never sets
// TeamCode outside what the caller's own filter asks for within their reach.
type Filter struct {
	TeamCode    string
	ProjectCode string
	RegionCode  string
	From, To    *timeutil.Date
	Limit       int
	Offset      int
}

// Page is a page of the monitor list.
type Page struct {
	Attendances []Attendance
	Total       int
}

// reachableTeams is every regu a grant of AttendanceGroupView or AttendanceAbsenceView reaches,
// resolved once for "today" in WIB: a monitor spans every regu and project the caller leads, each
// possibly in its own zone, so there is no one business date to resolve a list against (the same
// simplification pkg/leave's admin list makes).
func (s *Service) reachableTeams(ctx context.Context, q *db.Queries, personnelID int64, on timeutil.Date) ([]string, error) {
	var teams []string
	for _, perm := range []access.Permission{access.AttendanceGroupView, access.AttendanceAbsenceView} {
		res, err := s.access.Resolve(ctx, q, personnelID, perm, on)
		if err != nil {
			return nil, err
		}
		for _, t := range res.TeamCodes {
			if !slices.Contains(teams, t) {
				teams = append(teams, t)
			}
		}
	}
	return teams, nil
}

// List is GET /attendances: every record of a regu within the caller's attendance.group.view or
// attendance.absence.view reach, newest first. f.TeamCode narrows to one of those regu; asking for
// one outside the reach is PERMISSION_DENIED, the same answer as asking for none at all.
func (s *Service) List(ctx context.Context, p auth.Principal, f Filter) (Page, error) {
	var out Page
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		teams, err := s.reachableTeams(ctx, q, p.PersonnelID, timeutil.BusinessDate(s.clock.Now(), wib))
		if err != nil {
			return err
		}
		if f.TeamCode != "" {
			if !slices.Contains(teams, f.TeamCode) {
				return access.ErrPermissionDenied
			}
			teams = []string{f.TeamCode}
		}
		if len(teams) == 0 {
			return access.ErrPermissionDenied
		}
		return s.listInto(ctx, q, &out, db.ListAttendancesParams{
			TeamCodes: teams, ProjectCode: textOrNull(f.ProjectCode),
			FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageOffset: int32(f.Offset), PageLimit: int32(f.Limit), //nolint:gosec // bounded by httpx.ParseListQuery
		})
	})
	return out, err
}

// listInto reads one page of the monitor list and its total, shared by List and (Part C)
// ListForAdmin.
func (s *Service) listInto(ctx context.Context, q *db.Queries, out *Page, params db.ListAttendancesParams) error {
	rows, err := q.ListAttendances(ctx, params)
	if err != nil {
		return fmt.Errorf("list attendances: %w", err)
	}
	out.Attendances = make([]Attendance, 0, len(rows))
	for _, r := range rows {
		out.Attendances = append(out.Attendances, toAttendance(r))
	}
	total, err := q.CountAttendances(ctx, db.CountAttendancesParams{
		TeamCodes: params.TeamCodes, TeamCode: params.TeamCode, ProjectCode: params.ProjectCode,
		RegionCode: params.RegionCode, FromDate: params.FromDate, ToDate: params.ToDate,
	})
	if err != nil {
		return fmt.Errorf("count attendances: %w", err)
	}
	out.Total = int(total)
	return nil
}

func dateParamPtr(d *timeutil.Date) pgtype.Date {
	if d == nil {
		return pgtype.Date{}
	}
	return dateParam(*d)
}
