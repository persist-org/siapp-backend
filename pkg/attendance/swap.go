package attendance

import (
	"context"
	"errors"
	"fmt"
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

// swapCheckInEarly mirrors organization's unexported checkInEarly (2h before a working shift's
// start still counts as current): a covered shift's window must match a normal one's.
const swapCheckInEarly = 2 * time.Hour

// swapInstance is an active swap, with the shift's current name and tolerance for lateness
// (looked up fresh, like a working shift's, not snapshotted on the swap row).
type swapInstance struct {
	Swap             db.ShiftSwap
	ShiftName        string
	ToleranceMinutes int
}

// activeSwapsNear returns personnelID's active swaps (as replacer) whose window contains now, the
// same width organization.Shifts.Current uses for a working shift: 2h before start to 4h after
// end. Checkable adds one of these per swap as an extra instance, in the swap's own team.
func (s *Service) activeSwapsNear(ctx context.Context, organizationID, personnelID int64, now time.Time) ([]swapInstance, error) {
	today := timeutil.BusinessDate(now, time.UTC)
	var out []swapInstance
	err := database.WithTenant(ctx, s.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListActiveSwapsOfReplacer(ctx, db.ListActiveSwapsOfReplacerParams{
			PersonnelID: personnelID, DateFrom: dateParam(today.AddDays(-2)), DateTo: dateParam(today.AddDays(1)),
		})
		if err != nil {
			return fmt.Errorf("active swaps: %w", err)
		}
		for _, sw := range rows {
			if now.Before(sw.StartsAt.Time.Add(-swapCheckInEarly)) || now.After(sw.EndsAt.Time.Add(organization.CheckOutLate)) {
				continue
			}
			var name string
			var tolerance int
			if sh, serr := q.GetShift(ctx, sw.ShiftID); serr == nil {
				name, tolerance = sh.Name, int(sh.ToleranceMinutes)
			} else if !errors.Is(serr, pgx.ErrNoRows) {
				return fmt.Errorf("get shift: %w", serr)
			}
			out = append(out, swapInstance{Swap: sw, ShiftName: name, ToleranceMinutes: tolerance})
		}
		return nil
	})
	return out, err
}

// Phase 07.7: shift swap (tukar jaga). The DANRU assigns a replacement for an absent guard; the
// replacement holds the extra shift at once, no approval step.

