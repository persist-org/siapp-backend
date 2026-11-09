package attendance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
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
// ⚠ A place that already has a record is not the caller's to turn into an absence: a PRESENT row
// is ABSENCE_ALREADY_PRESENT (a guard who came to work), an existing LEAVE or ABSENT is
// NOT_ALLOWED. An admin correcting an existing record instead goes through Correct (Part C), which
// locks and updates the row rather than inserting one.
func (s *Service) insertAbsent(ctx context.Context, q *db.Queries, markedByID, personnelID int64, teamCode, projectCode string, inst organization.ShiftInstance, reason string, now time.Time) (db.Attendance, error) {
	existing, err := existingRecord(ctx, q, personnelID, shiftKey{TeamCode: teamCode, WorkDate: inst.WorkDate, ShiftID: inst.ShiftID})
	if err != nil {
		return db.Attendance{}, err
	}
	if existing != nil {
		if existing.Status == db.AttendanceStatusPRESENT {
			return db.Attendance{}, ErrAbsenceAlreadyPresent
		}
		return db.Attendance{}, ErrNotAllowed
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
			return ErrNoShift
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

// CancelAbsent is DELETE /attendances/absent/{id}: the DANRU undoing their own mistaken mangkir.
// Same permission as creating one (attendance.absence.create), checked against the record's own
// team and work date, not today's: a DANRU may be fixing one from yesterday.
//
// ⚠ The row is deleted outright, as the plan says ("deletes the mistaken ABSENT row"): there is no
// "no record" status to revert it to, only MISSING/UPCOMING, which are computed, never stored. The
// ABSENT_CANCELLED event is written from the row before it goes, so Personel still learns the
// mangkir it was told about is void.
func (s *Service) CancelAbsent(ctx context.Context, p auth.Principal, id int64) error {
	now := s.clock.Now()
	return database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetAttendanceForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		if row.Status != db.AttendanceStatusABSENT {
			return ErrNotFound // not a mangkir to undo; the same answer as one that does not exist
		}
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceAbsenceNew, dateValue(row.WorkDate))
		if err != nil {
			return err
		}
		if !res.HasTeam(row.TeamCode) {
			return ErrNotFound // outside the caller's reach: the same answer as not existing
		}
		if err := writeEvent(ctx, q, row, eventAbsentCancelled, now); err != nil {
			return err
		}
		if err := q.DeleteAttendance(ctx, id); err != nil {
			return fmt.Errorf("delete attendance: %w", err)
		}
		actorID := p.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "attendance.absent.delete", EntityType: "attendance", EntityID: id,
			Metadata: map[string]any{
				"personnel_id": row.PersonnelID, "team_code": row.TeamCode, "work_date": dateValue(row.WorkDate).String(),
			},
		})
	})
}

// ---------- the group summary (GET /attendances/group-summary) ----------

// GroupSummaryDay is one day of the regu's monthly summary.
type GroupSummaryDay struct {
	WorkDate  timeutil.Date
	Scheduled int
	Present   int
	Leave     int
	Absent    int
	Missing   int
	// Swapped is always 0 until 07.7 adds shift_swaps.
	Swapped int
}

