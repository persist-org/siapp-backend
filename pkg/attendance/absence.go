package attendance

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
//
// 07.7: a shift the person covers for someone else (an active swap in that regu) is theirs too, so
// it is found when the schedule has no such shift; swapID is then set and the record written for it
// carries the swap, like a check-in on it does.
func (s *Service) instanceFor(ctx context.Context, q *db.Queries, organizationID, personnelID int64, teamCode string, date timeutil.Date) (inst organization.ShiftInstance, projectCode string, swapID *int64, ok bool, err error) {
	instances, err := s.shifts.Instances(ctx, organizationID, personnelID, date, date)
	if err != nil {
		return organization.ShiftInstance{}, "", nil, false, fmt.Errorf("shift instances: %w", err)
	}
	for _, in := range instances {
		if in.IsOff || !slices.Contains(in.TeamCodes, teamCode) {
			continue
		}
		site, err := s.sites.TeamWithProject(ctx, organizationID, teamCode)
		if errors.Is(err, organization.ErrTeamNotFound) {
			return organization.ShiftInstance{}, "", nil, false, nil
		}
		if err != nil {
			return organization.ShiftInstance{}, "", nil, false, fmt.Errorf("team %s: %w", teamCode, err)
		}
		return in, site.Project.Code, nil, true, nil
	}
	in, projectCode, id, ok, err := s.coveredInstance(ctx, q, organizationID, personnelID, teamCode, date, 0)
	if err != nil || !ok {
		return organization.ShiftInstance{}, "", nil, false, err
	}
	return in, projectCode, &id, true, nil
}

// insertAbsent writes a fresh ABSENT record for personnelID's shift instance and its outbox event,
// inside the caller's transaction. swapID is set when the shift is one the person covers (07.7).
//
// ⚠ A place that already has a record is not the caller's to turn into an absence: a PRESENT row
// is ABSENCE_ALREADY_PRESENT (a guard who came to work), an existing LEAVE or ABSENT is
// NOT_ALLOWED. An admin correcting an existing record instead goes through Correct (Part C), which
// locks and updates the row rather than inserting one.
func (s *Service) insertAbsent(ctx context.Context, q *db.Queries, markedByID, personnelID int64, teamCode, projectCode string, inst organization.ShiftInstance, swapID *int64, reason string, now time.Time) (db.Attendance, error) {
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
		AbsentReason: textOrNull(reason), ShiftSwapID: int8Param(swapID), Now: ts(now),
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
	Date string `json:"date" validate:"required"`
	// Reason is required (mobile spec ATT §6.3).
	Reason string `json:"reason" validate:"required,max=500"`
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
	if strings.TrimSpace(in.Reason) == "" {
		return Attendance{}, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "reason", Code: "REQUIRED", Message: "Alasan wajib diisi.",
		})
	}
	now := s.clock.Now()
	var out Attendance
	var notice Notice
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceAbsenceNew, date)
		if err != nil {
			return err
		}
		if !res.HasTeam(in.TeamCode) {
			return access.ErrPermissionDenied
		}
		inst, projectCode, swapID, ok, err := s.instanceFor(ctx, q, p.OrganizationID, in.PersonnelID, in.TeamCode, date)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNoShift
		}
		// ⚠ Not before the shift starts. A mangkir on a shift still to come would sit there, send
		// the guard a notice, and turn their own check-in away (a record already exists: NOT_ALLOWED).
		if now.Before(inst.StartsAt) {
			return ErrAbsenceTooEarly
		}
		row, err := s.insertAbsent(ctx, q, p.PersonnelID, in.PersonnelID, in.TeamCode, projectCode, inst, swapID, in.Reason, now)
		if err != nil {
			return err
		}
		out = toAttendance(row)
		if notice, err = s.absenceNotice(ctx, q, p.PersonnelID, row); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	s.notify(ctx, notice) // after commit: the person is told of a mangkir that is on record
	return out, nil
}

// absenceNotice is the ABSENCE_MARKED notice to the person marked absent, naming the DANRU.
func (s *Service) absenceNotice(ctx context.Context, q *db.Queries, markedByID int64, row db.Attendance) (Notice, error) {
	n, err := s.noticeBase(ctx, q, NoticeAbsenceMarked, row.TeamCode, row.ProjectCode, dateValue(row.WorkDate))
	if err != nil || s.notifier == nil {
		return Notice{}, err
	}
	n.RecipientIDs = []int64{row.PersonnelID}
	n.EntityType, n.EntityID = entityType, row.ID
	if n.ActorName, err = s.personName(ctx, q, markedByID); err != nil {
		return Notice{}, err
	}
	return n, nil
}