// Swap is one shift swap.
type Swap struct {
	ID                   int64
	ReplacedPersonnelID  int64
	ReplacedAttendanceID int64
	ReplacingPersonnelID int64
	TeamCode             string
	WorkDate             timeutil.Date
	ShiftID              int64
	StartsAt             time.Time
	EndsAt               time.Time
	Note                 *string
	CreatedByID          int64
	CancelledAt          *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func toSwap(r db.ShiftSwap) Swap {
	return Swap{
		ID: r.ID, ReplacedPersonnelID: r.ReplacedPersonnelID, ReplacedAttendanceID: r.ReplacedAttendanceID,
		ReplacingPersonnelID: r.ReplacingPersonnelID, TeamCode: r.TeamCode, WorkDate: dateValue(r.WorkDate),
		ShiftID: r.ShiftID, StartsAt: r.StartsAt.Time, EndsAt: r.EndsAt.Time, Note: textPtrValue(r.Note),
		CreatedByID: r.CreatedByID, CancelledAt: timePtr(r.CancelledAt), CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func textPtrValue(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func textValueOr(v pgtype.Text, def string) string {
	if !v.Valid {
		return def
	}
	return v.String
}

func boolPtrParam(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

// Candidate is one person GET /shift-swaps/candidates offers.
type Candidate struct {
	PersonnelID int64
	Name        string
}

// Replaceable is GET /shift-swaps/replaceable?team_code=&work_date=: the regu's LEAVE/ABSENT
// records on a date that have no active swap yet.
func (s *Service) Replaceable(ctx context.Context, p auth.Principal, teamCode string, date timeutil.Date) ([]Attendance, error) {
	var out []Attendance
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceSwapCreate, date)
		if err != nil {
			return err
		}
		if !res.HasTeam(teamCode) {
			return access.ErrPermissionDenied
		}
		rows, err := q.ListReplaceableAttendances(ctx, db.ListReplaceableAttendancesParams{TeamCode: teamCode, WorkDate: dateParam(date)})
		if err != nil {
			return fmt.Errorf("list replaceable: %w", err)
		}
		out = make([]Attendance, 0, len(rows))
		for _, r := range rows {
			out = append(out, toAttendance(r))
		}
		return nil
	})
	return out, err
}

// Candidates is GET /shift-swaps/candidates?team_code=&work_date=: members of the team who are
// not on leave or absent that day. Any team in scope, so cross-team and cross-project replacement
// works.
func (s *Service) Candidates(ctx context.Context, p auth.Principal, teamCode string, date timeutil.Date) ([]Candidate, error) {
	var out []Candidate
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceSwapCreate, date)
		if err != nil {
			return err
		}
		if !res.HasTeam(teamCode) {
			return access.ErrPermissionDenied
		}
		rows, err := q.ListSwapCandidates(ctx, db.ListSwapCandidatesParams{TeamCode: teamCode, WorkDate: dateParam(date)})
		if err != nil {
			return fmt.Errorf("list candidates: %w", err)
		}
		out = make([]Candidate, 0, len(rows))
		for _, r := range rows {
			out = append(out, Candidate{PersonnelID: r.PersonnelID, Name: r.FullName})
		}
		return nil
	})
	return out, err
}

// CreateSwapInput is the body of POST /shift-swaps.
type CreateSwapInput struct {
	ReplacedAttendanceID int64  `json:"replaced_attendance_id" validate:"required,min=1"`
	ReplacingPersonnelID int64  `json:"replacing_personnel_id" validate:"required,min=1"`
	Note                 string `json:"note" validate:"omitempty,max=500"`
}

