package leave

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Status is where a request stands.
type Status = db.LeaveRequestStatus

// The statuses of a request. REJECTED and CANCELLED are final.
const (
	StatusPending   = db.LeaveRequestStatusPENDING
	StatusApproved  = db.LeaveRequestStatusAPPROVED
	StatusRejected  = db.LeaveRequestStatusREJECTED
	StatusCancelled = db.LeaveRequestStatusCANCELLED
)

// entityType is how media names a request's attachment.
const entityType = "LEAVE_REQUEST"

// maxRangeDays is the longest range one request may cover. The working days are counted from the
// schedule of the range, so an unbounded one is an unbounded read.
const maxRangeDays = 366

// Request number prefixes, one per type (KPI: LR, PR, SR).
var numberPrefix = map[Type]string{TypeLeave: "LR", TypePermit: "PR", TypeSick: "SR"}

// SubmitInput is a request to take leave, a permit or sick leave. The days are inclusive.
type SubmitInput struct {
	// TeamCode may be left out when the person serves in one regu on StartDate.
	TeamCode string `json:"team_code" validate:"omitempty,max=40"`
	Type     string `json:"type" validate:"required,oneof=LEAVE PERMIT SICK"`
	// CategoryCode may be left out for SICK, which then uses the SICK category.
	CategoryCode string        `json:"category_code" validate:"omitempty,max=40"`
	StartDate    timeutil.Date `json:"start_date"`
	EndDate      timeutil.Date `json:"end_date"`
	// StartTime and EndTime ("HH:MM", the project's local time) go together, and only categories
	// that are a part of a day take them.
	StartTime       *string        `json:"start_time"`
	EndTime         *string        `json:"end_time"`
	Reason          string         `json:"reason" validate:"required,notblank,max=500"`
	Responsibility  *string        `json:"responsibility" validate:"omitempty,max=255"`
	ContactInfo     *string        `json:"contact_info" validate:"omitempty,max=255"`
	ChangeDayFrom   *timeutil.Date `json:"change_day_from"`
	ChangeDayTo     *timeutil.Date `json:"change_day_to"`
	Hospitalization *bool          `json:"hospitalization"`
	LetterStatus    *bool          `json:"letter_status"`
	LetterDate      *timeutil.Date `json:"letter_date"`
	Disease         *string        `json:"disease" validate:"omitempty,max=500"`
	MediaIDs        []int64        `json:"media_ids" validate:"omitempty,max=1,dive,min=1"`
}

// Decision is who decided a request, and when.
type Decision struct {
	// Level is DANRU or ADMIN.
	Level string
	// ByName is the first name of the person who decided, as KPI shows "Disetujui oleh {nama}".
	ByName string
	At     time.Time
	Note   string
}

// Sick is what a sick request adds.
type Sick struct {
	Hospitalization bool
	LetterStatus    bool
	LetterDate      *timeutil.Date
	Disease         string
}

// Request is a request as the app shows it.
type Request struct {
	ID           int64
	Number       string
	PersonnelID  int64
	TeamCode     string
	ProjectCode  string
	Type         Type
	CategoryCode string
	CategoryName string
	Status       Status
	StartDate    timeutil.Date
	EndDate      timeutil.Date
	// StartTime and EndTime are "HH:MM", nil for a request that is not a part of a day.
	StartTime      *string
	EndTime        *string
	WorkingDays    int
	Reason         string
	Responsibility *string
	ContactInfo    *string
	ChangeDayFrom  *timeutil.Date
	ChangeDayTo    *timeutil.Date
	RequestedAt    time.Time
	Decision       *Decision
	Sick           *Sick
	Attachment     *media.View
	// CanCancel is true while the owner may still cancel: the request is waiting.
	CanCancel bool

	// What follows is filled for someone who decides the request, not for its owner.
	Person       *Person
	OverlapsWith []Overlap
	Coverage     *Coverage
	Balance      *AnnualBalance
	// Skipped lists the leave days an approval did not write because the person was present.
	Skipped []SkippedDay
}

