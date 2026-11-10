package attendance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/organization"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Phase 07.4: the guard's monthly calendar and the detail of one record.

// The two statuses of the calendar that are computed, never stored: a scheduled shift without a
// record is MISSING once it has ended and UPCOMING until then. The stored statuses (PRESENT, LEAVE,
// ABSENT) come from the record.
const (
	StatusMissing  = "MISSING"
	StatusUpcoming = "UPCOMING"
	// StatusSwapped is what the DANRU's group view shows for a LEAVE or ABSENT record that has an
	// active shift swap (07.7): somebody else covers it. Never stored.
	StatusSwapped = "SWAPPED"
)

// HistoryShift is one shift of one day of the calendar, in one regu.
type HistoryShift struct {
	// AttendanceID is nil while the shift has no record (MISSING and UPCOMING).
	AttendanceID *int64
	ShiftName    string
	TeamCode     string
	TeamName     string
	// Status is the record's status, or StatusMissing / StatusUpcoming.
	Status        string
	CheckInAt     *time.Time
	CheckInMethod *Method
	CheckOutAt    *time.Time
	// ShiftSwapID is set when the shift is one the person covers for someone else (07.7).
	ShiftSwapID *int64

	startsAt time.Time
}

// HistoryDay is the shifts of one work date.
type HistoryDay struct {
	WorkDate timeutil.Date
	Shifts   []HistoryShift
}