// Create is POST /shift-swaps: the DANRU assigning a replacement for an absent or on-leave guard.
// Validated in the plan's order: the record's own team and the replacing person's current team
// are both in the caller's attendance.swap.create reach (ErrSwapNotFound otherwise, the same
// answer as an id that does not exist, so neither can be probed); the record is LEAVE or ABSENT;
// the replacing person is not the replaced person, and is not on leave or absent that day; their
// own shifts that day must not overlap the one they are taking over (back-to-back is fine).
func (s *Service) Create(ctx context.Context, p auth.Principal, in CreateSwapInput) (Swap, error) {
	now := s.clock.Now()
	var out Swap
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		replaced, err := q.GetAttendanceForUpdate(ctx, in.ReplacedAttendanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSwapNotFound
		}
		if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		workDate := dateValue(replaced.WorkDate)

		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceSwapCreate, workDate)
		if err != nil {
			return err
		}
		if !res.HasTeam(replaced.TeamCode) {
			return ErrSwapNotFound
		}
		if replaced.Status != db.AttendanceStatusLEAVE && replaced.Status != db.AttendanceStatusABSENT {
			return ErrSwapReplacedNotAbsent
		}
		if in.ReplacingPersonnelID == replaced.PersonnelID {
			return apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "replacing_personnel_id", Code: "INVALID", Message: "Tidak boleh personil yang sama dengan yang digantikan.",
			})
		}

		memberships, err := s.sites.ActiveMemberships(ctx, p.OrganizationID, in.ReplacingPersonnelID, workDate)
		if err != nil {
			return fmt.Errorf("active memberships: %w", err)
		}
		inScope := false
		for _, m := range memberships {
			if res.HasTeam(m.TeamCode) {
				inScope = true
				break
			}
		}
		if !inScope {
			return ErrSwapNotFound
		}

		live, err := q.ListAttendancesOfPersonnelBetween(ctx, db.ListAttendancesOfPersonnelBetweenParams{
			PersonnelID: in.ReplacingPersonnelID, DateFrom: replaced.WorkDate, DateTo: replaced.WorkDate,
		})
		if err != nil {
			return fmt.Errorf("replacing attendances: %w", err)
		}
		for _, r := range live {
			if r.Status == db.AttendanceStatusLEAVE || r.Status == db.AttendanceStatusABSENT {
				return apperr.ErrValidation.WithFields(apperr.FieldError{
					Field: "replacing_personnel_id", Code: "INVALID", Message: "Personil pengganti sedang izin atau mangkir pada tanggal ini.",
				})
			}
		}

		instances, err := s.shifts.Instances(ctx, p.OrganizationID, in.ReplacingPersonnelID, workDate, workDate)
		if err != nil {
			return fmt.Errorf("replacing shifts: %w", err)
		}
		for _, inst := range instances {
			if inst.IsOff {
				continue
			}
			if inst.StartsAt.Before(replaced.ShiftEndsAt.Time) && replaced.ShiftStartsAt.Time.Before(inst.EndsAt) {
				return ErrSwapShiftOverlap
			}
		}

		row, err := q.InsertShiftSwap(ctx, db.InsertShiftSwapParams{
			ReplacedPersonnelID: replaced.PersonnelID, ReplacedAttendanceID: replaced.ID, ReplacingPersonnelID: in.ReplacingPersonnelID,
			TeamCode: replaced.TeamCode, WorkDate: replaced.WorkDate, ShiftID: replaced.ShiftID,
			StartsAt: replaced.ShiftStartsAt, EndsAt: replaced.ShiftEndsAt, Note: textOrNull(in.Note),
			CreatedByID: p.PersonnelID, Now: ts(now),
		})
		if database.IsUniqueViolation(err) {
			return ErrSwapAlreadyExists
		}
		if err != nil {
			return fmt.Errorf("insert shift swap: %w", err)
		}

		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "shift_swap.create", EntityType: "shift_swap", EntityID: row.ID,
			Metadata: map[string]any{
				"replaced_attendance_id": replaced.ID, "replaced_personnel_id": replaced.PersonnelID,
				"replacing_personnel_id": in.ReplacingPersonnelID, "team_code": replaced.TeamCode, "work_date": workDate.String(),
			},
		}); err != nil {
			return err
		}
		out = toSwap(row)
		return nil
	})
	return out, err
}

// SwapFilter narrows GET /shift-swaps.
type SwapFilter struct {
	// Active, when set, keeps only active (true) or cancelled (false) swaps; nil keeps both.
	Active   *bool
	From, To *timeutil.Date
}

// ListSwaps is GET /shift-swaps: every swap of the teams the caller's attendance.swap.create
// reaches, newest first.
func (s *Service) ListSwaps(ctx context.Context, p auth.Principal, f SwapFilter) ([]Swap, error) {
	var out []Swap
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceSwapCreate, timeutil.BusinessDate(s.clock.Now(), wib))
		if err != nil {
			return err
		}
		if len(res.TeamCodes) == 0 {
			return access.ErrPermissionDenied
		}
		rows, err := q.ListShiftSwaps(ctx, db.ListShiftSwapsParams{
			TeamCodes: res.TeamCodes, Active: boolPtrParam(f.Active), FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
		})
		if err != nil {
			return fmt.Errorf("list shift swaps: %w", err)
		}
		out = make([]Swap, 0, len(rows))
		for _, r := range rows {
			out = append(out, toSwap(r))
		}
		return nil
	})
	return out, err
}