func clockOf(t pgtype.Time) *string {
	if !t.Valid {
		return nil
	}
	minutes := t.Microseconds / int64(time.Minute/time.Microsecond)
	s := fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
	return &s
}

func clockParam(s *string) (pgtype.Time, bool) {
	if s == nil {
		return pgtype.Time{}, true
	}
	t, err := time.Parse("15:04", *s)
	if err != nil {
		if t, err = time.Parse("15:04:05", *s); err != nil {
			return pgtype.Time{}, false
		}
	}
	return pgtype.Time{Microseconds: int64(t.Hour()*60+t.Minute()) * int64(time.Minute/time.Microsecond), Valid: true}, true
}

func dateValue(d pgtype.Date) timeutil.Date {
	var out timeutil.Date
	_ = out.ScanDate(d)
	return out
}

func datePtr(d pgtype.Date) *timeutil.Date {
	if !d.Valid {
		return nil
	}
	v := dateValue(d)
	return &v
}

func dateParamPtr(d *timeutil.Date) pgtype.Date {
	if d == nil || d.IsZero() {
		return pgtype.Date{}
	}
	return dateParam(*d)
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return ""
}

// ---------- validation ----------

// fieldErrors collects what is wrong with a request, one entry per field.
type fieldErrors []apperr.FieldError

