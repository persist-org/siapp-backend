package attendance_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/leave"
	"siapp/pkg/platform/testkit"
)

// The plan's remaining tests of 07.6 and 07.7: the whole swap flow through the leave module, two
// parallel swaps of one record, swapping a guard who is at work, and the admin monitor's filters.

// orgAdmin adds an ORG_ADMIN and returns their token.
func (e *env) orgAdmin(org int64) string {
	e.t.Helper()
	id, token := e.person(org)
	e.exec(org, "INSERT INTO web_role_assignments (personnel_id, role) VALUES ($1, 'ORG_ADMIN')", id)
	return token
}

// leaveDefaults gives the organization the leave module's starting categories (SICK is the one
// that needs no quota).
func (e *env) leaveDefaults(org int64) {
	e.t.Helper()
	_, err := leave.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), e.clock, org)
	require.NoError(e.t, err)
}

type permissionReply struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func TestTheWholeSwapFlowFromALeaveToTheReplacersCheckIn(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	e.leaveDefaults(w.org)
	danruID, danru := e.danru(w.org, "1K-IBMI-S1")
	require.NotZero(t, danruID)

	// The replacer works an early shift that ends exactly when the covered Day Shift starts:
	// back-to-back shifts are allowed.
	var earlyID int64
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes) VALUES ('Early', 120, 480, 15) RETURNING id").Scan(&earlyID))
	})
	replacerID, replacerToken := e.replacer(w.org)
	e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", replacerID, workDate, earlyID)

	// The guard asks for sick leave and the DANRU approves: a LEAVE record appears.
	sub := e.api(w.token, http.MethodPost, "/permissions",
		map[string]any{"type": "SICK", "category_code": "SICK", "start_date": workDate, "end_date": workDate, "reason": "Demam", "hospitalization": false, "letter_status": false}, e.newKey())
	require.Equal(t, http.StatusCreated, sub.Status, "body: %s", sub.Raw)
	request := data[permissionReply](t, sub)
	dec := e.api(danru, http.MethodPost, "/permissions/"+strconv.FormatInt(request.ID, 10)+"/approve", nil, e.newKey())
	require.Equal(t, http.StatusOK, dec.Status, "body: %s", dec.Raw)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE personnel_id = $1 AND status = 'LEAVE'", w.personID))

	// The DANRU finds the record replaceable and the replacer a candidate; the guard on leave is not.
	rep := e.api(danru, http.MethodGet, "/shift-swaps/replaceable?team_code=1K-IBMI-S1&work_date="+workDate, nil, "")
	require.Equal(t, http.StatusOK, rep.Status, "body: %s", rep.Raw)
	records := data[[]record](t, rep)
	require.Len(t, records, 1)
	require.Equal(t, "LEAVE", records[0].Status)
	cands := e.api(danru, http.MethodGet, "/shift-swaps/candidates?team_code=1K-IBMI-S1&work_date="+workDate, nil, "")
	require.Equal(t, http.StatusOK, cands.Status, "body: %s", cands.Raw)
	var ids []int64
	for _, c := range data[[]candidateJSONForTest](t, cands) {
		ids = append(ids, c.PersonnelID)
	}
	require.Contains(t, ids, replacerID)
	require.NotContains(t, ids, w.personID)

	sw := data[swapReply](t, e.swap(danru, records[0].ID, replacerID))

	// The replacer now holds two shifts today: their own Early one and the covered Day Shift.
	shifts := e.today(replacerToken)
	require.Len(t, shifts, 2)
	var covered today
	for _, s := range shifts {
		if s.ShiftName == "Day Shift" {
			covered = s
		}
	}
	require.NotEmpty(t, covered.ShiftRef, "the covered shift is offered")

	// They check in with the code the swap team's DANRU shows.
	code := e.qrCodeOf(danru, "1K-IBMI-S1", "IN")
	in := e.api(replacerToken, http.MethodPost, "/attendances/qr/check-in",
		map[string]any{"shift_ref": covered.ShiftRef, "token": code}, e.newKey())
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	require.Equal(t, "PRESENT", data[record](t, in).Status)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE personnel_id = $1 AND team_code = '1K-IBMI-S1' AND shift_swap_id = $2", replacerID, sw.ID))

	// Both tabs agree.
	type replacedRow struct {
		ShiftName          string  `json:"shift_name"`
		ReplacingName      string  `json:"replacing_personnel_name"`
		ReplacingCheckInAt *string `json:"replacing_check_in_at"`
	}
	type replacingRow struct {
		ShiftName    string  `json:"shift_name"`
		ReplacedName string  `json:"replaced_personnel_name"`
		CheckInAt    *string `json:"check_in_at"`
	}
	gone := e.api(w.token, http.MethodGet, "/attendances/replaced", nil, "")
	require.Equal(t, http.StatusOK, gone.Status, "body: %s", gone.Raw)
	replaced := data[[]replacedRow](t, gone)
	require.Len(t, replaced, 1)
	require.Equal(t, "Day Shift", replaced[0].ShiftName)
	require.NotNil(t, replaced[0].ReplacingCheckInAt)
	took := e.api(replacerToken, http.MethodGet, "/attendances/replacing", nil, "")
	require.Equal(t, http.StatusOK, took.Status, "body: %s", took.Raw)
	replacing := data[[]replacingRow](t, took)
	require.Len(t, replacing, 1)
	require.NotNil(t, replacing[0].CheckInAt)

	// What Personel is told carries the swap, so the shift is credited to the replacer.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM personel_outbox WHERE payload->>'type' = 'CHECK_IN' AND (payload->>'shift_swap_id')::bigint = $1", sw.ID))

	// Too late to cancel.
	del := e.api(danru, http.MethodDelete, "/shift-swaps/"+strconv.FormatInt(sw.ID, 10), nil, "")
	del.requireFailure(t, http.StatusConflict, "CONFLICT", "SWAP_ALREADY_STARTED")
}