// CancelSwap is DELETE /shift-swaps/{id}: allowed only before the replacing person has checked
// into the covered shift.
func (s *Service) CancelSwap(ctx context.Context, p auth.Principal, id int64) error {
	now := s.clock.Now()
	return database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetShiftSwapForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSwapNotFound
		}
		if err != nil {
			return fmt.Errorf("get shift swap: %w", err)
		}
		if row.CancelledAt.Valid {
			return ErrSwapNotFound
		}
		res, err := s.access.Resolve(ctx, q, p.PersonnelID, access.AttendanceSwapCreate, dateValue(row.WorkDate))
		if err != nil {
			return err
		}
		if !res.HasTeam(row.TeamCode) {
			return ErrSwapNotFound
		}
		existing, err := existingRecord(ctx, q, row.ReplacingPersonnelID, shiftKey{TeamCode: row.TeamCode, WorkDate: dateValue(row.WorkDate), ShiftID: row.ShiftID})
		if err != nil {
			return err
		}
		if existing != nil && existing.CheckInAt.Valid {
			return ErrSwapAlreadyStarted
		}
		updated, err := q.CancelShiftSwap(ctx, db.CancelShiftSwapParams{Now: ts(now), ID: id})
		if err != nil {
			return fmt.Errorf("cancel shift swap: %w", err)
		}
		actorID := p.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "shift_swap.cancel", EntityType: "shift_swap", EntityID: updated.ID,
			Metadata: map[string]any{"replaced_attendance_id": updated.ReplacedAttendanceID, "team_code": updated.TeamCode},
		})
	})
}

// ---------- the replacer's two tabs ----------

// ReplacedRow is one of the caller's own shifts someone else took over ("Digantikan").
type ReplacedRow struct {
	WorkDate            timeutil.Date
	ShiftName           string
	ReplacingName       string
	ReplacingCheckInAt  *time.Time
	ReplacingCheckOutAt *time.Time
}

// Replaced is GET /attendances/replaced?date_from=&date_to=.
func (s *Service) Replaced(ctx context.Context, p auth.Principal, from, to *timeutil.Date) ([]ReplacedRow, error) {
	var out []ReplacedRow
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListReplacedSwaps(ctx, db.ListReplacedSwapsParams{PersonnelID: p.PersonnelID, FromDate: dateParamPtr(from), ToDate: dateParamPtr(to)})
		if err != nil {
			return fmt.Errorf("list replaced swaps: %w", err)
		}
		out = make([]ReplacedRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, ReplacedRow{
				WorkDate: dateValue(r.WorkDate), ShiftName: textValueOr(r.ShiftName, ""), ReplacingName: r.ReplacingName,
				ReplacingCheckInAt: timePtr(r.ReplacingCheckInAt), ReplacingCheckOutAt: timePtr(r.ReplacingCheckOutAt),
			})
		}
		return nil
	})
	return out, err
}

// ReplacingRow is one shift the caller took over from someone else ("Menggantikan").
type ReplacingRow struct {
	WorkDate     timeutil.Date
	ShiftName    string
	ReplacedName string
	CheckInAt    *time.Time
	CheckOutAt   *time.Time
}

// Replacing is GET /attendances/replacing?date_from=&date_to=.
func (s *Service) Replacing(ctx context.Context, p auth.Principal, from, to *timeutil.Date) ([]ReplacingRow, error) {
	var out []ReplacingRow
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListReplacingSwaps(ctx, db.ListReplacingSwapsParams{PersonnelID: p.PersonnelID, FromDate: dateParamPtr(from), ToDate: dateParamPtr(to)})
		if err != nil {
			return fmt.Errorf("list replacing swaps: %w", err)
		}
		out = make([]ReplacingRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, ReplacingRow{
				WorkDate: dateValue(r.WorkDate), ShiftName: textValueOr(r.ShiftName, ""), ReplacedName: r.ReplacedName,
				CheckInAt: timePtr(r.CheckInAt), CheckOutAt: timePtr(r.CheckOutAt),
			})
		}
		return nil
	})
	return out, err
}