func (f *fieldErrors) add(field, message string) {
	*f = append(*f, apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return apperr.ErrValidation.WithFields(f...)
}

// validateSubmit checks a request against the rules of its category (KPI handler.go:629-794). The
// category's flags decide which fields it needs and which it refuses: nothing is keyed on a code.
func validateSubmit(in SubmitInput, cat Category) error {
	var f fieldErrors
	switch {
	case in.StartDate.IsZero():
		f.add("start_date", "Wajib diisi (YYYY-MM-DD).")
	case in.EndDate.IsZero():
		f.add("end_date", "Wajib diisi (YYYY-MM-DD).")
	case in.EndDate.Before(in.StartDate):
		f.add("end_date", "Tidak boleh sebelum tanggal mulai.")
	case in.EndDate.Time().Sub(in.StartDate.Time()) > time.Duration(maxRangeDays)*24*time.Hour:
		f.add("end_date", fmt.Sprintf("Rentang tanggal paling lama %d hari.", maxRangeDays))
	}

	start, okStart := clockParam(in.StartTime)
	end, okEnd := clockParam(in.EndTime)
	switch {
	case !okStart:
		f.add("start_time", "Harus berformat HH:MM.")
	case !okEnd:
		f.add("end_time", "Harus berformat HH:MM.")
	case start.Valid != end.Valid:
		f.add("end_time", "Jam mulai dan jam selesai harus diisi bersama.")
	case cat.RequiresTimeRange && !start.Valid:
		f.add("start_time", "Kategori ini wajib mengisi jam mulai dan jam selesai.")
	case !cat.RequiresTimeRange && start.Valid:
		f.add("start_time", "Kategori ini tidak memakai jam.")
	case start.Valid && in.StartDate.String() == in.EndDate.String() && end.Microseconds <= start.Microseconds:
		f.add("end_time", "Jam selesai harus setelah jam mulai.")
	}

	switch {
	case cat.RequiresChangeDay && (in.ChangeDayFrom == nil || in.ChangeDayTo == nil):
		f.add("change_day_from", "Kategori ini wajib mengisi tanggal tukar hari.")
	case !cat.RequiresChangeDay && (in.ChangeDayFrom != nil || in.ChangeDayTo != nil):
		f.add("change_day_from", "Kategori ini tidak memakai tanggal tukar hari.")
	case in.ChangeDayFrom != nil && in.ChangeDayTo.Before(*in.ChangeDayFrom):
		f.add("change_day_to", "Tidak boleh sebelum tanggal awal tukar hari.")
	}

	if cat.Type == TypeSick {
		if in.Hospitalization == nil {
			f.add("hospitalization", "Wajib diisi untuk izin sakit.")
		}
		if in.LetterStatus == nil {
			f.add("letter_status", "Wajib diisi untuk izin sakit.")
		} else if *in.LetterStatus && len(in.MediaIDs) != 1 {
			f.add("media_ids", "Surat dokter wajib dilampirkan (satu foto).")
		}
	} else if in.Hospitalization != nil || in.LetterStatus != nil || in.LetterDate != nil || in.Disease != nil {
		f.add("hospitalization", "Isian sakit hanya untuk izin sakit.")
	}
	return f.err()
}

// ---------- working days ----------

// workingDays counts the days of [start, end] that use quota (KPI leaveday.go).
//
// The schedule rows of the range decide it. None at all: every calendar day counts (someone with
// no schedule yet can still ask). Otherwise a date with no row is a gap, and the request is
// refused naming the dates, so nobody takes leave on a day nobody knows if they work; the days are
// the dates that are not the day off. A range of day-offs only has none: LEAVE_NO_WORKING_DAYS.
//
// ⚠ SiAPP has no holiday table (deviation 6): a holiday is a day off in the schedule.
func workingDays(days []ScheduleDay, start, end timeutil.Date) (int, error) {
	calendar := int(end.Time().Sub(start.Time())/(24*time.Hour)) + 1
	if len(days) == 0 {
		return calendar, nil
	}
	have := make(map[string]bool, len(days))
	count := 0
	for _, d := range days {
		have[d.Date.String()] = true
		if !d.Off {
			count++
		}
	}
	var missing []string
	for d := start; !d.After(end); d = d.AddDays(1) {
		if !have[d.String()] {
			missing = append(missing, d.String())
		}
	}
	if len(missing) > 0 {
		return 0, scheduleMissingError(missing)
	}
	if count == 0 {
		return 0, errNoWorkingDays
	}
	return count, nil
}

// ---------- overlap ----------

// overlapDate is the first day the new request cannot be made on, or "" when it fits (KPI
// overlap.go). Candidates are the person's live requests (pending or approved) of any type that
// share a day with it. A "part of day" request is one whose category requires a time range.
//
//   - A normal request conflicts with any candidate.
//   - A part-of-day request conflicts with a normal candidate.
//   - Two part-of-day requests conflict when their windows clash on a day both cover. (KPI checks
//     the windows only when the old range wholly contains the new one and calls the rest a legacy
//     gap; SiAPP checks every shared day.)
//   - Two part-of-day candidates already on the days conflict with anything (KPI's shiftCount > 1).
func overlapDate(rows []db.ListLiveLeaveRequestsOverlappingRow, cat Category, start timeutil.Date, from, to pgtype.Time) string {
	if len(rows) == 0 {
		return ""
	}
	first := func(r db.ListLiveLeaveRequestsOverlappingRow) string {
		s := dateValue(r.StartDate)
		if start.After(s) {
			s = start
		}
		return s.String()
	}
	timed := 0
	for _, r := range rows {
		if r.RequiresTimeRange {
			timed++
		}
	}
	if timed > 1 {
		return first(rows[0])
	}
	for _, r := range rows {
		if !cat.RequiresTimeRange || !r.RequiresTimeRange {
			return first(r) // a normal request on either side shares the whole day
		}
		// Both are part of a day: they clash when the windows do (oldStart < newEnd && oldEnd > newStart).
		if r.StartTime.Microseconds < to.Microseconds && r.EndTime.Microseconds > from.Microseconds {
			return first(r)
		}
	}
	return ""
}

// ---------- submit ----------

// Submit makes a request. One transaction, in KPI's order (service.go:512-740), so the answer a
// person gets for a request that is wrong in two ways is the same as KPI's: the person is locked,
// the regu and its DANRU are resolved, then the overlap, then the working days and the quota,
// then the number and the row, the photo, and for annual leave the deduction.
//
// ⚠ A request of annual leave takes its days out of the balance NOW, not when it is approved, and
// gives them back when it is rejected or cancelled (ledger.go). A request of any other category
// reserves nothing: the quota is counted from the requests there are (AllowanceFor).
func (s *Service) Submit(ctx context.Context, p auth.Principal, in SubmitInput) (Request, error) {
	cat, err := s.categoryForSubmit(ctx, p, in)
	if err != nil {
		return Request{}, err
	}
	if err := validateSubmit(in, cat); err != nil {
		return Request{}, err
	}
	from, _ := clockParam(in.StartTime)
	to, _ := clockParam(in.EndTime)

	// The schedule is read before the transaction: it asks another module, which opens its own.
	// What it says is checked in its turn below, after the overlap, as KPI does.
	days, wdErr := 0, error(nil)
	if cat.HasQuota {
		var sched []ScheduleDay
		if sched, wdErr = s.scheduleOf(ctx, p, in.StartDate, in.EndDate); wdErr == nil {
			days, wdErr = workingDays(sched, in.StartDate, in.EndDate)
		}
	}

	var (
		id     int64
		notice *Notice
	)
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		// 1. The person's row lock: their balance and their requests are one queue.
		if _, err := q.LockPersonnel(ctx, p.PersonnelID); err != nil {
			return fmt.Errorf("lock personnel: %w", err)
		}
		// 2. The regu, and who will decide (the admins when there is nobody).
		team, err := s.teamOn(ctx, q, p.PersonnelID, in.TeamCode, in.StartDate)
		if err != nil {
			return err
		}
		var approver pgtype.Int8
		danru, err := q.GetTeamDanruOn(ctx, db.GetTeamDanruOnParams{TeamCode: team.TeamCode, OnDate: dateParam(in.StartDate)})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get danru: %w", err)
		}
		if err == nil && danru.PersonnelID != p.PersonnelID {
			approver = pgInt8(danru.PersonnelID)
		}
		// 3. Overlap.
		live, err := q.ListLiveLeaveRequestsOverlapping(ctx, db.ListLiveLeaveRequestsOverlappingParams{
			PersonnelID: p.PersonnelID, StartDate: dateParam(in.StartDate), EndDate: dateParam(in.EndDate),
		})
		if err != nil {
			return fmt.Errorf("list overlapping requests: %w", err)
		}
		if d := overlapDate(live, cat, in.StartDate, from, to); d != "" {
			return overlapError(d)
		}
		// 4. Working days (counted above), then the quota.
		if wdErr != nil {
			return wdErr
		}
		if cat.HasQuota {
			if err := s.checkQuota(ctx, q, p.PersonnelID, cat, team.TeamCode, in.StartDate, days); err != nil {
				return err
			}
		}
		// 5. The number, the row, the photo.
		now := s.clock.Now()
		_, loc, err := clientAndZone(ctx, q, team.TeamCode)
		if err != nil {
			return err
		}
		counter, err := q.NextLeaveRequestCounter(ctx, cat.Type)
		if err != nil {
			return fmt.Errorf("next leave request counter: %w", err)
		}
		number := fmt.Sprintf("%s%s%05d", numberPrefix[cat.Type], now.In(loc).Format("200601"), counter)
		row, err := q.InsertLeaveRequest(ctx, db.InsertLeaveRequestParams{
			Number: number, PersonnelID: p.PersonnelID, TeamCode: team.TeamCode, ProjectCode: team.ProjectCode,
			Type: cat.Type, CategoryID: cat.ID, StartDate: dateParam(in.StartDate), EndDate: dateParam(in.EndDate),
			StartTime: from, EndTime: to, WorkingDays: int32(days), //nolint:gosec // at most 366
			Reason: in.Reason, Responsibility: textPtrOrNull(in.Responsibility), ContactInfo: textPtrOrNull(in.ContactInfo),
			ChangeDayFrom: dateParamPtr(in.ChangeDayFrom), ChangeDayTo: dateParamPtr(in.ChangeDayTo),
			Hospitalization: boolOrNull(in.Hospitalization), LetterStatus: boolOrNull(in.LetterStatus),
			LetterDate: dateParamPtr(in.LetterDate), Disease: textPtrOrNull(in.Disease),
			ApproverPersonnelID: approver, Now: ts(now),
		})
		if err != nil {
			return fmt.Errorf("insert leave request: %w", err)
		}
		id = row.ID
		if len(in.MediaIDs) > 0 {
			if err := s.files.Attach(ctx, q, media.AttachInput{
				MediaIDs: in.MediaIDs, OwnerID: p.PersonnelID, Purpose: media.PurposeLeaveAttachment,
				EntityType: entityType, EntityID: row.ID, Max: 1,
			}); err != nil {
				return err
			}
			if err := q.SetLeaveRequestAttachment(ctx, db.SetLeaveRequestAttachmentParams{ID: row.ID, MediaID: pgInt8(in.MediaIDs[0])}); err != nil {
				return fmt.Errorf("set leave request attachment: %w", err)
			}
		}
		// 6. Annual leave: the days leave the balance now.
		if cat.IsAnnual {
			if _, err := Deduct(ctx, q, now, p.PersonnelID, row.ID, days, "Pengajuan "+number); errors.Is(err, ErrBalanceInsufficient) {
				return quotaExceededError("Saldo cuti tidak mencukupi.")
			} else if err != nil {
				return err
			}
		}
		// 7. Audit, and the notice to the DANRU (or the admins), sent after the commit.
		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.request.submit", EntityType: "leave_request", EntityID: row.ID,
			Metadata: map[string]any{"number": number, "category": cat.Code, "days": days},
		}); err != nil {
			return err
		}
		notice, err = s.noticeFor(ctx, q, NoticeRequested, row, false)
		return err
	})
	if err != nil {
		return Request{}, err
	}
	s.notify(ctx, notice)
	return s.Get(ctx, p, id)
}

