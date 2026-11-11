package attendance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/leave"
	"siapp/pkg/media"
	"siapp/pkg/organization"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/geo"
	"siapp/pkg/platform/timeutil"
)

// The other modules this one asks, as small interfaces of its own (README: cross-domain
// dependencies). cmd/api wires the real ones.
type (
	// shiftSource is organization.Shifts.
	shiftSource interface {
		Current(ctx context.Context, organizationID, personnelID int64, at time.Time) ([]organization.ShiftInstance, error)
		Instances(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]organization.ShiftInstance, error)
		Available(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]organization.ShiftInstance, error)
	}
	// siteSource is organization.Query: a regu with its proyek, a proyek by code inside the
	// caller's transaction, and (07.7) the regu a person serves in on a date.
	siteSource interface {
		TeamWithProject(ctx context.Context, organizationID int64, teamCode string) (organization.TeamWithProject, error)
		ProjectIn(ctx context.Context, q *db.Queries, code string) (organization.Project, error)
		ActiveMemberships(ctx context.Context, organizationID, personnelID int64, on timeutil.Date) ([]organization.Membership, error)
	}
	// photoStore is media.Service.
	photoStore interface {
		Attach(ctx context.Context, q *db.Queries, in media.AttachInput) error
		Views(ctx context.Context, q *db.Queries, entityType string, entityID int64, ids []int64) ([]media.View, error)
	}
	// permissions is access.Service.
	permissions interface {
		Resolve(ctx context.Context, q *db.Queries, personnelID int64, perm access.Permission, on timeutil.Date) (access.Resolution, error)
	}
)

// Service is the attendance usecases.
type Service struct {
	pool   *pgxpool.Pool
	clock  clock.Clock
	shifts shiftSource
	sites  siteSource
	photos photoStore
	access permissions
	// qr is nil while ATTENDANCE_QR_SECRET is not set: the QR endpoints then answer 503.
	qr *QRTokens
	// notifier is nil until cmd/api plugs one in (UseNotifier): nothing is then sent.
	notifier Notifier
}

// NewService returns the Service.
func NewService(pool *pgxpool.Pool, c clock.Clock, shifts shiftSource, sites siteSource, photos photoStore, perms permissions) *Service {
	return &Service{pool: pool, clock: c, shifts: shifts, sites: sites, photos: photos, access: perms}
}

// UseQR turns the QR method on: tokens are issued and verified with qr. Called once at start-up.
func (s *Service) UseQR(qr *QRTokens) { s.qr = qr }

// capture is what a check-in or check-out brings besides the shift: how it was captured and the
// evidence. The selfie endpoints build one with method SELFIE; Phase 07.2's QR endpoints build one
// with method QR and the DANRU, and call the same checkIn and checkOut: the rules are one piece
// of code, and only the evidence differs.
type capture struct {
	Method   Method
	MediaIDs []int64
	// Fix is where the phone says it is. nil when none was sent (QR, where the DANRU's presence is
	// the proof).
	Fix            *geo.Point
	AccuracyMeters *float64
	// QRDanruID is the DANRU whose QR was scanned.
	QRDanruID *int64
}

// measurement is what a fix means against the project's geofence.
type measurement struct {
	distance pgtype.Int4
	inArea   pgtype.Bool
}

// measure compares the fix with the project's site.
//
// ⚠ The geofence is the PROJECT's: its point and attendance_radius_meters. Regu have no geofence of
// their own (D20: one proyek has one address, and all its personnel work there), which is why
// this takes the project and not the regu. Inside the radius is within_area = true. Outside it:
// with strict_attendance_geofence the capture is refused (ATTENDANCE_OUTSIDE_AREA); without it the
// record is saved with within_area = false, so the DANRU and the admins see the doubt and decide
// (DL-48). A project whose point is not entered is PROJECT_NOT_CONFIGURED: nobody can check in
// where the site is unknown. The GPS accuracy is kept as evidence but never used to excuse a
// distance: a loose fix is exactly what a faked one looks like.
func measure(project organization.Project, fix *geo.Point) (measurement, error) {
	if fix == nil {
		return measurement{}, nil
	}
	if !project.Configured() {
		return measurement{}, organization.ErrProjectNotConfigured
	}
	d := geo.DistanceMeters(*fix, geo.Point{Lat: *project.Latitude, Lng: *project.Longitude})
	inArea := d <= float64(project.AttendanceRadiusMeters)
	if !inArea && project.StrictAttendanceGeofence {
		return measurement{}, ErrOutsideArea.WithDetails(map[string]any{
			"distance_meters": int(math.Round(d)), "radius_meters": project.AttendanceRadiusMeters,
		})
	}
	return measurement{
		distance: pgtype.Int4{Int32: int32(math.Round(d)), Valid: true}, //nolint:gosec // at most half the earth's circumference, 2e7
		inArea:   pgtype.Bool{Bool: inArea, Valid: true},
	}, nil
}