// Month returns the caller's calendar for a "YYYY-MM" month: the scheduled shifts merged with the
// records that exist, one entry per shift per regu, days with no working shift left out.
//
// ⚠ Two sources are merged, neither alone is the calendar. The schedule says what the person was
// supposed to work (organization.Shifts.Instances, OFF days skipped); the records say what happened.
// A scheduled shift takes its status from its record when there is one; without one it is MISSING
// when it has ended and UPCOMING otherwise. MISSING and UPCOMING are computed on every read and
// stored nowhere: they are the absence of a record, and a record written later (a late "mangkir", an
// admin correction) must replace them without a migration of old rows. A record whose shift is no
// longer on the schedule (the schedule was changed after the person checked in, or an approved leave
// covers a day that was since rescheduled) is still shown: what was captured is never hidden.
//
// An overnight shift is on the date it STARTS, like its record's work_date.
//
// This is the field view of what SiAPP captured. HR corrections made directly in Personel are not
// reflected here; the official totals are Personel's (module 14 reads them back).
func (s *Service) Month(ctx context.Context, p auth.Principal, month string) ([]HistoryDay, error) {
	first, last, err := timeutil.MonthRange(month)
	if err != nil {
		return nil, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "month", Code: "INVALID", Message: "month harus berformat YYYY-MM.",
		})
	}
	now := s.clock.Now()

	instances, err := s.shifts.Instances(ctx, p.OrganizationID, p.PersonnelID, first, last)
	if err != nil {
		return nil, fmt.Errorf("month instances: %w", err)
	}

	var rows []db.Attendance
	var swaps []db.ShiftSwap
	shiftNames := map[int64]string{}
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		var err error
		rows, err = q.ListAttendancesOfPersonnelBetween(ctx, db.ListAttendancesOfPersonnelBetweenParams{
			PersonnelID: p.PersonnelID, DateFrom: dateParam(first), DateTo: dateParam(last),
		})
		if err != nil {
			return fmt.Errorf("list attendances: %w", err)
		}
		// 07.7: shifts the person covers for someone else are part of their month.
		if swaps, err = q.ListActiveSwapsOfReplacer(ctx, db.ListActiveSwapsOfReplacerParams{
			PersonnelID: p.PersonnelID, DateFrom: dateParam(first), DateTo: dateParam(last),
		}); err != nil {
			return fmt.Errorf("list swaps: %w", err)
		}
		ids := make([]int64, 0, len(rows)+len(swaps))
		for _, r := range rows {
			ids = append(ids, r.ShiftID)
		}
		for _, sw := range swaps {
			ids = append(ids, sw.ShiftID)
		}
		if len(ids) == 0 {
			return nil
		}
		shifts, err := q.GetShiftsByIDs(ctx, ids)
		if err != nil {
			return fmt.Errorf("shift names: %w", err)
		}
		for _, sh := range shifts {
			shiftNames[sh.ID] = sh.Name
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	records := make(map[shiftKey]Attendance, len(rows))
	for _, r := range rows {
		records[shiftKey{TeamCode: r.TeamCode, WorkDate: dateValue(r.WorkDate), ShiftID: r.ShiftID}] = toAttendance(r)
	}

	teamNames := map[string]string{}
	teamName := func(code string) (string, error) {
		if name, ok := teamNames[code]; ok {
			return name, nil
		}
		site, err := s.sites.TeamWithProject(ctx, p.OrganizationID, code)
		name := code // a regu deleted since: the code is all that is left to show
		switch {
		case err == nil:
			name = site.Team.Description
		case !errors.Is(err, organization.ErrTeamNotFound):
			return "", fmt.Errorf("team %s: %w", code, err)
		}
		teamNames[code] = name
		return name, nil
	}

	byDay := map[timeutil.Date][]HistoryShift{}
	shown := map[shiftKey]bool{}
	for _, inst := range instances {
		if inst.IsOff {
			continue
		}
		for _, code := range inst.TeamCodes {
			key := shiftKey{TeamCode: code, WorkDate: inst.WorkDate, ShiftID: inst.ShiftID}
			name, err := teamName(code)
			if err != nil {
				return nil, err
			}
			entry := HistoryShift{ShiftName: inst.ShiftName, TeamCode: code, TeamName: name, startsAt: inst.StartsAt}
			if rec, ok := records[key]; ok {
				entry.fill(rec)
				shown[key] = true
			} else if now.After(inst.EndsAt) {
				entry.Status = StatusMissing
			} else {
				entry.Status = StatusUpcoming
			}
			byDay[inst.WorkDate] = append(byDay[inst.WorkDate], entry)
		}
	}
	for key, rec := range records {
		if shown[key] {
			continue
		}
		name, err := teamName(key.TeamCode)
		if err != nil {
			return nil, err
		}
		entry := HistoryShift{ShiftName: shiftNames[key.ShiftID], TeamCode: key.TeamCode, TeamName: name, startsAt: rec.StartsAt}
		entry.fill(rec)
		byDay[key.WorkDate] = append(byDay[key.WorkDate], entry)
	}

	// A covered shift whose replacer has not checked in yet has no record: MISSING once it has ended,
	// UPCOMING until then, like a scheduled one. With a record it is already in the loop above (its
	// record carries the swap).
	for _, sw := range swaps {
		key := shiftKey{TeamCode: sw.TeamCode, WorkDate: dateValue(sw.WorkDate), ShiftID: sw.ShiftID}
		if _, ok := records[key]; ok {
			continue
		}
		name, err := teamName(sw.TeamCode)
		if err != nil {
			return nil, err
		}
		id := sw.ID
		entry := HistoryShift{ShiftName: shiftNames[sw.ShiftID], TeamCode: sw.TeamCode, TeamName: name, ShiftSwapID: &id, startsAt: sw.StartsAt.Time}
		if now.After(sw.EndsAt.Time) {
			entry.Status = StatusMissing
		} else {
			entry.Status = StatusUpcoming
		}
		byDay[key.WorkDate] = append(byDay[key.WorkDate], entry)
	}

	out := make([]HistoryDay, 0, len(byDay))
	for day, shifts := range byDay {
		slices.SortStableFunc(shifts, func(a, b HistoryShift) int {
			if c := a.startsAt.Compare(b.startsAt); c != 0 {
				return c
			}
			return strings.Compare(a.TeamCode, b.TeamCode)
		})
		out = append(out, HistoryDay{WorkDate: day, Shifts: shifts})
	}
	slices.SortFunc(out, func(a, b HistoryDay) int { return a.WorkDate.Time().Compare(b.WorkDate.Time()) })
	return out, nil
}

func (h *HistoryShift) fill(rec Attendance) {
	id := rec.ID
	h.AttendanceID, h.Status = &id, string(rec.Status)
	h.CheckInAt, h.CheckInMethod, h.CheckOutAt = rec.CheckInAt, rec.CheckInMethod, rec.CheckOutAt
	h.ShiftSwapID = rec.ShiftSwapID
}

// Detail is one record with what the detail screen shows around it.
type Detail struct {
	Attendance  Attendance
	ProjectName string
	TeamName    string
	ShiftName   string
	// DanruNames are the regu's DANRU on the work date and the DANRU whose QR was scanned, each once.
	DanruNames []string
	// The selfies are nil when the capture had none (QR) or the photo is no longer viewable.
	CheckInSelfie  *media.View
	CheckOutSelfie *media.View
}

