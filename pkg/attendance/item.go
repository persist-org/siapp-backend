package attendance

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// Status is attendances.status.
type Status = db.AttendanceStatus

// Method is how a check-in or check-out was captured.
type Method = db.AttendanceMethod

// Attendance is one shift instance's record, as the API shows it. Latitude and longitude of the
// fixes stay in the database as evidence; the API shows what the fix meant (the distance and
// whether it was inside the area).
type Attendance struct {
	ID          int64
	PersonnelID int64
	TeamCode    string
	ProjectCode string
	WorkDate    timeutil.Date
	ShiftID     int64
	StartsAt    time.Time
	EndsAt      time.Time
	Timezone    string
	Status      Status

	CheckInAt       *time.Time
	CheckInMethod   *Method
	CheckInDistance *int
	CheckInInArea   *bool
	// LateMinutes is how late the check-in was, 0 when on time (see lateMinutes).
	LateMinutes int

	CheckOutAt          *time.Time
	CheckOutMethod      *Method
	CheckOutDistance    *int
	CheckOutInArea      *bool
	CheckOutUpdateCount int

	PersonelSyncStatus db.PersonelSyncStatus
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func toAttendance(r db.Attendance) Attendance {
	a := Attendance{
		ID: r.ID, PersonnelID: r.PersonnelID, TeamCode: r.TeamCode, ProjectCode: r.ProjectCode,
		WorkDate: dateValue(r.WorkDate), ShiftID: r.ShiftID,
		StartsAt: r.ShiftStartsAt.Time, EndsAt: r.ShiftEndsAt.Time, Timezone: r.Timezone, Status: r.Status,
		CheckInAt: timePtr(r.CheckInAt), CheckInMethod: methodPtr(r.CheckInMethod),
		CheckInDistance: intPtr(r.CheckInDistanceMeters), CheckInInArea: boolPtr(r.CheckInWithinArea),
		LateMinutes: int(r.LateMinutes),
		CheckOutAt:  timePtr(r.CheckOutAt), CheckOutMethod: methodPtr(r.CheckOutMethod),
		CheckOutDistance: intPtr(r.CheckOutDistanceMeters), CheckOutInArea: boolPtr(r.CheckOutWithinArea),
		CheckOutUpdateCount: int(r.CheckOutUpdateCount),
		PersonelSyncStatus:  r.PersonelSyncStatus, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	return a
}

// ---------- shift_ref ----------

// shiftKey identifies one shift instance of one person: the regu they serve in, the date the shift
// starts on and the shift. The person is the caller, so it is not part of the key.
type shiftKey struct {
	TeamCode string
	WorkDate timeutil.Date
	ShiftID  int64
}

// ref is the opaque shift_ref the app sends back: base64url of "teamCode|workDate|shiftId".
//
// It is only a handle, not a permission: the usecase looks it up among the shifts the caller can
// check into right now (Checkable), so a ref built by hand for another person's, another regu's or
// another organization's shift matches nothing and is refused like any other stale one.
func (k shiftKey) ref() string {
	raw := k.TeamCode + "|" + k.WorkDate.String() + "|" + strconv.FormatInt(k.ShiftID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// parseShiftRef is the inverse of ref. ok is false for anything that is not a well-formed ref.
func parseShiftRef(ref string) (k shiftKey, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(ref)
	if err != nil {
		return shiftKey{}, false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] == "" {
		return shiftKey{}, false
	}
	day, err := timeutil.ParseDate(parts[1])
	if err != nil {
		return shiftKey{}, false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || id < 1 {
		return shiftKey{}, false
	}
	return shiftKey{TeamCode: parts[0], WorkDate: day, ShiftID: id}, true
}

// ---------- pgtype helpers ----------

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func dateParam(d timeutil.Date) pgtype.Date { return pgtype.Date{Time: d.Time(), Valid: true} }

func dateValue(d pgtype.Date) timeutil.Date {
	if !d.Valid {
		return timeutil.Date{}
	}
	return timeutil.NewDate(d.Time.Year(), d.Time.Month(), d.Time.Day())
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

func methodPtr(m db.NullAttendanceMethod) *Method {
	if !m.Valid {
		return nil
	}
	v := m.AttendanceMethod
	return &v
}

func intPtr(v pgtype.Int4) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int32)
	return &n
}

func boolPtr(v pgtype.Bool) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Bool
	return &b
}

func int8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func formatted(t pgtype.Timestamptz) *string { return stamp(timePtr(t)) }

func methodString(m db.NullAttendanceMethod) *string { return methodName(methodPtr(m)) }

func nullMethod(m Method) db.NullAttendanceMethod {
	return db.NullAttendanceMethod{AttendanceMethod: m, Valid: true}
}