// allow checks the permission for the shift's work date, inside the transaction that writes. The
// route's Require middleware only asks about today; a shift that started yesterday (an overnight
// one) is decided here, by the date of the record.
func (s *Service) allow(ctx context.Context, q *db.Queries, personnelID int64, day timeutil.Date) error {
	res, err := s.access.Resolve(ctx, q, personnelID, access.AttendanceCheckin, day)
	if err != nil {
		return err
	}
	if !res.Allowed {
		return access.ErrPermissionDenied
	}
	return nil
}

func accuracyParam(v *float64) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(math.Min(math.Round(*v), math.MaxInt32)), Valid: true}
}

func fixParams(fix *geo.Point) (lat, lng *float64) {
	if fix == nil {
		return nil, nil
	}
	return &fix.Lat, &fix.Lng
}

func int8Param(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// checkIn records the person's check-in into the shift the ref names.
//
// ⚠ Previous working day. A check-in is refused while the person's previous working day is
// unsettled (requirePreviousSettled), as in kpi-personalia-api.
//
// ⚠ Late check-in. A late guard is never refused: the check-in is accepted and the record says how
// late it was (late_minutes, see lateMinutes). The regu's reject_attendance_after_tolerance toggle
// (DL-48) is no longer read here.
//
// ⚠ Double check-in. The row is inserted with ON CONFLICT DO NOTHING on the instance's unique key,
// not inserted and caught: a unique violation would abort the transaction before the existing row
// could be read. Two parallel check-ins race on the key; the loser waits for the winner to
// commit, gets no row back and reads the winner's, so it answers ALREADY_CHECKED_IN and writes no
// outbox row. A row that is a LEAVE or an ABSENT is not the guard's to overwrite: NOT_ALLOWED.
//
// The checks run from the most basic to the most specific, so each caller gets the answer that is
// true of them first: the permission (403), then a record the shift already has (ALREADY_CHECKED_IN
// or NOT_ALLOWED, whatever the fix says), then the previous working day, and only then the geofence.
func (s *Service) checkIn(ctx context.Context, p auth.Principal, ref string, c capture) (Attendance, error) {
	now := s.clock.Now()
	shift, err := s.find(ctx, p.OrganizationID, p.PersonnelID, ref, now)
	if err != nil {
		return Attendance{}, err
	}

	var out Attendance
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if err := s.allow(ctx, q, p.PersonnelID, shift.Key.WorkDate); err != nil {
			return err
		}
		// ⚠ A covered shift: s.find read the swap before this transaction, so a DANRU may have
		// cancelled it since. Lock it and re-read. CancelSwap locks the same row, so either this
		// check-in commits first (and the cancel then answers SWAP_ALREADY_STARTED) or the cancel does
		// (and this answers SHIFT_NOT_ACTIVE): never a record pointing at a cancelled swap.
		if shift.SwapID != nil {
			sw, serr := q.GetShiftSwapForUpdate(ctx, *shift.SwapID)
			if errors.Is(serr, pgx.ErrNoRows) || (serr == nil && sw.CancelledAt.Valid) {
				return ErrShiftNotActive
			}
			if serr != nil {
				return fmt.Errorf("get shift swap: %w", serr)
			}
		}
		existing, err := existingRecord(ctx, q, p.PersonnelID, shift.Key)
		if err != nil {
			return err
		}
		if existing != nil {
			return refusalFor(*existing)
		}
		if err := requirePreviousSettled(ctx, q, p.PersonnelID, shift.Key.WorkDate); err != nil {
			return err
		}
		m, err := measure(shift.Project, c.Fix)
		if err != nil {
			return err
		}
		// ⚠ Two figures: what the clock said (raw, kept so it can be computed from again) and what the
		// person is held to, which an approved permit covering the day can lower (07.5 Part C).
		raw := lateMinutes(shift.StartsAt, now, shift.ToleranceMinutes)
		late, err := leave.EffectiveLateness(ctx, q, p.PersonnelID, shift.Key.WorkDate, shift.StartsAt, now, raw, shift.Location)
		if err != nil {
			return fmt.Errorf("effective lateness: %w", err)
		}
		lat, lng := fixParams(c.Fix)
		row, err := q.InsertAttendanceCheckIn(ctx, db.InsertAttendanceCheckInParams{
			PersonnelID: p.PersonnelID, TeamCode: shift.Key.TeamCode, ProjectCode: shift.Project.Code,
			WorkDate: dateParam(shift.Key.WorkDate), ShiftID: shift.Key.ShiftID,
			ShiftStartsAt: ts(shift.StartsAt), ShiftEndsAt: ts(shift.EndsAt), Timezone: shift.Location.String(),
			Now: ts(now), Method: nullMethod(c.Method), Latitude: lat, Longitude: lng,
			AccuracyMeters: accuracyParam(c.AccuracyMeters), DistanceMeters: m.distance, WithinArea: m.inArea,
			QrDanruID:      int8Param(c.QRDanruID),
			LateMinutes:    int32(min(late, math.MaxInt32)), //nolint:gosec // clamped
			LateMinutesRaw: int32(min(raw, math.MaxInt32)),  //nolint:gosec // clamped
			ShiftSwapID:    int8Param(shift.SwapID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// A parallel check-in committed between the read above and this insert.
			return s.whyNotCheckedIn(ctx, q, p.PersonnelID, shift.Key)
		}
		if err != nil {
			return fmt.Errorf("insert attendance: %w", err)
		}
		if len(c.MediaIDs) > 0 {
			if err := s.photos.Attach(ctx, q, media.AttachInput{
				MediaIDs: c.MediaIDs, OwnerID: p.PersonnelID, Purpose: media.PurposeAttendanceSelfie,
				EntityType: entityType, EntityID: row.ID, Max: selfiesPerCapture,
			}); err != nil {
				return err
			}
			if err := q.SetAttendanceCheckInMedia(ctx, db.SetAttendanceCheckInMediaParams{ID: row.ID, MediaID: pgtype.Int8{Int64: c.MediaIDs[0], Valid: true}}); err != nil {
				return fmt.Errorf("set check-in photo: %w", err)
			}
			row.CheckInMediaID = pgtype.Int8{Int64: c.MediaIDs[0], Valid: true}
		}
		if err := writeEvent(ctx, q, row, eventCheckIn, now); err != nil {
			return err
		}
		out = toAttendance(row)
		return nil
	})
	return out, err
}

// requirePreviousSettled refuses a check-in into a shift on workDate while the person's previous
// working day (their latest earlier scheduled day that is not an OFF day) is not settled. The whole
// day is judged, every record of it whatever its shift or regu, because the schedule says only
// that the day was worked: the shift of a record may be one the schedule was moved away from, and a
// person may serve in two regu on one day.
//   - Any PRESENT record without a check-out: ATTENDANCE_PREVIOUS_NOT_CHECKED_OUT (one regu left
//     open is open, even when another was closed).
//   - A request of leave, a permit or sick leave that is still alive (PENDING or APPROVED, any
//     category) covering the day settles it, before anything else is looked at.
//   - Otherwise any record at all settles it: a PRESENT one with its check-out, an approved LEAVE,
//     or an ABSENT. ABSENT is only ever written by someone deciding the day (the DANRU marking the
//     absence, an admin's correction): the day has been dealt with, and blocking on it would turn
//     every absence decision into a lockout until someone undid it.
//   - Otherwise (no record: the unexplained absence, alpha): ATTENDANCE_PREVIOUS_NOT_CHECKED_IN, but
//     only once the person has checked in through SiAPP for some day before it. Before their first
//     check-in SiAPP has no history to judge: on go-live day every guard's last working day has no
//     record (it was kept by the old app), and so does a new hire's or a back-dated schedule's.
//
// ⚠ This ports kpi-personalia-api's guards against clocking in over an open day (the previous day
// left open, and the unexcused absence). An approved leave plays the part of its permit (07.5), an
// admin's correction (07.6) the part of HR's fix. Only the one previous working day is looked at,
// not a 14-day window. The rule is for check-in only: check-out and "update pulang" are never refused
// for it, so a guard can always finish the day they are in.
func requirePreviousSettled(ctx context.Context, q *db.Queries, personnelID int64, workDate timeutil.Date) error {
	prev, err := q.GetPreviousWorkingDay(ctx, db.GetPreviousWorkingDayParams{
		PersonnelID: personnelID, Before: dateParam(workDate),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("previous working day: %w", err)
	}
	// A pending or approved request of any category covering the day explains it, whatever its record
	// looks like (a PRESENT row left open included): the person has dealt with the day (07.5).
	covered, err := q.HasLiveLeaveCovering(ctx, db.HasLiveLeaveCoveringParams{PersonnelID: personnelID, OnDate: prev})
	if err != nil {
		return fmt.Errorf("previous day leave: %w", err)
	}
	if covered {
		return nil
	}
	rows, err := q.ListAttendancesOfPersonnelBetween(ctx, db.ListAttendancesOfPersonnelBetweenParams{
		PersonnelID: personnelID, DateFrom: prev, DateTo: prev,
	})
	if err != nil {
		return fmt.Errorf("previous attendance: %w", err)
	}
	details := map[string]any{"previous_work_date": dateValue(prev).String()}
	for _, r := range rows {
		if r.Status == db.AttendanceStatusPRESENT && !r.CheckOutAt.Valid {
			return ErrPreviousNotCheckedOut.WithDetails(details)
		}
	}
	if len(rows) > 0 {
		return nil
	}
	judged, err := q.HasCheckInBefore(ctx, db.HasCheckInBeforeParams{PersonnelID: personnelID, Before: prev})
	if err != nil {
		return fmt.Errorf("first check-in: %w", err)
	}
	if !judged {
		return nil
	}
	return ErrPreviousNotCheckedIn.WithDetails(details)
}

// lateMinutes is how late a check-in at `at` is for a shift that starts at `start`, in whole minutes.
// A check-in up to the tolerance after the start is on time (0). Past it, the lateness is counted
// from the start of the shift, not from the end of the tolerance: with a tolerance of 15, 08:16 for
// an 08:00 shift is 16 minutes late. A partial minute is not counted (08:16:59 is 16).
func lateMinutes(start, at time.Time, toleranceMinutes int) int {
	late := at.Sub(start)
	if late <= time.Duration(toleranceMinutes)*time.Minute {
		return 0
	}
	return int(late / time.Minute)
}

// existingRecord is the record the shift instance already has, nil when it has none.
func existingRecord(ctx context.Context, q *db.Queries, personnelID int64, key shiftKey) (*db.Attendance, error) {
	row, err := q.GetAttendanceOfInstance(ctx, db.GetAttendanceOfInstanceParams{
		PersonnelID: personnelID, TeamCode: key.TeamCode, WorkDate: dateParam(key.WorkDate), ShiftID: key.ShiftID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get attendance: %w", err)
	}
	return &row, nil
}

// refusalFor is why a check-in cannot take a place that already has a record: a presence is
// ALREADY_CHECKED_IN, a leave or an absence is NOT_ALLOWED.
func refusalFor(existing db.Attendance) error {
	if existing.Status != db.AttendanceStatusPRESENT {
		return ErrNotAllowed
	}
	return ErrAlreadyCheckedIn
}

// whyNotCheckedIn explains an insert that found its place taken by a parallel check-in.
func (s *Service) whyNotCheckedIn(ctx context.Context, q *db.Queries, personnelID int64, key shiftKey) error {
	existing, err := existingRecord(ctx, q, personnelID, key)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrAlreadyCheckedIn
	}
	return refusalFor(*existing)
}

// checkOut records the person's check-out of the shift the ref names. The record is locked, so two
// check-outs (or a check-out and an "update pulang") take turns, and the second one sees the first.
func (s *Service) checkOut(ctx context.Context, p auth.Principal, ref string, c capture) (Attendance, error) {
	now := s.clock.Now()
	shift, err := s.find(ctx, p.OrganizationID, p.PersonnelID, ref, now)
	if err != nil {
		return Attendance{}, err
	}

	var out Attendance
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if err := s.allow(ctx, q, p.PersonnelID, shift.Key.WorkDate); err != nil {
			return err
		}
		row, err := q.GetAttendanceOfInstanceForUpdate(ctx, db.GetAttendanceOfInstanceForUpdateParams{
			PersonnelID: p.PersonnelID, TeamCode: shift.Key.TeamCode, WorkDate: dateParam(shift.Key.WorkDate), ShiftID: shift.Key.ShiftID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotCheckedIn
		}
		if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		if row.Status != db.AttendanceStatusPRESENT {
			return ErrNotCheckedIn
		}
		if row.CheckOutAt.Valid {
			return ErrAlreadyCheckedOut
		}
		m, err := measure(shift.Project, c.Fix)
		if err != nil {
			return err
		}
		row, err = s.writeCheckOut(ctx, q, p, row, c, m, now, 0)
		if err != nil {
			return err
		}
		if err := writeEvent(ctx, q, row, eventCheckOut, now); err != nil {
			return err
		}
		out = toAttendance(row)
		return nil
	})
	return out, err
}

// writeCheckOut attaches the photo and sets the check-out fields of a locked row. bump is how much
// check_out_update_count grows: 0 for the first check-out, 1 for an "update pulang".
func (s *Service) writeCheckOut(ctx context.Context, q *db.Queries, p auth.Principal, row db.Attendance, c capture, m measurement, now time.Time, bump int32) (db.Attendance, error) {
	var mediaID pgtype.Int8
	if len(c.MediaIDs) > 0 {
		if err := s.photos.Attach(ctx, q, media.AttachInput{
			MediaIDs: c.MediaIDs, OwnerID: p.PersonnelID, Purpose: media.PurposeAttendanceSelfie,
			EntityType: entityType, EntityID: row.ID, Max: selfiesPerCapture,
		}); err != nil {
			return db.Attendance{}, err
		}
		mediaID = pgtype.Int8{Int64: c.MediaIDs[0], Valid: true}
	}
	lat, lng := fixParams(c.Fix)
	updated, err := q.SetAttendanceCheckOut(ctx, db.SetAttendanceCheckOutParams{
		ID: row.ID, At: ts(now), Method: nullMethod(c.Method), MediaID: mediaID, Latitude: lat, Longitude: lng,
		AccuracyMeters: accuracyParam(c.AccuracyMeters), DistanceMeters: m.distance, WithinArea: m.inArea,
		QrDanruID: int8Param(c.QRDanruID), Bump: bump, Now: ts(now),
	})
	if err != nil {
		return db.Attendance{}, fmt.Errorf("set check-out: %w", err)
	}
	return updated, nil
}

// updateCheckOut is "update pulang": the person overwrites their own check-out, for example after
// leaving later than they tapped. The old values go to the audit log, the new ones to Personel as a
// CHECK_OUT_UPDATED event, and check_out_update_count says how often it happened.
//
// It is only for the record's owner and only while the shift's check-out window is open (the
// window organization.Shifts.Current uses), so the record cannot be rewritten weeks later. Anyone
// else's record, and another organization's, is ATTENDANCE_NOT_FOUND.
//
// The geofence is the one of the project saved on the record, read in the same transaction that
// locks it, after the window and status checks: one transaction, and the answers that do not need
// the fix come first.
func (s *Service) updateCheckOut(ctx context.Context, p auth.Principal, id int64, c capture) (Attendance, error) {
	now := s.clock.Now()
	var out Attendance
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetAttendanceForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.PersonnelID != p.PersonnelID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get attendance: %w", err)
		}
		if err := s.allow(ctx, q, p.PersonnelID, dateValue(row.WorkDate)); err != nil {
			return err
		}
		if !row.CheckOutAt.Valid {
			return ErrNotCheckedOut
		}
		// The same window organization.Shifts.Current leaves for a check-out.
		if now.After(row.ShiftEndsAt.Time.Add(organization.CheckOutLate)) {
			return ErrShiftNotActive
		}
		project, err := s.sites.ProjectIn(ctx, q, row.ProjectCode)
		if err != nil {
			return err
		}
		m, err := measure(project, c.Fix)
		if err != nil {
			return err
		}
		before := checkOutValues(row)
		updated, err := s.writeCheckOut(ctx, q, p, row, c, m, now, 1)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &p.PersonnelID, Action: "attendance.check_out.update", EntityType: "attendance", EntityID: row.ID,
			Metadata: map[string]any{"before": before, "check_out_update_count": updated.CheckOutUpdateCount},
		}); err != nil {
			return err
		}
		if err := writeEvent(ctx, q, updated, eventCheckOutUpdated, now); err != nil {
			return err
		}
		out = toAttendance(updated)
		return nil
	})
	return out, err
}