// categoryForSubmit finds the category a request is for, and refuses one of the wrong type or one
// that is switched off. A sick request without a category uses SICK.
func (s *Service) categoryForSubmit(ctx context.Context, p auth.Principal, in SubmitInput) (Category, error) {
	code := in.CategoryCode
	if code == "" && Type(in.Type) == TypeSick {
		code = "SICK"
	}
	if code == "" {
		return Category{}, validationError("category_code", "Wajib diisi.")
	}
	var cat Category
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := categoryByCode(ctx, q, code)
		cat = toCategory(row)
		return err
	})
	if err != nil {
		return Category{}, err
	}
	if !cat.Active || cat.Type != Type(in.Type) {
		return Category{}, ErrCategoryInvalid
	}
	return cat, nil
}

// scheduleOf reads the person's schedule. A module that was not wired gives an empty one: every
// calendar day counts.
func (s *Service) scheduleOf(ctx context.Context, p auth.Principal, from, to timeutil.Date) ([]ScheduleDay, error) {
	if s.schedule == nil {
		return nil, nil
	}
	instances, err := s.schedule.Instances(ctx, p.OrganizationID, p.PersonnelID, from, to)
	if err != nil {
		return nil, fmt.Errorf("schedule instances: %w", err)
	}
	out := make([]ScheduleDay, 0, len(instances))
	for _, in := range instances {
		out = append(out, in.ScheduleDay)
	}
	return out, nil
}

