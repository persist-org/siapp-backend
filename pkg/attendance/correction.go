package attendance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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

// CorrectionInput is the body of PATCH /admin/attendances/{id}. `reason` is always required.
// `status` (PRESENT, LEAVE or ABSENT) retargets the record; `check_in_at`/`check_out_at` edit the
// times of a record that is (or is becoming, via `status: PRESENT`) hadir. A single CORRECTED
// event is written to Personel regardless of which of these changed: the event is for "this
// record was corrected by an admin", not for the particular transition (unlike the DANRU's own
// ABSENT/ABSENT_CANCELLED on /attendances/absent).
type CorrectionInput struct {
	Status   *string `json:"status"`
	CheckIn  *string `json:"check_in_at"`
	CheckOut *string `json:"check_out_at"`
	Reason   string  `json:"reason" validate:"required,max=1000"`
}

func correctionValidationError(field, message string) error {
	return apperr.ErrValidation.WithFields(apperr.FieldError{Field: field, Code: "INVALID", Message: message})
}

// parseCorrection reads and validates the input; status, checkIn and checkOut are nil when not
// sent. It cannot check status against the record's own (that needs the row), so Correct does the
// rest once it has it.
func parseCorrection(in CorrectionInput) (status *db.AttendanceStatus, checkIn, checkOut *time.Time, err error) {
	if strings.TrimSpace(in.Reason) == "" {
		return nil, nil, nil, correctionValidationError("reason", "Alasan koreksi wajib diisi.")
	}
	if in.Status == nil && in.CheckIn == nil && in.CheckOut == nil {
		return nil, nil, nil, correctionValidationError("status", "Isi status, check_in_at, atau check_out_at.")
	}
	if in.Status != nil {
		st := db.AttendanceStatus(strings.ToUpper(*in.Status))
		switch st {
		case db.AttendanceStatusPRESENT, db.AttendanceStatusLEAVE, db.AttendanceStatusABSENT:
			status = &st
		default:
			return nil, nil, nil, correctionValidationError("status", "Harus PRESENT, LEAVE, atau ABSENT.")
		}
		if st != db.AttendanceStatusPRESENT && (in.CheckIn != nil || in.CheckOut != nil) {
			return nil, nil, nil, correctionValidationError("status", "check_in_at/check_out_at hanya berlaku untuk status PRESENT.")
		}
	}
	if in.CheckIn != nil {
		t, perr := time.Parse(time.RFC3339, *in.CheckIn)
		if perr != nil {
			return nil, nil, nil, correctionValidationError("check_in_at", "Harus RFC3339.")
		}
		checkIn = &t
	}
	if in.CheckOut != nil {
		t, perr := time.Parse(time.RFC3339, *in.CheckOut)
		if perr != nil {
			return nil, nil, nil, correctionValidationError("check_out_at", "Harus RFC3339.")
		}
		checkOut = &t
	}
	return status, checkIn, checkOut, nil
}

// Correct is PATCH /admin/attendances/{id}: an admin's fix of a record after the fact, the part of
// HR's fix requirePreviousSettled's doc comment refers to. Scoped by region
// (access.AdminScope.AllowsRegion of the record's project), same as leave/decide.go's
// decideByAdmin: outside the admin's reach answers 404, the same as a record that does not exist.
func (s *Service) Correct(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, in CorrectionInput) (Attendance, error) {
	status, checkIn, checkOut, err := parseCorrection(in)
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

		newStatus := row.Status
		if status != nil {
			newStatus = *status
		} else if row.Status != db.AttendanceStatusPRESENT && (checkIn != nil || checkOut != nil) {
			return correctionValidationError("check_in_at", "Catatan ini bukan hadir; sertakan status: PRESENT untuk mengoreksi jamnya.")
		}

		params := db.CorrectAttendanceParams{
			Status: newStatus, MarkedAbsentByID: row.MarkedAbsentByID, AbsentReason: row.AbsentReason,
			CheckInAt: row.CheckInAt, CheckInMethod: row.CheckInMethod, LateMinutes: row.LateMinutes,
			CheckOutAt: row.CheckOutAt, CheckOutMethod: row.CheckOutMethod, Now: ts(now), ID: id,
		}
		switch newStatus {
		case db.AttendanceStatusABSENT:
			params.MarkedAbsentByID = pgtype.Int8{Int64: p.PersonnelID, Valid: true}
			params.AbsentReason = textOrNull(in.Reason)
			params.CheckInAt, params.CheckInMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.CheckOutAt, params.CheckOutMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.LateMinutes = 0
		case db.AttendanceStatusLEAVE:
			params.MarkedAbsentByID, params.AbsentReason = pgtype.Int8{}, pgtype.Text{}
			params.CheckInAt, params.CheckInMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.CheckOutAt, params.CheckOutMethod = pgtype.Timestamptz{}, db.NullAttendanceMethod{}
			params.LateMinutes = 0
		case db.AttendanceStatusPRESENT:
			params.MarkedAbsentByID, params.AbsentReason = pgtype.Int8{}, pgtype.Text{}
			if checkIn != nil {
				late, lerr := s.correctedLateness(ctx, q, row, *checkIn)
				if lerr != nil {
					return lerr
				}
				params.CheckInAt, params.CheckInMethod = ts(*checkIn), nullMethod(db.AttendanceMethodFORCE)
				params.LateMinutes = int32(min(late, math.MaxInt32)) //nolint:gosec // clamped
			}
			if !params.CheckInAt.Valid {
				return correctionValidationError("check_in_at", "Wajib diisi agar catatan ini menjadi hadir.")
			}
			if checkOut != nil {
				params.CheckOutAt, params.CheckOutMethod = ts(*checkOut), nullMethod(db.AttendanceMethodFORCE)
			}
			if params.CheckOutAt.Valid && params.CheckOutAt.Time.Before(params.CheckInAt.Time) {
				return correctionValidationError("check_out_at", "Tidak boleh lebih awal dari check_in_at.")
			}
		}

		// ⚠ A swap covers an absent or on-leave record. Once the guard is PRESENT it covers nothing:
		// cancel it, or refuse when the replacer already worked the shift (both would be credited).
		if newStatus == db.AttendanceStatusPRESENT && row.Status != db.AttendanceStatusPRESENT {
			if err := releaseSwaps(ctx, q, row.ID, now); err != nil {
				return err
			}
		}

		before := correctionSnapshot(row)
		updated, err := q.CorrectAttendance(ctx, params)
		if err != nil {
			return fmt.Errorf("correct attendance: %w", err)
		}
		if err := writeEvent(ctx, q, updated, eventCorrected, now); err != nil {
			return err
		}
		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "attendance.correct", EntityType: "attendance", EntityID: id,
			Metadata: map[string]any{"before": before, "after": correctionSnapshot(updated), "reason": in.Reason},
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
			Status: textOrNull(f.Status), SyncStatus: textOrNull(f.SyncStatus),
			FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To),
			PageOffset: int32(f.Offset), PageLimit: int32(f.Limit), //nolint:gosec // bounded by httpx.ParseListQuery
		})
	})
	return out, err
}
