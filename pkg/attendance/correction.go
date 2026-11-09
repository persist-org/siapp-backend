package attendance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/leave"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// Phase 07.6 Part C: the admin correction (PATCH /admin/attendances/{id}) and the admin monitor
// (GET /admin/attendances).

// CorrectionInput is the body of PATCH /admin/attendances/{id}. Exactly one of three shapes, never
// mixed: `is_absent: true` turns the record into ABSENT; `is_absent: false` undoes an existing
// ABSENT back into PRESENT (check_in required); `check_in`/`check_out` edit the times of an
// already-PRESENT record. validationError below enforces the three stay mutually exclusive.
type CorrectionInput struct {
	CheckIn  *string `json:"check_in"`
	CheckOut *string `json:"check_out"`
	IsAbsent *bool   `json:"is_absent"`
	Note     string  `json:"note" validate:"omitempty,max=1000"`
}

func correctionValidationError(field, message string) error {
	return apperr.ErrValidation.WithFields(apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}

// parseCorrection reads and validates the input; checkIn/checkOut are nil when not sent.
func parseCorrection(in CorrectionInput) (checkIn, checkOut *time.Time, err error) {
	if in.CheckIn == nil && in.CheckOut == nil && in.IsAbsent == nil {
		return nil, nil, correctionValidationError("is_absent", "Isi check_in, check_out, atau is_absent.")
	}
	if in.IsAbsent != nil && (in.CheckIn != nil || in.CheckOut != nil) {
		return nil, nil, correctionValidationError("is_absent", "Tidak boleh dikirim bersama check_in atau check_out.")
	}
	if in.CheckIn != nil {
		t, err := time.Parse(time.RFC3339, *in.CheckIn)
		if err != nil {
			return nil, nil, correctionValidationError("check_in", "Harus RFC3339.")
		}
		checkIn = &t
	}
	if in.CheckOut != nil {
		t, err := time.Parse(time.RFC3339, *in.CheckOut)
		if err != nil {
			return nil, nil, correctionValidationError("check_out", "Harus RFC3339.")
		}
		checkOut = &t
	}
	return checkIn, checkOut, nil
}

// Correct is PATCH /admin/attendances/{id}: an admin's fix of a record after the fact, the part of
// HR's fix requirePreviousSettled's doc comment refers to. Scoped by region
// (access.AdminScope.AllowsRegion of the record's project), same as leave/decide.go's
// decideByAdmin: outside the admin's reach answers 404, the same as a record that does not exist.
func (s *Service) Correct(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, in CorrectionInput) (Attendance, error) {
	checkIn, checkOut, err := parseCorrection(in)
	if err != nil {
		return Attendance{}, err
	}
	now := s.clock.Now()
	var out Attendance
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetAttendanceForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		if !scope.AllowsRegion(access.RegionOfCode(row.ProjectCode)) {
			return ErrNotFound
		}

		params := db.CorrectAttendanceParams{
			Status: row.Status, MarkedAbsentByID: row.MarkedAbsentByID, AbsentReason: row.AbsentReason,
			CheckInAt: row.CheckInAt, CheckInMethod: row.CheckInMethod, LateMinutes: row.LateMinutes,
			CheckOutAt: row.CheckOutAt, CheckOutMethod: row.CheckOutMethod, Now: ts(now), ID: id,
		}
		var eventType string
		switch {
		case in.IsAbsent != nil && *in.IsAbsent:
			if row.Status == db.AttendanceStatusABSENT {
				return correctionValidationError("is_absent", "Catatan ini sudah mangkir.")
			}
			params.Status = db.AttendanceStatusABSENT
			params.MarkedAbsentByID = pgtype.Int8{Int64: p.PersonnelID, Valid: true}
			params.AbsentReason = textOrNull(in.Note)
			params.CheckInAt, params.CheckInMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.CheckOutAt, params.CheckOutMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.LateMinutes = 0
			eventType = eventAbsent

		case in.IsAbsent != nil && !*in.IsAbsent:
			if row.Status != db.AttendanceStatusABSENT {
				return correctionValidationError("is_absent", "Catatan ini tidak sedang mangkir.")
			}
			if checkIn == nil {
				return correctionValidationError("check_in", "Wajib diisi untuk membatalkan mangkir.")
			}
			late, lerr := s.correctedLateness(ctx, q, row, *checkIn)
			if lerr != nil {
				return lerr
			}
			params.Status = db.AttendanceStatusPRESENT
			params.MarkedAbsentByID, params.AbsentReason = pgtype.Int8{}, pgtype.Text{}
			params.CheckInAt, params.CheckInMethod = ts(*checkIn), nullMethod(db.AttendanceMethodFORCE)
			params.LateMinutes = int32(min(late, math.MaxInt32)) //nolint:gosec // clamped
			eventType = eventAbsentCancelled

		default:
			if row.Status != db.AttendanceStatusPRESENT {
				return correctionValidationError("check_in", "Hanya catatan hadir yang bisa dikoreksi jamnya; gunakan is_absent untuk mangkir.")
			}
			if checkIn != nil {
				late, lerr := s.correctedLateness(ctx, q, row, *checkIn)
				if lerr != nil {
					return lerr
				}
				params.CheckInAt, params.CheckInMethod = ts(*checkIn), nullMethod(db.AttendanceMethodFORCE)
				params.LateMinutes = int32(min(late, math.MaxInt32)) //nolint:gosec // clamped
			}
			if checkOut != nil {
				if !params.CheckInAt.Valid {
					return correctionValidationError("check_out", "Catatan ini belum punya jam datang.")
				}
				params.CheckOutAt, params.CheckOutMethod = ts(*checkOut), nullMethod(db.AttendanceMethodFORCE)
			}
			eventType = eventCorrected
		}

		before := correctionSnapshot(row)
		updated, err := q.CorrectAttendance(ctx, params)
		if err != nil {
			return fmt.Errorf("correct attendance: %w", err)
		}
		if err := writeEvent(ctx, q, updated, eventType, now); err != nil {
			return err
		}
		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "attendance.correct", EntityType: "attendance", EntityID: id,
			Metadata: map[string]any{"before": before, "after": correctionSnapshot(updated), "note": in.Note},
		}); err != nil {
			return err
		}
		out = toAttendance(updated)
		return nil
	})
	return out, err
}

