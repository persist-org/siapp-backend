package attendance_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
)

// presigner is the storage the attendance tests run against: nothing is stored, a photo's URL is
// just derived from its key.
type presigner struct{ media.Storage }

func (presigner) PresignGet(_ context.Context, key, _ string, _ time.Duration) (string, error) {
	return "https://files.test/" + key + "?sig=x", nil
}

type historyShift struct {
	AttendanceID  *int64  `json:"attendance_id"`
	ShiftName     string  `json:"shift_name"`
	TeamCode      string  `json:"team_code"`
	TeamName      string  `json:"team_name"`
	Status        string  `json:"status"`
	CheckInAt     *string `json:"check_in_at"`
	CheckInMethod *string `json:"check_in_method"`
	CheckOutAt    *string `json:"check_out_at"`
}

type historyDay struct {
	WorkDate string         `json:"work_date"`
	Shifts   []historyShift `json:"shifts"`
}

type selfie struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

type detail struct {
	record
	ProjectName    string   `json:"project_name"`
	TeamName       string   `json:"team_name"`
	ShiftName      string   `json:"shift_name"`
	DanruNames     []string `json:"danru_names"`
	CheckInSelfie  *selfie  `json:"check_in_selfie"`
	CheckOutSelfie *selfie  `json:"check_out_selfie"`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (e *env) month(token, month string) []historyDay {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/attendances/me?month="+month, nil, "")
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]historyDay](e.t, r)
}

// schedule gives the guard a shift on a date.
func (e *env) schedule(w world, shiftID int64, date string) {
	e.t.Helper()
	e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", w.personID, date, shiftID)
}

func TestMonthMergesTheScheduleWithTheRecords(t *testing.T) {
	e := newEnv(t)
	w := e.world() // Day Shift on 2026-03-10, which is "now"

	var off, night int64
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, is_off) VALUES ('Libur', 0, 0, true) RETURNING id").Scan(&off))
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes) VALUES ('Night Shift', 1320, 360, 15) RETURNING id").Scan(&night))
	})
	e.schedule(w, w.shiftID, "2026-03-09") // ended, nothing captured
	e.schedule(w, w.shiftID, "2026-03-11") // not started
	e.schedule(w, off, "2026-03-12")       // a day off is not a shift
	e.schedule(w, night, "2026-03-13")     // overnight: belongs to the 13th
	e.schedule(w, w.shiftID, "2026-04-01") // another month

	in := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	recordID := data[record](t, in).ID

	days := e.month(w.token, "2026-03")
	got := map[string]historyShift{}
	var order []string
	for _, d := range days {
		order = append(order, d.WorkDate)
		require.Len(t, d.Shifts, 1, d.WorkDate)
		got[d.WorkDate] = d.Shifts[0]
	}
	require.Equal(t, []string{"2026-03-09", "2026-03-10", "2026-03-11", "2026-03-13"}, order, "ascending, no day off, no other month")

	require.Equal(t, "MISSING", got["2026-03-09"].Status)
	require.Nil(t, got["2026-03-09"].AttendanceID, "computed, not stored")
	require.Equal(t, 0, e.count(w.org, "SELECT count(*) FROM attendances WHERE work_date <> '2026-03-10'"))

	present := got["2026-03-10"]
	require.Equal(t, "PRESENT", present.Status)
	require.Equal(t, recordID, *present.AttendanceID)
	require.Equal(t, "Day Shift", present.ShiftName)
	require.Equal(t, "1K-IBMI-S1", present.TeamCode)
	require.Equal(t, "Regu 1", present.TeamName)
	require.Equal(t, "SELFIE", *present.CheckInMethod)
	require.NotNil(t, present.CheckInAt)
	require.Nil(t, present.CheckOutAt)

	require.Equal(t, "UPCOMING", got["2026-03-11"].Status)
	require.Equal(t, "Night Shift", got["2026-03-13"].ShiftName)
	require.Equal(t, "UPCOMING", got["2026-03-13"].Status)

	// Time passes: yesterday's gap stays a gap, and the shifts that have ended turn into MISSING.
	e.clock.Set(time.Date(2026, 3, 14, 3, 0, 0, 0, time.UTC))
	days = e.month(w.token, "2026-03")
	require.Equal(t, "MISSING", days[2].Shifts[0].Status, "the 11th ended")
	require.Equal(t, "MISSING", days[3].Shifts[0].Status, "the overnight shift of the 13th ended at 06:00 on the 14th")
	require.Equal(t, "PRESENT", days[1].Shifts[0].Status, "a record is not overwritten by the computed statuses")

	// What was captured is never hidden: the schedule of the 10th is removed after the fact.
	e.exec(w.org, "DELETE FROM schedules WHERE work_date = '2026-03-10'")
	days = e.month(w.token, "2026-03")
	require.Len(t, days, 4)
	require.Equal(t, "PRESENT", days[1].Shifts[0].Status)
	require.Equal(t, "Day Shift", days[1].Shifts[0].ShiftName)

	// Another month holds only its own.
	require.Len(t, e.month(w.token, "2026-04"), 1)
	require.Empty(t, e.month(w.token, "2025-01"))
}

