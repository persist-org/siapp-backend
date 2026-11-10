package attendance_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// replacer adds a member of the regu with no schedule that day, so nothing of theirs overlaps.
func (e *env) replacer(org int64) (int64, string) {
	e.t.Helper()
	id, token := e.person(org)
	e.exec(org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", id)
	return id, token
}

type swapReply struct {
	ID                   int64   `json:"id"`
	ReplacedAttendanceID *int64  `json:"replaced_attendance_id"`
	CancelledAt          *string `json:"cancelled_at"`
}

// markAbsent has the DANRU mark the guard absent on workDate and returns the record.
func (e *env) markAbsent(danruToken string, personnelID int64) record {
	e.t.Helper()
	r := e.api(danruToken, http.MethodPost, "/attendances/absent",
		map[string]any{"personnel_id": personnelID, "team_code": "1K-IBMI-S1", "date": workDate, "reason": "tidak hadir"}, e.newKey())
	require.Equal(e.t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	return data[record](e.t, r)
}

func (e *env) swap(danruToken string, attendanceID, replacerID int64) reply {
	e.t.Helper()
	return e.api(danruToken, http.MethodPost, "/shift-swaps",
		map[string]any{"replaced_attendance_id": attendanceID, "replacing_personnel_id": replacerID}, e.newKey())
}

func TestCancellingAMangkirCancelsItsUnstartedSwap(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)

	absent := e.markAbsent(danru, w.personID)
	r := e.swap(danru, absent.ID, replacerID)
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	sw := data[swapReply](t, r)

	del := e.api(danru, http.MethodDelete, "/attendances/absent/"+strconv.FormatInt(absent.ID, 10), nil, "")
	require.Equal(t, http.StatusOK, del.Status, "body: %s", del.Raw)

	// The swap survives as cancelled history, no longer pointing at the deleted record.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1 AND cancelled_at IS NOT NULL AND replaced_attendance_id IS NULL", sw.ID))
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1", absent.ID))
}

func TestACoveredMangkirCannotBeCancelledOnceTheReplacerStarted(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, replacerToken := e.replacer(w.org)

	absent := e.markAbsent(danru, w.personID)
	require.Equal(t, http.StatusCreated, e.swap(danru, absent.ID, replacerID).Status)

	ref := e.onlyShift(replacerToken).ShiftRef
	in := e.api(replacerToken, http.MethodPost, "/attendances/selfie/check-in",
		body(ref, e.selfie(w.org, replacerID), siteLat, siteLng), e.newKey())
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)

	del := e.api(danru, http.MethodDelete, "/attendances/absent/"+strconv.FormatInt(absent.ID, 10), nil, "")
	del.requireFailure(t, http.StatusConflict, "CONFLICT", "SWAP_ALREADY_STARTED")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1", absent.ID), "the record stays")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE cancelled_at IS NULL"), "the swap stays")
}

// Deleting the replaced record of any swap, whatever its state, used to fail on the foreign key
// (a 500): revoking a leave deletes its LEAVE rows exactly this way.
func TestDeletingTheRecordOfASwapKeepsTheSwap(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)

	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	e.exec(w.org, "DELETE FROM attendances WHERE id = $1", absent.ID)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1 AND replaced_attendance_id IS NULL", sw.ID))
}

func TestOneReplacerCannotTakeTwoOverlappingShifts(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	secondID, _ := e.guard(w.org, w.shiftID)

	first := e.markAbsent(danru, w.personID)
	second := e.markAbsent(danru, secondID)

	require.Equal(t, http.StatusCreated, e.swap(danru, first.ID, replacerID).Status)
	// The same Day Shift, taken over a second time by the same person: the first swap is a shift of theirs.
	e.swap(danru, second.ID, replacerID).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "SWAP_SHIFT_OVERLAP")
}

func TestAGroupCheckInOutsideTheShiftWindowIsRefusedPerEntry(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	e.exec(w.org, "UPDATE projects SET allow_group_attendance = true WHERE code = '1K-IBMI-S'")
	// A shift a week ahead: scheduled, but nowhere near being checked into.
	e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, '2026-03-17', $2)", w.personID, w.shiftID)

	r := e.api(danru, http.MethodPost, "/attendances/group", map[string]any{
		"team_code": "1K-IBMI-S1", "shift_ref": base64Ref("1K-IBMI-S1", "2026-03-17", w.shiftID),
		"personnel_ids": []int64{w.personID}, "latitude": siteLat, "longitude": siteLng,
	}, e.newKey())
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	type entry struct {
		Attendance *record `json:"attendance"`
		Error      *struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	entries := data[[]entry](t, r)
	require.Len(t, entries, 1)
	require.Nil(t, entries[0].Attendance)
	require.NotNil(t, entries[0].Error)
	require.Equal(t, "ATTENDANCE_SHIFT_NOT_ACTIVE", entries[0].Error.Reason)
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances WHERE work_date = '2026-03-17'"))
}

// ---------- the rest of 07.6 / 07.7 ----------

func TestAMangkirNeedsAReason(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	for _, reason := range []any{nil, "", "   "} {
		body := map[string]any{"personnel_id": w.personID, "team_code": "1K-IBMI-S1", "date": workDate}
		if reason != nil {
			body["reason"] = reason
		}
		r := e.raw(danru, http.MethodPost, "/attendances/absent", body, e.newKey())
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "reason %v: %s", reason, r.Raw)
	}
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
}

type teamRow struct {
	PersonnelID int64  `json:"personnel_id"`
	Status      string `json:"status"`
}

func (e *env) teamRows(token, query string) []teamRow {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/attendances?"+query, nil, "")
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	return data[[]teamRow](e.t, r)
}