// Detail is GET /attendances/{id}: allowed for the owner, or for anyone whose group or absence view
// reaches the record's regu on its work date (a DANRU of the regu, or a project's DANRU with a PROJECT
// grant). Anyone else gets ErrNotFound, the same answer as for a record that does not exist or
// belongs to another organization, so ids cannot be probed.
//
// ⚠ The photos are embedded here and nowhere else: media has no by-id endpoint (media.Views), so the
// decision of who may see a selfie is this one, made above.
func (s *Service) Detail(ctx context.Context, p auth.Principal, id int64) (Detail, error) {
	var (
		out  Detail
		row  db.Attendance
		code string
	)
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		var err error
		if row, err = q.GetAttendance(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		if row.PersonnelID != p.PersonnelID {
			if ok, err := s.canSee(ctx, q, p.PersonnelID, row); err != nil {
				return err
			} else if !ok {
				return ErrNotFound
			}
		}
		out.Attendance = toAttendance(row)
		code = row.TeamCode

		if sh, err := q.GetShift(ctx, row.ShiftID); err == nil {
			out.ShiftName = sh.Name
		} else if !errors.Is(err, pgx.ErrNoRows) { // a deleted shift keeps its record, not its name
			return fmt.Errorf("get shift: %w", err)
		}

		if out.DanruNames, err = s.danruNames(ctx, q, row); err != nil {
			return err
		}

		var photoIDs []int64
		for _, m := range []int64{row.CheckInMediaID.Int64, row.CheckOutMediaID.Int64} {
			if m > 0 {
				photoIDs = append(photoIDs, m)
			}
		}
		views, err := s.photos.Views(ctx, q, entityType, row.ID, photoIDs)
		if err != nil {
			return fmt.Errorf("selfie views: %w", err)
		}
		for i := range views {
			switch views[i].ID {
			case row.CheckInMediaID.Int64:
				out.CheckInSelfie = &views[i]
			case row.CheckOutMediaID.Int64:
				out.CheckOutSelfie = &views[i]
			}
		}
		return nil
	})
	if err != nil {
		return Detail{}, err
	}

	out.TeamName, out.ProjectName = code, row.ProjectCode
	site, err := s.sites.TeamWithProject(ctx, p.OrganizationID, code)
	switch {
	case err == nil:
		out.TeamName, out.ProjectName = site.Team.Description, site.Project.Description
	case !errors.Is(err, organization.ErrTeamNotFound):
		return Detail{}, fmt.Errorf("team %s: %w", code, err)
	}
	return out, nil
}

// canSee is whether someone other than the owner may read the record: a grant of the group view or
// of the absence view that reaches the record's regu on its work date. A MEMBER holds both only as
// SELF, which reaches no regu.
func (s *Service) canSee(ctx context.Context, q *db.Queries, personnelID int64, row db.Attendance) (bool, error) {
	for _, perm := range []access.Permission{access.AttendanceGroupView, access.AttendanceAbsenceView} {
		res, err := s.access.Resolve(ctx, q, personnelID, perm, dateValue(row.WorkDate))
		if err != nil {
			return false, err
		}
		if res.HasTeam(row.TeamCode) {
			return true, nil
		}
	}
	return false, nil
}

// danruNames returns the names of the DANRU behind a record: the regu's DANRU on the work date, then
// the DANRU whose QR code was scanned to check in or out (the same person, usually, but a PROJECT
// DANRU can show another regu's code), each once.
func (s *Service) danruNames(ctx context.Context, q *db.Queries, row db.Attendance) ([]string, error) {
	names := []string{}
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	d, err := q.GetTeamDanruOn(ctx, db.GetTeamDanruOnParams{TeamCode: row.TeamCode, OnDate: row.WorkDate})
	if err == nil {
		add(d.FullName)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get danru: %w", err)
	}
	for _, id := range []int64{row.CheckInQrDanruID.Int64, row.CheckOutQrDanruID.Int64} {
		if id < 1 {
			continue
		}
		person, err := q.GetPersonnel(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("get qr danru: %w", err)
		}
		add(person.FullName)
	}
	return names, nil
}