// teamOn picks the regu a request is made for: the one named, which the person must serve in on the
// day, or their only one that day.
func (s *Service) teamOn(ctx context.Context, q *db.Queries, personnelID int64, teamCode string, on timeutil.Date) (db.ListMembershipsOfPersonnelOnRow, error) {
	rows, err := q.ListMembershipsOfPersonnelOn(ctx, db.ListMembershipsOfPersonnelOnParams{PersonnelID: personnelID, OnDate: dateParam(on)})
	if err != nil {
		return db.ListMembershipsOfPersonnelOnRow{}, fmt.Errorf("list memberships: %w", err)
	}
	var found []db.ListMembershipsOfPersonnelOnRow
	for _, r := range rows {
		if teamCode == "" || r.TeamCode == teamCode {
			found = append(found, r)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) == 0:
		return db.ListMembershipsOfPersonnelOnRow{}, access.ErrPermissionDenied
	default:
		return db.ListMembershipsOfPersonnelOnRow{}, validationError("team_code", "Wajib diisi: Anda bertugas di lebih dari satu regu pada tanggal ini.")
	}
}

// checkQuota is the day check (KPI service.go:882-892): fewer than one day left fails, and so do
// more days than are left, except for a category that skips that comparison (maternity).
func (s *Service) checkQuota(ctx context.Context, q *db.Queries, personnelID int64, cat Category, teamCode string, on timeutil.Date, days int) error {
	a, err := s.AllowanceFor(ctx, q, personnelID, cat.Code, teamCode, on)
	if err != nil {
		return err
	}
	switch {
	case a.RemainingDays < 1:
		return quotaExceededError("Kuota " + cat.Name + " sudah habis atau belum tersedia.")
	case days > a.RemainingDays && !cat.SkipsDayCheck:
		return quotaExceededError(fmt.Sprintf("Sisa kuota %s %d hari, kurang dari %d hari yang diajukan.", cat.Name, a.RemainingDays, days))
	}
	return nil
}

