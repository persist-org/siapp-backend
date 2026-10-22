package organization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// clockPattern is a local time of day as the API writes it: "HH:mm", 00:00 to 23:59.
var clockPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// Weekdays are the days of a shift group, Monday (1) to Sunday (7) as ISO counts them. The
// names are the keys the API uses, as PJ48 does.
var Weekdays = [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}

// Shift is one entry of the shift master (PJ48 m_shifts). The times are minutes after
// midnight, local to the project the shift is worked at.
type Shift struct {
	ID               int64
	Name             string
	StartMinute      int
	EndMinute        int
	IsOff            bool
	ToleranceMinutes int
	Active           bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Overnight reports whether the shift ends on the next day: its end is not after its start.
func (s Shift) Overnight() bool { return !s.IsOff && s.EndMinute <= s.StartMinute }

// ShiftInput is a request to create a shift. An OFF day (is_off) has no times, overnight or
// tolerance: sending any of them is refused. A working shift needs both times, and must say
// overnight: true to end on the next day (a reversed range is otherwise a typo).
type ShiftInput struct {
	Name             string  `json:"name" validate:"required,notblank,max=100"`
	StartTime        *string `json:"start_time"`
	EndTime          *string `json:"end_time"`
	IsOff            bool    `json:"is_off"`
	Overnight        *bool   `json:"overnight"`
	ToleranceMinutes *int    `json:"tolerance_minutes" validate:"omitempty,gte=0,lte=720"`
	Active           *bool   `json:"is_active"`
}

// ShiftUpdate is a request to change a shift. is_off has no field: an OFF day cannot become a
// working shift (or the reverse) under the schedules that already use it. An omitted
// overnight keeps the shift's own, and the new times must agree with it.
type ShiftUpdate struct {
	Name             *string `json:"name" validate:"omitempty,notblank,max=100"`
	StartTime        *string `json:"start_time"`
	EndTime          *string `json:"end_time"`
	Overnight        *bool   `json:"overnight"`
	ToleranceMinutes *int    `json:"tolerance_minutes" validate:"omitempty,gte=0,lte=720"`
	Active           *bool   `json:"is_active"`
}

// ShiftFilter narrows a list. Zero values match everything.
type ShiftFilter struct {
	Q      string
	Active *bool
	Limit  int
	Offset int
}

// GroupDay is one day of a shift group and its shift.
type GroupDay struct {
	Weekday int // 1 = Monday .. 7 = Sunday
	Shift   Shift
}

// ShiftGroup is a weekly pattern (PJ48 shift group): one shift for each day of the week.
type ShiftGroup struct {
	ID        int64
	Name      string
	Days      []GroupDay // always 7, Monday first
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ShiftGroupInput is a request to create a shift group. Days must name all seven weekdays.
type ShiftGroupInput struct {
	Name string           `json:"name" validate:"required,notblank,max=100"`
	Days map[string]int64 `json:"days"`
}

// ShiftGroupUpdate is a request to change a shift group: its name, and any of its days.
type ShiftGroupUpdate struct {
	Name *string          `json:"name" validate:"omitempty,notblank,max=100"`
	Days map[string]int64 `json:"days"`
}

// ShiftMasters manages the organization's shifts and shift groups. Everyone with a web role
// reads them; only an ORG_ADMIN changes them (they are one list for the whole group, as in
// PJ48).
type ShiftMasters struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewShiftMasters returns the ShiftMasters usecase.
func NewShiftMasters(pool *pgxpool.Pool, c clock.Clock) *ShiftMasters {
	return &ShiftMasters{pool: pool, clock: c}
}

// ListShifts returns one page of shifts, by name, and their total.
func (m *ShiftMasters) ListShifts(ctx context.Context, actor auth.Principal, f ShiftFilter) ([]Shift, int, error) {
	var out []Shift
	var total int64
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListShifts(ctx, db.ListShiftsParams{
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list shifts: %w", err)
		}
		for _, r := range rows {
			out = append(out, toShift(r))
		}
		total, err = q.CountShifts(ctx, db.CountShiftsParams{Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count shifts: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetShift returns one shift.
func (m *ShiftMasters) GetShift(ctx context.Context, actor auth.Principal, id int64) (Shift, error) {
	var out Shift
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetShift(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShiftNotFound
		}
		if err != nil {
			return fmt.Errorf("get shift: %w", err)
		}
		out = toShift(row)
		return nil
	})
	return out, err
}

// CreateShift adds a shift. Only an ORG_ADMIN may.
func (m *ShiftMasters) CreateShift(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ShiftInput) (Shift, error) {
	if !scope.All {
		return Shift{}, access.ErrOrgAdminOnly
	}
	var start, end, tolerance int
	if in.IsOff {
		if err := offHasNoTimes(in.StartTime, in.EndTime, in.Overnight, in.ToleranceMinutes); err != nil {
			return Shift{}, err
		}
	} else {
		if in.StartTime == nil || in.EndTime == nil {
			return Shift{}, ErrShiftInvalidTime.WithMessage("Shift kerja wajib memiliki start_time dan end_time.")
		}
		tolerance = defaultTolerance
		if in.ToleranceMinutes != nil {
			tolerance = *in.ToleranceMinutes
		}
		var err error
		if start, end, err = workingTimes(*in.StartTime, *in.EndTime, in.Overnight != nil && *in.Overnight, tolerance); err != nil {
			return Shift{}, err
		}
	}
	var out Shift
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.CreateShift(ctx, db.CreateShiftParams{
			Name: strings.TrimSpace(in.Name), StartMinute: int16(start), EndMinute: int16(end), //nolint:gosec // 0..1439
			IsOff: in.IsOff, ToleranceMinutes: int16(tolerance), //nolint:gosec // validated 0..720
			Active: in.Active == nil || *in.Active, Actor: personnelRef(actor), Now: ts(m.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toShift(row)
		return recordIDAudit(ctx, q, actor, "shift.create", "shift", out.ID, nil, shiftMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Shift{}, ErrShiftNameTaken
	}
	if err != nil {
		return Shift{}, wrapUnexpected(err, "create shift")
	}
	return out, nil
}

// UpdateShift changes a shift. Only an ORG_ADMIN may. A change of times applies to every date
// already scheduled with this shift, as in PJ48.
func (m *ShiftMasters) UpdateShift(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ShiftUpdate) (Shift, error) {
	if !scope.All {
		return Shift{}, access.ErrOrgAdminOnly
	}
	var out Shift
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetShiftForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShiftNotFound
		}
		if err != nil {
			return fmt.Errorf("get shift: %w", err)
		}
		before := toShift(row)
		var start, end, tolerance pgtype.Int2
		if before.IsOff {
			if err := offHasNoTimes(in.StartTime, in.EndTime, in.Overnight, in.ToleranceMinutes); err != nil {
				return err
			}
		} else if in.StartTime != nil || in.EndTime != nil || in.Overnight != nil || in.ToleranceMinutes != nil {
			// What is not sent is the shift's own, and the whole result is checked: a new end
			// must still agree with the shift's overnight, and its tolerance with its length.
			startText, endText := formatClock(before.StartMinute), formatClock(before.EndMinute)
			if in.StartTime != nil {
				startText = *in.StartTime
			}
			if in.EndTime != nil {
				endText = *in.EndTime
			}
			overnight := before.Overnight()
			if in.Overnight != nil {
				overnight = *in.Overnight
			}
			t := before.ToleranceMinutes
			if in.ToleranceMinutes != nil {
				t = *in.ToleranceMinutes
			}
			s, e, err := workingTimes(startText, endText, overnight, t)
			if err != nil {
				return err
			}
			start, end = pgtype.Int2{Int16: int16(s), Valid: true}, pgtype.Int2{Int16: int16(e), Valid: true} //nolint:gosec // 0..1439
			tolerance = pgtype.Int2{Int16: int16(t), Valid: true}                                             //nolint:gosec // validated 0..720
		}
		if before.Active && in.Active != nil && !*in.Active {
			// ⚠ A group keeps generating its days with this shift every night, so an inactive
			// shift would still be scheduled: the group's days must move off it first.
			days, err := q.CountGroupDaysUsingShift(ctx, id)
			if err != nil {
				return fmt.Errorf("count group days of shift: %w", err)
			}
			if days > 0 {
				return ErrShiftInUse.WithMessage("Shift masih dipakai shift group. Ganti hari yang memakainya sebelum menonaktifkan.")
			}
		}
		updated, err := q.UpdateShift(ctx, db.UpdateShiftParams{
			ID: id, Name: textPtr(trimmed(in.Name)), StartMinute: start, EndMinute: end,
			ToleranceMinutes: tolerance, Active: boolOrNull(in.Active), Actor: personnelRef(actor), Now: ts(m.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toShift(updated)
		return recordIDAudit(ctx, q, actor, "shift.update", "shift", id, shiftMeta(before), shiftMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Shift{}, ErrShiftNameTaken
	}
	if err != nil {
		return Shift{}, wrapUnexpected(err, "update shift")
	}
	return out, nil
}

// DeleteShift removes a shift (a soft delete: past schedules keep pointing at it). Only an
// ORG_ADMIN may. A shift a group uses, or one scheduled from today on, is SHIFT_IN_USE.
func (m *ShiftMasters) DeleteShift(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetShiftForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShiftNotFound
		}
		if err != nil {
			return fmt.Errorf("get shift: %w", err)
		}
		days, err := q.CountGroupDaysUsingShift(ctx, id)
		if err != nil {
			return fmt.Errorf("count group days of shift: %w", err)
		}
		// The earliest "today" of the three zones is WIB's, so counting from it covers today
		// everywhere.
		scheduled, err := q.CountSchedulesOfShiftFrom(ctx, db.CountSchedulesOfShiftFromParams{ShiftID: id, FromDate: dateParam(earliestToday(m.clock.Now()))})
		if err != nil {
			return fmt.Errorf("count schedules of shift: %w", err)
		}
		if days > 0 || scheduled > 0 {
			return ErrShiftInUse
		}
		if _, err := q.SoftDeleteShift(ctx, db.SoftDeleteShiftParams{ID: id, Actor: personnelRef(actor), Now: ts(m.clock.Now())}); err != nil {
			return fmt.Errorf("delete shift: %w", err)
		}
		return recordIDAudit(ctx, q, actor, "shift.delete", "shift", id, shiftMeta(toShift(row)), nil)
	})
}

// ListShiftGroups returns one page of shift groups with their days, by name, and their total.
func (m *ShiftMasters) ListShiftGroups(ctx context.Context, actor auth.Principal, q string, limit, offset int) ([]ShiftGroup, int, error) {
	var out []ShiftGroup
	var total int64
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(qs *db.Queries) error {
		if offset > math.MaxInt32 || limit > math.MaxInt32 {
			return nil
		}
		rows, err := qs.ListShiftGroups(ctx, db.ListShiftGroupsParams{
			Q: textOrNull(q), PageLimit: int32(limit), PageOffset: int32(offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list shift groups: %w", err)
		}
		if out, err = withDays(ctx, qs, rows); err != nil {
			return err
		}
		total, err = qs.CountShiftGroups(ctx, textOrNull(q))
		if err != nil {
			return fmt.Errorf("count shift groups: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetShiftGroup returns one shift group with its days.
func (m *ShiftMasters) GetShiftGroup(ctx context.Context, actor auth.Principal, id int64) (ShiftGroup, error) {
	var out ShiftGroup
	err := database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		var err error
		out, err = loadGroup(ctx, q, id, lockNone)
		return err
	})
	return out, err
}

// CreateShiftGroup adds a shift group with all seven days. Only an ORG_ADMIN may.
func (m *ShiftMasters) CreateShiftGroup(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ShiftGroupInput) (ShiftGroup, error) {
	if !scope.All {
		return ShiftGroup{}, access.ErrOrgAdminOnly
	}
	days, err := parseDays(in.Days, true)
	if err != nil {
		return ShiftGroup{}, err
	}
	var out ShiftGroup
	err = database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := checkDayShifts(ctx, q, days); err != nil {
			return err
		}
		row, err := q.CreateShiftGroup(ctx, db.CreateShiftGroupParams{Name: strings.TrimSpace(in.Name), Actor: personnelRef(actor), Now: ts(m.clock.Now())})
		if err != nil {
			return err
		}
		if err := saveDays(ctx, q, row.ID, days); err != nil {
			return err
		}
		if out, err = loadGroup(ctx, q, row.ID, lockNone); err != nil {
			return err
		}
		return recordIDAudit(ctx, q, actor, "shift_group.create", "shift_group", row.ID, nil, groupMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return ShiftGroup{}, ErrShiftGroupNameTaken
	}
	if err != nil {
		return ShiftGroup{}, wrapUnexpected(err, "create shift group")
	}
	return out, nil
}

// UpdateShiftGroup changes a group's name and any of its days. Only an ORG_ADMIN may.
//
// As in PJ48 the change is to the pattern only: schedules already generated from it stay; the
// dates generated from now on (the nightly extension, or a new assignment) follow it.
func (m *ShiftMasters) UpdateShiftGroup(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in ShiftGroupUpdate) (ShiftGroup, error) {
	if !scope.All {
		return ShiftGroup{}, access.ErrOrgAdminOnly
	}
	days, err := parseDays(in.Days, false)
	if err != nil {
		return ShiftGroup{}, err
	}
	var out ShiftGroup
	err = database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := loadGroup(ctx, q, id, lockUpdate)
		if err != nil {
			return err
		}
		if err := checkDayShifts(ctx, q, days); err != nil {
			return err
		}
		if _, err := q.UpdateShiftGroup(ctx, db.UpdateShiftGroupParams{ID: id, Name: textPtr(trimmed(in.Name)), Actor: personnelRef(actor), Now: ts(m.clock.Now())}); err != nil {
			return err
		}
		if err := saveDays(ctx, q, id, days); err != nil {
			return err
		}
		if out, err = loadGroup(ctx, q, id, lockNone); err != nil {
			return err
		}
		return recordIDAudit(ctx, q, actor, "shift_group.update", "shift_group", id, groupMeta(before), groupMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return ShiftGroup{}, ErrShiftGroupNameTaken
	}
	if err != nil {
		return ShiftGroup{}, wrapUnexpected(err, "update shift group")
	}
	return out, nil
}

// DeleteShiftGroup removes a shift group (a soft delete). Only an ORG_ADMIN may. A group a
// person is assigned to is SHIFT_GROUP_IN_USE.
func (m *ShiftMasters) DeleteShiftGroup(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, m.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := loadGroup(ctx, q, id, lockUpdate)
		if err != nil {
			return err
		}
		assigned, err := q.CountAssignmentsOfShiftGroup(ctx, pgtype.Int8{Int64: id, Valid: true})
		if err != nil {
			return fmt.Errorf("count assignments of shift group: %w", err)
		}
		if assigned > 0 {
			return ErrShiftGroupInUse
		}
		if _, err := q.SoftDeleteShiftGroup(ctx, db.SoftDeleteShiftGroupParams{ID: id, Actor: personnelRef(actor), Now: ts(m.clock.Now())}); err != nil {
			return fmt.Errorf("delete shift group: %w", err)
		}
		return recordIDAudit(ctx, q, actor, "shift_group.delete", "shift_group", id, groupMeta(before), nil)
	})
}

// defaultTolerance is a new working shift's tolerance when none is sent (PJ48's 15 minutes).
const defaultTolerance = 15

// offHasNoTimes refuses times, overnight or a tolerance on an OFF day: it has none of them, and
// accepting them silently would let an admin believe they were saved.
func offHasNoTimes(start, end *string, overnight *bool, tolerance *int) error {
	var fields []apperr.FieldError
	for _, f := range []struct {
		name string
		sent bool
	}{{"start_time", start != nil}, {"end_time", end != nil}, {"overnight", overnight != nil}, {"tolerance_minutes", tolerance != nil}} {
		if f.sent {
			fields = append(fields, apperr.FieldError{Field: f.name, Code: "INVALID", Message: "Shift OFF tidak memiliki field ini."})
		}
	}
	if len(fields) > 0 {
		return ErrShiftInvalidTime.WithMessage("Shift OFF tidak memiliki jam, overnight, maupun toleransi.").WithFields(fields...)
	}
	return nil
}

// workingTimes validates a working shift and returns its times as minutes after midnight.
//
// ⚠ overnight is what the admin says, not what the times imply: an end before the start is a
// night shift only when overnight is true, and a typo otherwise. The two must agree, the times
// must differ, and the tolerance must be shorter than the shift.
func workingTimes(start, end string, overnight bool, tolerance int) (int, int, error) {
	s, okS := parseClock(start)
	e, okE := parseClock(end)
	switch {
	case !okS:
		return 0, 0, ErrShiftInvalidTime.WithFields(apperr.FieldError{Field: "start_time", Code: "INVALID", Message: "Harus berformat HH:mm (00:00 sampai 23:59)."})
	case !okE:
		return 0, 0, ErrShiftInvalidTime.WithFields(apperr.FieldError{Field: "end_time", Code: "INVALID", Message: "Harus berformat HH:mm (00:00 sampai 23:59)."})
	case s == e:
		return 0, 0, ErrShiftInvalidTime.WithFields(apperr.FieldError{Field: "end_time", Code: "INVALID", Message: "Tidak boleh sama dengan start_time."})
	case !overnight && e < s:
		return 0, 0, ErrShiftInvalidTime.WithMessage("Jam pulang sebelum jam masuk. Untuk shift yang berakhir keesokan harinya, kirim overnight: true.").
			WithFields(apperr.FieldError{Field: "end_time", Code: "INVALID", Message: "Harus setelah start_time bila overnight false."})
	case overnight && e > s:
		return 0, 0, ErrShiftInvalidTime.WithMessage("Shift overnight berakhir keesokan harinya: jam pulang harus sebelum jam masuk.").
			WithFields(apperr.FieldError{Field: "overnight", Code: "INVALID", Message: "Jam pulang setelah jam masuk: shift ini tidak melewati tengah malam."})
	}
	length := e - s
	if overnight {
		length += 24 * 60
	}
	if tolerance >= length {
		return 0, 0, validationError("tolerance_minutes", fmt.Sprintf("Harus lebih pendek dari durasi shift (%d menit).", length))
	}
	return s, e, nil
}

// parseClock reads "HH:mm" as minutes after midnight.
func parseClock(s string) (int, bool) {
	m := clockPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return h*60 + mi, true
}

// formatClock writes minutes after midnight as "HH:mm".
func formatClock(minutes int) string { return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60) }

// parseDays reads the day map of a request: weekday name to shift id. A create needs all
// seven days; a change any of them. Names are exact (lowercase, as the contract's enum): a
// case-insensitive match would let "Monday" and "monday" both arrive and one silently win.
func parseDays(in map[string]int64, all bool) (map[int]int64, error) {
	out := map[int]int64{}
	for name, shiftID := range in {
		weekday := slices.Index(Weekdays[:], name) + 1
		if weekday == 0 {
			return nil, validationError("days."+name, "Hari tidak dikenal. Gunakan monday sampai sunday.")
		}
		if shiftID < 1 {
			return nil, validationError("days."+Weekdays[weekday-1], "Wajib diisi dengan id shift.")
		}
		out[weekday] = shiftID
	}
	if all {
		for i, w := range Weekdays {
			if _, ok := out[i+1]; !ok {
				return nil, validationError("days."+w, "Ketujuh hari wajib diisi (monday sampai sunday).")
			}
		}
	}
	return out, nil
}

// checkDayShifts refuses a day that names a shift that does not exist or is not active.
func checkDayShifts(ctx context.Context, q *db.Queries, days map[int]int64) error {
	if len(days) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(days))
	for _, id := range days {
		ids = append(ids, id)
	}
	// FOR SHARE: a deactivation or delete of one of them in flight is waited for and then seen.
	rows, err := q.GetShiftsByIDsForShare(ctx, ids)
	if err != nil {
		return fmt.Errorf("get shifts: %w", err)
	}
	found := map[int64]bool{}
	for _, r := range rows {
		found[r.ID] = r.Active
	}
	for weekday := 1; weekday <= 7; weekday++ {
		id, ok := days[weekday]
		if !ok {
			continue
		}
		active, exists := found[id]
		if !exists {
			return validationError("days."+Weekdays[weekday-1], "Shift tidak ditemukan.")
		}
		if !active {
			return validationError("days."+Weekdays[weekday-1], "Shift tidak aktif.")
		}
	}
	return nil
}

func saveDays(ctx context.Context, q *db.Queries, groupID int64, days map[int]int64) error {
	for weekday, shiftID := range days {
		if err := q.UpsertShiftGroupDay(ctx, db.UpsertShiftGroupDayParams{
			ShiftGroupID: groupID, Weekday: int16(weekday), ShiftID: shiftID, //nolint:gosec // 1..7
		}); err != nil {
			return fmt.Errorf("save shift group day: %w", err)
		}
	}
	return nil
}

// rowLock is how a read locks the row it reads.
type rowLock int

const (
	lockNone   rowLock = iota
	lockShare          // FOR SHARE: the row is about to be pointed at; a delete of it waits
	lockUpdate         // FOR UPDATE: the row is about to change
)

// loadGroup reads a group and its days, locking the group row as asked.
func loadGroup(ctx context.Context, q *db.Queries, id int64, lock rowLock) (ShiftGroup, error) {
	var row db.ShiftGroup
	var err error
	switch lock {
	case lockUpdate:
		row, err = q.GetShiftGroupForUpdate(ctx, id)
	case lockShare:
		row, err = q.GetShiftGroupForShare(ctx, id)
	case lockNone:
		row, err = q.GetShiftGroup(ctx, id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ShiftGroup{}, ErrShiftGroupNotFound
	}
	if err != nil {
		return ShiftGroup{}, fmt.Errorf("get shift group: %w", err)
	}
	groups, err := withDays(ctx, q, []db.ShiftGroup{row})
	if err != nil {
		return ShiftGroup{}, err
	}
	return groups[0], nil
}

func withDays(ctx context.Context, q *db.Queries, rows []db.ShiftGroup) ([]ShiftGroup, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	days, err := q.ListShiftGroupDays(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list shift group days: %w", err)
	}
	byGroup := map[int64][]GroupDay{}
	for _, d := range days {
		byGroup[d.ShiftGroupID] = append(byGroup[d.ShiftGroupID], GroupDay{Weekday: int(d.Weekday), Shift: Shift{
			ID: d.ShiftID, Name: d.ShiftName, StartMinute: int(d.StartMinute), EndMinute: int(d.EndMinute), IsOff: d.IsOff,
		}})
	}
	out := make([]ShiftGroup, 0, len(rows))
	for _, r := range rows {
		out = append(out, ShiftGroup{ID: r.ID, Name: r.Name, Days: byGroup[r.ID], CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

func toShift(r db.Shift) Shift {
	return Shift{
		ID: r.ID, Name: r.Name, StartMinute: int(r.StartMinute), EndMinute: int(r.EndMinute), IsOff: r.IsOff,
		ToleranceMinutes: int(r.ToleranceMinutes), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func shiftMeta(s Shift) map[string]any {
	return map[string]any{
		"name": s.Name, "start_time": formatClock(s.StartMinute), "end_time": formatClock(s.EndMinute),
		"is_off": s.IsOff, "tolerance_minutes": s.ToleranceMinutes, "active": s.Active,
	}
}

func groupMeta(g ShiftGroup) map[string]any {
	days := map[string]any{}
	for _, d := range g.Days {
		days[Weekdays[d.Weekday-1]] = d.Shift.ID
	}
	return map[string]any{"name": g.Name, "days": days}
}

// earliestToday is today's business date where it is earliest among the supported zones
// (WIB, which is behind WITA and WIT): from it on, every zone's today is covered.
func earliestToday(now time.Time) timeutil.Date {
	loc, err := time.LoadLocation(timeutil.SupportedTimezones[0])
	if err != nil {
		return timeutil.BusinessDate(now, time.UTC)
	}
	return timeutil.BusinessDate(now, loc)
}

// recordIDAudit writes an audit row about an entity with a numeric id (shifts, groups,
// schedules), with its before and after (either may be nil).
func recordIDAudit(ctx context.Context, q *db.Queries, actor auth.Principal, action, entity string, id int64, before, after map[string]any) error {
	meta := map[string]any{}
	if before != nil {
		meta["before"] = before
	}
	if after != nil {
		meta["after"] = after
	}
	actorID := actor.PersonnelID
	if err := audit.Record(ctx, q, audit.Entry{ActorPersonnelID: &actorID, Action: action, EntityType: entity, EntityID: id, Metadata: meta}); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