type candidateJSONForTest struct {
	PersonnelID int64 `json:"personnel_id"`
}

func TestAGuardAtWorkCannotBeReplaced(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)

	in := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	present := data[record](t, in)

	e.swap(danru, present.ID, replacerID).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "SWAP_REPLACED_NOT_ABSENT")
}

func TestTwoParallelSwapsOfOneRecordLetExactlyOneThrough(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	firstID, _ := e.replacer(w.org)
	secondID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)

	var wg sync.WaitGroup
	results := make([]reply, 2)
	for i, replacer := range []int64{firstID, secondID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.raw(danru, http.MethodPost, "/shift-swaps",
				map[string]any{"replaced_attendance_id": absent.ID, "replacing_personnel_id": replacer}, e.newKey())
		}()
	}
	wg.Wait()

	created, refused := 0, 0
	for _, r := range results {
		switch r.Status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			r.requireFailure(t, http.StatusConflict, "DUPLICATE", "SWAP_ALREADY_EXISTS")
			refused++
		default:
			t.Fatalf("unexpected answer %d: %s", r.Status, r.Raw)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, refused)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE cancelled_at IS NULL"))
}

func TestTheAdminMonitorFiltersByStatusAndSyncStatus(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	admin := e.orgAdmin(w.org)
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	secondID, _ := e.guard(w.org, w.shiftID)
	e.markAbsent(danru, w.personID)
	e.markAbsent(danru, secondID)
	e.exec(w.org, "UPDATE attendances SET personel_sync_status = 'SYNCED' WHERE personnel_id = $1", secondID)
	// A third record of another status: a check-in by a new guard.
	presentID, presentToken := e.guard(w.org, w.shiftID)
	pw := w
	pw.personID, pw.token = presentID, presentToken
	require.Equal(t, http.StatusCreated, e.checkIn(pw, e.onlyShift(presentToken).ShiftRef, siteLat, siteLng).Status)

	list := func(query string) []record {
		r := e.api(admin, http.MethodGet, "/admin/attendances?"+query, nil, "")
		require.Equal(t, http.StatusOK, r.Status, "%s: %s", query, r.Raw)
		return data[[]record](t, r)
	}
	require.Len(t, list(""), 3)
	require.Len(t, list("status=ABSENT"), 2)
	require.Len(t, list("status=PRESENT"), 1)
	require.Len(t, list("sync_status=SYNCED"), 1)
	require.Len(t, list("status=ABSENT&sync_status=PENDING"), 1)
	require.Empty(t, list("status=LEAVE"))
	require.Empty(t, list("sync_status=FAILED"))

	for _, bad := range []string{"status=MISSING", "sync_status=DONE"} {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/attendances?"+bad, nil, "").Status, bad)
	}
}

func TestAnAdminCorrectionToPresentCancelsAnUnstartedSwap(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	admin := e.orgAdmin(w.org)
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	r := e.api(admin, http.MethodPatch, "/admin/attendances/"+strconv.FormatInt(absent.ID, 10),
		map[string]any{"status": "PRESENT", "check_in_at": "2026-03-10T01:05:00Z", "reason": "Ternyata hadir"}, "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1 AND cancelled_at IS NOT NULL", sw.ID))
}