// checkOutValues is the check-out as the audit log keeps it before "update pulang" overwrites it.
func checkOutValues(r db.Attendance) map[string]any {
	return map[string]any{
		"check_out_at": formatted(r.CheckOutAt), "method": methodString(r.CheckOutMethod),
		"media_id": int8Ptr(r.CheckOutMediaID), "distance_meters": intPtr(r.CheckOutDistanceMeters),
		"within_area": boolPtr(r.CheckOutWithinArea), "qr_danru_id": int8Ptr(r.CheckOutQrDanruID),
	}
}

// ---------- the endpoints' usecases ----------

// SelfieInput is the body of the selfie check-in and check-out.
type SelfieInput struct {
	ShiftRef       string   `json:"shift_ref" validate:"required"`
	MediaIDs       []int64  `json:"media_ids" validate:"required,len=1,dive,min=1"`
	Latitude       *float64 `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"omitempty,gte=0,lte=100000"`
}

func (in SelfieInput) capture() capture {
	return capture{
		Method: db.AttendanceMethodSELFIE, MediaIDs: in.MediaIDs,
		Fix: &geo.Point{Lat: *in.Latitude, Lng: *in.Longitude}, AccuracyMeters: in.AccuracyMeters,
	}
}

// UpdateInput is the body of "update pulang": a fresh selfie and fix for the new check-out.
type UpdateInput struct {
	MediaIDs       []int64  `json:"media_ids" validate:"required,len=1,dive,min=1"`
	Latitude       *float64 `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"omitempty,gte=0,lte=100000"`
}