// GroupSummary is GET /attendances/group-summary?team_code=&month=: the regu's per-day counts for
// a month (scheduled, present, leave, absent, missing, swapped), built from one GROUP BY query
// (db.Queries.GroupSummary), not a loop per day. Guarded the same way List is: a grant of
// attendance.group.view or attendance.absence.view must reach team_code.
func (s *Service) GroupSummary(ctx context.Context, p auth.Principal, teamCode, month string) ([]GroupSummaryDay, error) {
	first, last, err := timeutil.MonthRange(month)
	if err != nil {
		return nil, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "month", Code: "INVALID", Message: "month harus berformat YYYY-MM.",
		})
	}
	var out []GroupSummaryDay
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		teams, terr := s.reachableTeams(ctx, q, p.PersonnelID, timeutil.BusinessDate(s.clock.Now(), wib))
		if terr != nil {
			return terr
		}
		if !slices.Contains(teams, teamCode) {
			return access.ErrPermissionDenied
		}
		rows, rerr := q.GroupSummary(ctx, db.GroupSummaryParams{TeamCode: teamCode, DateFrom: dateParam(first), DateTo: dateParam(last)})
		if rerr != nil {
			return fmt.Errorf("group summary: %w", rerr)
		}
		out = make([]GroupSummaryDay, 0, len(rows))
		for _, r := range rows {
			out = append(out, GroupSummaryDay{
				WorkDate: dateValue(r.WorkDate), Scheduled: int(r.Scheduled), Present: int(r.Present),
				Leave: int(r.Leave), Absent: int(r.Absent), Missing: int(r.Missing),
			})
		}
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

// Page is a page of the admin monitor list (Part C's ListForAdmin).
type Page struct {
	Attendances []Attendance
	Total       int
}

// TeamAttendanceRow is one member's record on one day, GET /attendances' shape (the DANRU group
// view): who, which shift, what happened, not the full capture evidence the admin monitor and the
// detail endpoint show.
type TeamAttendanceRow struct {
	AttendanceID       int64
	PersonnelID        int64
	PersonnelName      string
	WorkDate           timeutil.Date
	ShiftName          string
	Status             string
	CheckInAt          *time.Time
	CheckInMethod      *Method
	CheckInWithinArea  *bool
	CheckOutAt         *time.Time
	CheckOutMethod     *Method
	CheckOutWithinArea *bool
}

// TeamPage is a page of the DANRU group view.
type TeamPage struct {
	Rows  []TeamAttendanceRow
	Total int
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

// List is GET /attendances: every regu's member-and-day within the caller's attendance.group.view
// or attendance.absence.view reach, newest first. f.TeamCode narrows to one of those regu; asking
// for one outside the reach is PERMISSION_DENIED, the same answer as asking for none at all.
func (s *Service) List(ctx context.Context, p auth.Principal, f Filter) (TeamPage, error) {
	var out TeamPage
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
		params := db.ListTeamAttendanceRowsParams{
			TeamCodes: teams, ProjectCode: textOrNull(f.ProjectCode),
			FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageOffset: int32(f.Offset), PageLimit: int32(f.Limit), //nolint:gosec // bounded by httpx.ParseListQuery
		}
		rows, err := q.ListTeamAttendanceRows(ctx, params)
		if err != nil {
			return fmt.Errorf("list team attendance rows: %w", err)
		}
		out.Rows = make([]TeamAttendanceRow, 0, len(rows))
		for _, r := range rows {
			out.Rows = append(out.Rows, TeamAttendanceRow{
				AttendanceID: r.ID, PersonnelID: r.PersonnelID, PersonnelName: r.FullName,
				WorkDate: dateValue(r.WorkDate), ShiftName: r.ShiftName, Status: string(r.Status),
				CheckInAt: timePtr(r.CheckInAt), CheckInMethod: methodPtr(r.CheckInMethod), CheckInWithinArea: boolPtr(r.CheckInWithinArea),
				CheckOutAt: timePtr(r.CheckOutAt), CheckOutMethod: methodPtr(r.CheckOutMethod), CheckOutWithinArea: boolPtr(r.CheckOutWithinArea),
			})
		}
		total, err := q.CountTeamAttendanceRows(ctx, db.CountTeamAttendanceRowsParams{
			TeamCodes: params.TeamCodes, ProjectCode: params.ProjectCode, FromDate: params.FromDate, ToDate: params.ToDate,
		})
		if err != nil {
			return fmt.Errorf("count team attendance rows: %w", err)
		}
		out.Total = int(total)
		return nil
	})
	return out, err
}

// listInto reads one page of the admin monitor list and its total (Part C's ListForAdmin).
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