func TestAMemberSeesOnlyTheirOwnMangkirHistory(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	otherID, _ := e.guard(w.org, w.shiftID)
	e.markAbsent(danru, w.personID)
	e.markAbsent(danru, otherID)

	own := e.teamRows(w.token, "status=ABSENT&month=2026-03")
	require.Len(t, own, 1)
	require.Equal(t, w.personID, own[0].PersonnelID)
	// Their own regu is no more readable for them than any other.
	require.Len(t, e.teamRows(w.token, "team_code=1K-IBMI-S1"), 1)
	// Another month has none; the DANRU sees both of the regu.
	require.Empty(t, e.teamRows(w.token, "status=ABSENT&month=2026-04"))
	require.Len(t, e.teamRows(danru, "status=ABSENT&month=2026-03"), 2)

	for _, bad := range []string{"status=MISSING", "month=2026-3", "month=2026-03&date_from=2026-03-01"} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(w.token, http.MethodGet, "/attendances?"+bad, nil, "").Status, bad)
	}
}

func TestAGroupRowOfACoveredRecordIsSwapped(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	require.Equal(t, http.StatusCreated, e.swap(danru, absent.ID, replacerID).Status)

	rows := e.teamRows(danru, "team_code=1K-IBMI-S1&month=2026-03")
	require.Len(t, rows, 1)
	require.Equal(t, "SWAPPED", rows[0].Status)
	// Filtering by the stored status shows the stored one.
	require.Equal(t, "ABSENT", e.teamRows(danru, "status=ABSENT&month=2026-03")[0].Status)
}

func TestTheCalendarIncludesACoveredShiftBeforeTheReplacerChecksIn(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, replacerToken := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	r := e.api(replacerToken, http.MethodGet, "/attendances/me?month=2026-03", nil, "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	type day struct {
		WorkDate string `json:"work_date"`
		Shifts   []struct {
			Status      string `json:"status"`
			ShiftSwapID *int64 `json:"shift_swap_id"`
		} `json:"shifts"`
	}
	days := data[[]day](t, r)
	require.Len(t, days, 1)
	require.Equal(t, workDate, days[0].WorkDate)
	require.Len(t, days[0].Shifts, 1)
	require.Equal(t, "UPCOMING", days[0].Shifts[0].Status)
	require.NotNil(t, days[0].Shifts[0].ShiftSwapID)
	require.Equal(t, sw.ID, *days[0].Shifts[0].ShiftSwapID)
}

func TestAReplacerOfACoveredShiftCanBeMarkedAbsent(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	// The replacer has no schedule of their own that day: only the swap makes this shift theirs.
	again := e.markAbsent(danru, replacerID)
	require.Equal(t, "ABSENT", again.Status)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1 AND shift_swap_id = $2", again.ID, sw.ID))
}

func TestAGroupCheckInOfACoveredShiftCarriesTheSwap(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	e.exec(w.org, "UPDATE projects SET allow_group_attendance = true WHERE code = '1K-IBMI-S'")
	replacerID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	r := e.api(danru, http.MethodPost, "/attendances/group", map[string]any{
		"team_code": "1K-IBMI-S1", "shift_ref": base64Ref("1K-IBMI-S1", workDate, w.shiftID),
		"personnel_ids": []int64{replacerID}, "latitude": siteLat, "longitude": siteLng,
	}, e.newKey())
	require.Equal(t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE personnel_id = $1 AND status = 'PRESENT' AND check_in_method = 'GROUP' AND shift_swap_id = $2", replacerID, sw.ID))
	// The credit goes to the replacer: both tabs now show a check-in.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1", sw.ID))
}

func TestAMangkirNotifiesTheGuardAfterItIsOnRecord(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	danruID, danru := e.danru(w.org, "1K-IBMI-S1")
	require.NotZero(t, danruID)

	// A refused mark sends nothing.
	r := e.raw(danru, http.MethodPost, "/attendances/absent", map[string]any{"personnel_id": w.personID, "team_code": "1K-IBMI-S1", "date": workDate}, e.newKey())
	require.Equal(t, http.StatusUnprocessableEntity, r.Status)
	require.Empty(t, e.notices.all())

	absent := e.markAbsent(danru, w.personID)
	got := e.notices.all()
	require.Len(t, got, 1)
	require.Equal(t, "ABSENCE_MARKED", string(got[0].Kind))
	require.Equal(t, []int64{w.personID}, got[0].RecipientIDs)
	require.Equal(t, "Petugas Absen", got[0].ActorName, "names the DANRU")
	require.Equal(t, workDate, got[0].Date.String())
	require.Equal(t, absent.ID, got[0].EntityID)
}

func TestASwapNotifiesBothPeopleEachNamingTheOther(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	e.exec(w.org, "UPDATE personnels SET full_name = 'Budi Pengganti' WHERE id = $1", replacerID)
	e.exec(w.org, "UPDATE personnels SET full_name = 'Andi Absen' WHERE id = $1", w.personID)
	absent := e.markAbsent(danru, w.personID)

	// Overlap refusal: nothing more is sent.
	before := len(e.notices.all())
	require.Equal(t, http.StatusUnprocessableEntity, e.swap(danru, absent.ID, w.personID).Status)
	require.Len(t, e.notices.all(), before)

	require.Equal(t, http.StatusCreated, e.swap(danru, absent.ID, replacerID).Status)
	got := e.notices.all()[before:]
	require.Len(t, got, 2)
	byRecipient := map[int64]string{}
	for _, n := range got {
		require.Equal(t, "SWAP_ASSIGNED", string(n.Kind))
		require.Len(t, n.RecipientIDs, 1)
		byRecipient[n.RecipientIDs[0]] = n.ActorName
	}
	require.Equal(t, map[int64]string{w.personID: "Budi Pengganti", replacerID: "Andi Absen"}, byRecipient)
}