// Today is GET /attendances/today: the shifts the caller can check into now.
func (s *Service) Today(ctx context.Context, p auth.Principal) ([]CheckableShift, error) {
	return s.Checkable(ctx, p.OrganizationID, p.PersonnelID, s.clock.Now())
}

// SelfieCheckIn is POST /attendances/selfie/check-in.
func (s *Service) SelfieCheckIn(ctx context.Context, p auth.Principal, in SelfieInput) (Attendance, error) {
	return s.checkIn(ctx, p, in.ShiftRef, in.capture())
}

// SelfieCheckOut is POST /attendances/selfie/check-out.
func (s *Service) SelfieCheckOut(ctx context.Context, p auth.Principal, in SelfieInput) (Attendance, error) {
	return s.checkOut(ctx, p, in.ShiftRef, in.capture())
}

// UpdateSelfieCheckOut is PATCH /attendances/{id}/check-out.
func (s *Service) UpdateSelfieCheckOut(ctx context.Context, p auth.Principal, id int64, in UpdateInput) (Attendance, error) {
	return s.updateCheckOut(ctx, p, id, SelfieInput{
		MediaIDs: in.MediaIDs, Latitude: in.Latitude, Longitude: in.Longitude, AccuracyMeters: in.AccuracyMeters,
	}.capture())
}