// CancelAbsent is DELETE /attendances/absent/{id}: the DANRU undoing their own mistaken mangkir (one
// they marked themselves, not an admin's correction).
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
		// ⚠ Only the mangkir the caller marked: one an admin set through a correction is the admin's.
		if !row.MarkedAbsentByID.Valid || row.MarkedAbsentByID.Int64 != p.PersonnelID {
			return ErrNotFound
		}
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceAbsenceNew, dateValue(row.WorkDate))
		if err != nil {
			return err
		}
		if !res.HasTeam(row.TeamCode) {
			return ErrNotFound // outside the caller's reach: the same answer as not existing
		}
		// A covered mangkir: cancel its swap first (the record is going away), or refuse when the
		// replacer has already worked the shift.
		if err := releaseSwaps(ctx, q, id, now); err != nil {
			return err
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
	// Swapped is a LEAVE/ABSENT record with an active shift swap (07.7): covered, not missing.
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
		teams, _, terr := s.reachableTeams(ctx, q, p.PersonnelID, timeutil.BusinessDate(s.clock.Now(), wib))
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
				Leave: int(r.Leave), Absent: int(r.Absent), Missing: int(r.Missing), Swapped: int(r.Swapped),
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
	// Status is PRESENT, LEAVE or ABSENT (the stored statuses); "" keeps all.
	Status string
	// SyncStatus is PENDING, SYNCED or FAILED, the admin monitor only; "" keeps all.
	SyncStatus string
	From, To   *timeutil.Date
	Limit      int
	Offset     int
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
// simplification pkg/leave's admin list makes). own is true when the caller holds either permission
// at all, even as SELF only (a MEMBER's own mangkir history): their own records are then readable
// whatever regu they are in.
func (s *Service) reachableTeams(ctx context.Context, q *db.Queries, personnelID int64, on timeutil.Date) (teams []string, own bool, err error) {
	for _, perm := range []access.Permission{access.AttendanceGroupView, access.AttendanceAbsenceView} {
		res, err := s.access.Resolve(ctx, q, personnelID, perm, on)
		if err != nil {
			return nil, false, err
		}
		own = own || res.Allowed
		for _, t := range res.TeamCodes {
			if !slices.Contains(teams, t) {
				teams = append(teams, t)
			}
		}
	}
	return teams, own, nil
}

// List is GET /attendances: every regu's member-and-day within the caller's attendance.group.view
// or attendance.absence.view reach, newest first. f.TeamCode narrows to one of those regu; asking
// for one outside the reach is PERMISSION_DENIED, the same answer as asking for none at all.
func (s *Service) List(ctx context.Context, p auth.Principal, f Filter) (TeamPage, error) {
	var out TeamPage
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		teams, own, err := s.reachableTeams(ctx, q, p.PersonnelID, timeutil.BusinessDate(s.clock.Now(), wib))
		if err != nil {
			return err
		}
		// ⚠ No permission at all: refused. A caller with only a SELF grant is let through and sees
		// their own records only (the SQL scope is "a reached regu OR my own"), so for them a
		// team_code outside their reach is an answer limited to their own records, never a probe of
		// that regu. A caller with a wider grant asking for a regu outside it is refused.
		if len(teams) == 0 && !own {
			return access.ErrPermissionDenied
		}
		if f.TeamCode != "" && !slices.Contains(teams, f.TeamCode) && len(teams) > 0 {
			return access.ErrPermissionDenied
		}
		var selfID pgtype.Int8
		if own {
			selfID = pgtype.Int8{Int64: p.PersonnelID, Valid: true}
		}
		params := db.ListTeamAttendanceRowsParams{
			TeamCodes: teams, SelfID: selfID, TeamCode: textOrNull(f.TeamCode), ProjectCode: textOrNull(f.ProjectCode),
			Status: textOrNull(f.Status), FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageOffset: int32(f.Offset), PageLimit: int32(f.Limit), //nolint:gosec // bounded by httpx.ParseListQuery
		}
		rows, err := q.ListTeamAttendanceRows(ctx, params)
		if err != nil {
			return fmt.Errorf("list team attendance rows: %w", err)
		}
		out.Rows = make([]TeamAttendanceRow, 0, len(rows))
		for _, r := range rows {
			status := string(r.Status)
			if r.Swapped && f.Status == "" {
				status = StatusSwapped // a covered LEAVE/ABSENT; filtering by status shows the stored one
			}
			out.Rows = append(out.Rows, TeamAttendanceRow{
				AttendanceID: r.ID, PersonnelID: r.PersonnelID, PersonnelName: r.FullName,
				WorkDate: dateValue(r.WorkDate), ShiftName: r.ShiftName, Status: status,
				CheckInAt: timePtr(r.CheckInAt), CheckInMethod: methodPtr(r.CheckInMethod), CheckInWithinArea: boolPtr(r.CheckInWithinArea),
				CheckOutAt: timePtr(r.CheckOutAt), CheckOutMethod: methodPtr(r.CheckOutMethod), CheckOutWithinArea: boolPtr(r.CheckOutWithinArea),
			})
		}
		total, err := q.CountTeamAttendanceRows(ctx, db.CountTeamAttendanceRowsParams{
			TeamCodes: params.TeamCodes, SelfID: params.SelfID, TeamCode: params.TeamCode, ProjectCode: params.ProjectCode,
			Status: params.Status, FromDate: params.FromDate, ToDate: params.ToDate,
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
		RegionCode: params.RegionCode, Status: params.Status, SyncStatus: params.SyncStatus,
		FromDate: params.FromDate, ToDate: params.ToDate,
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