// correctedLateness recomputes late_minutes for a check-in an admin sets, the same way a live
// check-in does: against the shift's current tolerance (the shift may since have been edited or
// deleted, in which case 0 tolerance is used, the same as kpi's own behavior for a deleted shift),
// and lowered by an approved permit covering the day (leave.EffectiveLateness).
func (s *Service) correctedLateness(ctx context.Context, q *db.Queries, row db.Attendance, checkIn time.Time) (int, error) {
	var tolerance int
	if sh, err := q.GetShift(ctx, row.ShiftID); err == nil {
		tolerance = int(sh.ToleranceMinutes)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("get shift: %w", err)
	}
	loc, err := time.LoadLocation(row.Timezone)
	if err != nil {
		return 0, fmt.Errorf("load location %s: %w", row.Timezone, err)
	}
	raw := lateMinutes(row.ShiftStartsAt.Time, checkIn, tolerance)
	late, err := leave.EffectiveLateness(ctx, q, row.PersonnelID, dateValue(row.WorkDate), row.ShiftStartsAt.Time, checkIn, raw, loc)
	if err != nil {
		return 0, fmt.Errorf("effective lateness: %w", err)
	}
	return late, nil
}

// correctionSnapshot is what the audit log keeps of a record before and after a correction.
func correctionSnapshot(r db.Attendance) map[string]any {
	return map[string]any{
		"status": string(r.Status), "check_in_at": formatted(r.CheckInAt), "check_out_at": formatted(r.CheckOutAt),
		"late_minutes": r.LateMinutes, "marked_absent_by_id": int8Ptr(r.MarkedAbsentByID),
	}
}

// ---------- the admin monitor (GET /admin/attendances) ----------

// ListForAdmin is GET /admin/attendances: every record within the admin's region reach.
func (s *Service) ListForAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, f Filter) (Page, error) {
	if !scope.All {
		f.RegionCode = scope.RegionCode // a REGION_ADMIN sees their region only, whatever they ask
	}
	var out Page
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		return s.listInto(ctx, q, &out, db.ListAttendancesParams{
			TeamCode: textOrNull(f.TeamCode), ProjectCode: textOrNull(f.ProjectCode), RegionCode: textOrNull(f.RegionCode),
			FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageOffset: int32(f.Offset), PageLimit: int32(f.Limit), //nolint:gosec // bounded by httpx.ParseListQuery
		})
	})
	return out, err
}