// ---------- reads ----------

// requestOf builds the app's view of a request: its decision, its sick fields and its attachment.
func (s *Service) requestOf(ctx context.Context, q *db.Queries, lr db.LeaveRequest, categoryCode, categoryName, decidedBy string) (Request, error) {
	r := Request{
		ID: lr.ID, Number: lr.Number, PersonnelID: lr.PersonnelID, TeamCode: lr.TeamCode, ProjectCode: lr.ProjectCode,
		Type: lr.Type, CategoryCode: categoryCode, CategoryName: categoryName, Status: lr.Status,
		StartDate: dateValue(lr.StartDate), EndDate: dateValue(lr.EndDate), StartTime: clockOf(lr.StartTime), EndTime: clockOf(lr.EndTime),
		WorkingDays: int(lr.WorkingDays), Reason: lr.Reason, Responsibility: textPtr(lr.Responsibility), ContactInfo: textPtr(lr.ContactInfo),
		ChangeDayFrom: datePtr(lr.ChangeDayFrom), ChangeDayTo: datePtr(lr.ChangeDayTo),
		RequestedAt: lr.CreatedAt.Time, CanCancel: lr.Status == StatusPending,
	}
	if lr.DecidedLevel.Valid && lr.DecidedAt.Valid {
		r.Decision = &Decision{Level: string(lr.DecidedLevel.LeaveDecidedLevel), ByName: firstName(decidedBy), At: lr.DecidedAt.Time, Note: lr.DecisionNote.String}
	}
	if lr.Type == TypeSick {
		r.Sick = &Sick{Hospitalization: lr.Hospitalization.Bool, LetterStatus: lr.LetterStatus.Bool, LetterDate: datePtr(lr.LetterDate), Disease: lr.Disease.String}
	}
	if lr.AttachmentMediaID.Valid && s.files != nil {
		views, err := s.files.Views(ctx, q, entityType, lr.ID, []int64{lr.AttachmentMediaID.Int64})
		if err != nil {
			return Request{}, fmt.Errorf("attachment views: %w", err)
		}
		if len(views) > 0 {
			r.Attachment = &views[0]
		}
	}
	return r, nil
}

// Filter narrows the caller's own list.
type Filter struct {
	Types    []Type
	Statuses []Status
	// From and To keep the requests whose days intersect [From, To].
	From, To *timeutil.Date
	Limit    int
	Offset   int
}

// Page is a page of requests and how many there are in all.
type Page struct {
	Requests []Request
	Total    int
}

// ListMine returns the caller's requests, newest first.
func (s *Service) ListMine(ctx context.Context, p auth.Principal, f Filter) (Page, error) {
	var out Page
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		// Text arrays: the pool's exec mode cannot encode an array of an enum type.
		types, statuses := make([]string, 0, len(f.Types)), make([]string, 0, len(f.Statuses))
		for _, t := range f.Types {
			types = append(types, string(t))
		}
		for _, st := range f.Statuses {
			statuses = append(statuses, string(st))
		}
		rows, err := q.ListLeaveRequestViewsOfPersonnel(ctx, db.ListLeaveRequestViewsOfPersonnelParams{
			PersonnelID: p.PersonnelID, Types: types, Statuses: statuses, FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list leave requests: %w", err)
		}
		out.Requests = make([]Request, 0, len(rows))
		for _, row := range rows {
			r, err := s.requestOf(ctx, q, row.LeaveRequest, row.CategoryCode, row.CategoryName, row.DecidedByName)
			if err != nil {
				return err
			}
			out.Requests = append(out.Requests, r)
		}
		total, err := q.CountLeaveRequestsOfPersonnel(ctx, db.CountLeaveRequestsOfPersonnelParams{
			PersonnelID: p.PersonnelID, Types: types, Statuses: statuses, FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
		})
		out.Total = int(total)
		return err
	})
	return out, err
}