func TestMonthIsTheCallersOwn(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, other := e.guard(w.org, w.shiftID)
	e.exec(w.org, "DELETE FROM schedules WHERE personnel_id = $1", w.personID)

	require.Empty(t, e.month(w.token, "2026-03"), "no schedule, no record: nothing, and nothing of the other guard's")
	require.Len(t, e.month(other, "2026-03"), 1)
}

func TestMonthNeedsAValidMonth(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	for _, path := range []string{"/attendances/me", "/attendances/me?month=", "/attendances/me?month=2026-13", "/attendances/me?month=2026-3", "/attendances/me?month=2026-03-10"} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.token, http.MethodGet, path, nil, "").Status, path)
	}
}

func TestDetailShowsTheRecordItsNamesDanruAndSelfies(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	danruID, danruToken := e.danru(w.org, "1K-IBMI-S1")
	e.exec(w.org, "UPDATE personnels SET full_name = 'Pak Danru' WHERE id = $1", danruID)
	ref := e.onlyShift(w.token).ShiftRef

	// A QR check-in has no selfie; the DANRU who showed the code is named.
	qr := e.qrScan(w, "check-in", ref, e.qrCodeOf(danruToken, "1K-IBMI-S1", "IN"))
	require.Equal(t, http.StatusCreated, qr.Status, "body: %s", qr.Raw)
	id := data[record](t, qr).ID

	r := e.api(w.token, http.MethodGet, "/attendances/"+itoa(id), nil, "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	d := data[detail](t, r)
	require.Equal(t, id, d.ID)
	require.Equal(t, "Proyek IBMI", d.ProjectName)
	require.Equal(t, "Regu 1", d.TeamName)
	require.Equal(t, "Day Shift", d.ShiftName)
	require.Equal(t, []string{"Pak Danru"}, d.DanruNames)
	require.Equal(t, "QR", *d.CheckInMethod)
	require.Nil(t, d.CheckInSelfie)
	require.Nil(t, d.CheckOutSelfie)

	// A selfie check-out brings its photo, and a signed URL for it.
	e.clock.Advance(8 * time.Hour)
	out := e.checkOut(w, ref, siteLat, siteLng)
	require.Equal(t, http.StatusOK, out.Status, "body: %s", out.Raw)
	d = data[detail](t, e.api(w.token, http.MethodGet, "/attendances/"+itoa(id), nil, ""))
	require.Nil(t, d.CheckInSelfie)
	require.NotNil(t, d.CheckOutSelfie)
	require.True(t, strings.HasPrefix(d.CheckOutSelfie.URL, "https://files.test/"), d.CheckOutSelfie.URL)
	require.Equal(t, "image/jpeg", d.CheckOutSelfie.ContentType)
	require.NotNil(t, d.CheckOutAt)
}

func TestDetailOfASelfieCheckInHasItsPhoto(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	in := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)

	d := data[detail](t, e.api(w.token, http.MethodGet, "/attendances/"+itoa(data[record](t, in).ID), nil, ""))
	require.NotNil(t, d.CheckInSelfie)
	require.True(t, strings.HasPrefix(d.CheckInSelfie.URL, "https://files.test/"))
	require.Empty(t, d.DanruNames, "a regu without a DANRU has none to name")
}

func TestDetailIsForTheOwnerAndTheScopeOfTheRegu(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danruToken := e.danru(w.org, "1K-IBMI-S1")
	_, memberToken := e.guard(w.org, w.shiftID)

	// A regu of another project (another client: a client has one project per business line).
	e.exec(w.org, "INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-OTHR', '1K', 'Lain', 'OTHR')")
	e.exec(w.org, `INSERT INTO projects (code, client_code, description, business_line, timezone, latitude, longitude)
VALUES ('1K-OTHR-S', '1K-OTHR', 'Proyek Lain', 'security', 'Asia/Jakarta', $1, $2)`, siteLat, siteLng)
	e.exec(w.org, "INSERT INTO teams (code, project_code, description) VALUES ('1K-OTHR-S1', '1K-OTHR-S', 'Regu Lain')")
	_, foreignDanru := e.danru(w.org, "1K-OTHR-S1")

	in := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	path := "/attendances/" + itoa(data[record](t, in).ID)

	require.Equal(t, http.StatusOK, e.api(w.token, http.MethodGet, path, nil, "").Status, "the owner")
	require.Equal(t, http.StatusOK, e.api(danruToken, http.MethodGet, path, nil, "").Status, "the DANRU of the regu")

	e.api(memberToken, http.MethodGet, path, nil, "").requireFailure(t, http.StatusNotFound, "NOT_FOUND", "ATTENDANCE_NOT_FOUND")
	e.api(foreignDanru, http.MethodGet, path, nil, "").requireFailure(t, http.StatusNotFound, "NOT_FOUND", "ATTENDANCE_NOT_FOUND")

	// The same answer for a record that does not exist, and for an id that is no id.
	e.api(w.token, http.MethodGet, "/attendances/999999", nil, "").requireFailure(t, http.StatusNotFound, "NOT_FOUND", "ATTENDANCE_NOT_FOUND")

	// Another organization's record is not found either.
	foreign := e.world()
	foreignIn := e.checkIn(foreign, e.onlyShift(foreign.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, foreignIn.Status, "body: %s", foreignIn.Raw)
	e.api(w.token, http.MethodGet, "/attendances/"+itoa(data[record](t, foreignIn).ID), nil, "").
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", "ATTENDANCE_NOT_FOUND")
}