// Get is GET /permissions/{id}: the owner reads their request, and so does someone whose
// attendance.leave.approve reaches the request's regu today (the rule of the queue and of the
// decision, not the request's start date). Everyone else, and a
// request that is not there, get ErrNotFound (never 403): ids cannot be probed.
//
// The admins read requests through /admin routes (Part C).
func (s *Service) Get(ctx context.Context, p auth.Principal, id int64) (Request, error) {
	var out Request
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetLeaveRequestViewWithPerson(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave request: %w", err)
		}
		lr := row.LeaveRequest
		if lr.PersonnelID != p.PersonnelID {
			if s.perms == nil {
				return ErrNotFound
			}
			// Today in the regu's project zone, like the queue and the decision: whoever leads
			// the regu now reads it (a former DANRU no longer does), whatever the start date.
			_, loc, err := clientAndZone(ctx, q, lr.TeamCode)
			if err != nil {
				return err
			}
			res, err := s.perms.Resolve(ctx, q, p.PersonnelID, access.AttendanceLeaveApprove, timeutil.BusinessDate(s.clock.Now(), loc))
			if err != nil {
				return err
			}
			if !res.HasTeam(lr.TeamCode) {
				return ErrNotFound
			}
		}
		if out, err = s.requestOf(ctx, q, lr, row.CategoryCode, row.CategoryName, row.DecidedByName); err != nil {
			return err
		}
		if lr.PersonnelID != p.PersonnelID {
			return s.enrich(ctx, q, &out, row.PersonName, row.PersonPosition, true)
		}
		return nil
	})
	return out, err
}

// ---------- cancel ----------

// Cancel withdraws the caller's own request while it waits. Annual leave gets its days back, once.
//
// The status is read again after the person's row lock, so two cancels at once (or a cancel and a
// decision) take turns: one wins and the other is told why (KPI approval.go:429-501 has two
// different 409s, and so does SiAPP). Someone else's request is ErrNotFound.
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id int64) (Request, error) {
	var notice *Notice
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		owner, err := q.GetLeaveRequestView(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && owner.LeaveRequest.PersonnelID != p.PersonnelID {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave request: %w", err)
		}
		if _, err := q.LockPersonnel(ctx, p.PersonnelID); err != nil {
			return fmt.Errorf("lock personnel: %w", err)
		}
		locked, err := q.GetLeaveRequestForUpdate(ctx, id)
		if err != nil {
			return fmt.Errorf("lock leave request: %w", err)
		}
		switch locked.Status {
		case StatusPending:
		case StatusCancelled:
			return ErrAlreadyCancelled
		case StatusApproved, StatusRejected:
			return ErrCannotCancel
		}
		if _, err := q.CancelLeaveRequest(ctx, db.CancelLeaveRequestParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("cancel leave request: %w", err)
		}
		cat, err := categoryByCode(ctx, q, owner.CategoryCode)
		if err != nil {
			return err
		}
		if cat.IsAnnual {
			if _, err := Refund(ctx, q, s.clock.Now(), p.PersonnelID, id, "Pembatalan "+locked.Number); err != nil {
				return err
			}
		}
		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.request.cancel", EntityType: "leave_request", EntityID: id,
			Metadata: map[string]any{"number": locked.Number},
		}); err != nil {
			return err
		}
		notice, err = s.noticeFor(ctx, q, NoticeCancelled, locked, false)
		return err
	})
	if err != nil {
		return Request{}, err
	}
	s.notify(ctx, notice)
	return s.Get(ctx, p, id)
}
