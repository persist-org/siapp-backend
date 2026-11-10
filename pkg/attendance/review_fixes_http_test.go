package attendance_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Tests for the PR review of 07.6/07.7: the mangkir time limit, a swap whose replacer was marked
// absent, parallel swaps of one replacer, a leave revoke giving the mangkir back, and the admin list.

func TestAMangkirCannotBeMarkedBeforeTheShiftStarts(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")

	// The Day Shift starts at 01:00Z.
	e.clock.Set(time.Date(2026, 3, 10, 0, 30, 0, 0, time.UTC))
	r := e.raw(danru, http.MethodPost, "/attendances/absent",
		map[string]any{"personnel_id": w.personID, "team_code": "1K-IBMI-S1", "date": workDate, "reason": "tidak hadir"}, e.newKey())
	r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "ABSENCE_SHIFT_NOT_STARTED")
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM attendances"))
	require.Empty(t, e.notices.all(), "no notice for a mark that was refused")

	// Once it has started the same mark goes through.
	e.clock.Set(tuesday)
	require.Equal(t, "ABSENT", e.markAbsent(danru, w.personID).Status)
}

func TestASwapWhoseReplacerWasMarkedAbsentCannotBeCancelled(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)
	absent := e.markAbsent(danru, w.personID)
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))
	replacerAbsent := e.markAbsent(danru, replacerID)

	// The replacer's mangkir is a record of the swap: cancelling it would leave that record pointing at
	// a cancelled swap, and the replaced guard's mangkir would be undone by the replacer's.
	e.api(danru, http.MethodDelete, "/shift-swaps/"+strconv.FormatInt(sw.ID, 10), nil, "").
		requireFailure(t, http.StatusConflict, "CONFLICT", "SWAP_ALREADY_STARTED")
	e.api(danru, http.MethodDelete, "/attendances/absent/"+strconv.FormatInt(absent.ID, 10), nil, "").
		requireFailure(t, http.StatusConflict, "CONFLICT", "SWAP_ALREADY_STARTED")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1 AND cancelled_at IS NULL", sw.ID))

	// Undoing the replacer's mangkir first makes the swap cancellable again.
	del := e.api(danru, http.MethodDelete, "/attendances/absent/"+strconv.FormatInt(replacerAbsent.ID, 10), nil, "")
	require.Equal(t, http.StatusOK, del.Status, "body: %s", del.Raw)
	del = e.api(danru, http.MethodDelete, "/shift-swaps/"+strconv.FormatInt(sw.ID, 10), nil, "")
	require.Equal(t, http.StatusOK, del.Status, "body: %s", del.Raw)
}

func TestTwoParallelSwapsOfOneReplacerOverlappingLetExactlyOneThrough(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	secondID, _ := e.guard(w.org, w.shiftID)
	replacerID, _ := e.replacer(w.org)
	first := e.markAbsent(danru, w.personID)
	second := e.markAbsent(danru, secondID)

	// Two different records, the same shift (so overlapping times), the same replacer.
	var wg sync.WaitGroup
	results := make([]reply, 2)
	for i, id := range []int64{first.ID, second.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.raw(danru, http.MethodPost, "/shift-swaps",
				map[string]any{"replaced_attendance_id": id, "replacing_personnel_id": replacerID}, e.newKey())
		}()
	}
	wg.Wait()

	created, refused := 0, 0
	for _, r := range results {
		switch r.Status {
		case http.StatusCreated:
			created++
		case http.StatusUnprocessableEntity:
			r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "SWAP_SHIFT_OVERLAP")
			refused++
		default:
			t.Fatalf("unexpected answer %d: %s", r.Status, r.Raw)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, refused)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE replacing_personnel_id = $1 AND cancelled_at IS NULL", replacerID))
}

func TestRevokingALeaveGivesBackTheMangkirItReplaced(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	e.leaveDefaults(w.org)
	admin := e.orgAdmin(w.org)
	danruID, danru := e.danru(w.org, "1K-IBMI-S1")
	replacerID, _ := e.replacer(w.org)

	// A DANRU marks a mangkir, the guard's late sick leave is approved over it, and a swap is assigned.
	absent := e.markAbsent(danru, w.personID)
	sub := e.api(w.token, http.MethodPost, "/permissions",
		map[string]any{"type": "SICK", "category_code": "SICK", "start_date": workDate, "end_date": workDate, "reason": "Demam", "hospitalization": false, "letter_status": false}, e.newKey())
	require.Equal(t, http.StatusCreated, sub.Status, "body: %s", sub.Raw)
	request := data[permissionReply](t, sub)
	id := strconv.FormatInt(request.ID, 10)
	dec := e.api(danru, http.MethodPost, "/permissions/"+id+"/approve", nil, e.newKey())
	require.Equal(t, http.StatusOK, dec.Status, "body: %s", dec.Raw)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1 AND status = 'LEAVE'", absent.ID), "the mangkir became leave")
	sw := data[swapReply](t, e.swap(danru, absent.ID, replacerID))

	// The admin declines the approved request: the leave goes, the mangkir comes back as it was.
	rev := e.api(admin, http.MethodPost, "/admin/permissions/"+id+"/decline", map[string]any{"note": "Surat tidak sah"}, e.newKey())
	require.Equal(t, http.StatusOK, rev.Status, "body: %s", rev.Raw)
	require.Equal(t, 1, e.count(w.org,
		"SELECT count(*) FROM attendances WHERE id = $1 AND status = 'ABSENT' AND marked_absent_by_id = $2 AND absent_reason = 'tidak hadir' AND leave_request_id IS NULL",
		absent.ID, danruID))
	// It is still a mangkir, so its replacer is still needed.
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM shift_swaps WHERE id = $1 AND cancelled_at IS NULL AND replaced_attendance_id = $2", sw.ID, absent.ID))
}

func TestADanruCannotDeleteAMangkirAnAdminSet(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	admin := e.orgAdmin(w.org)
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	in := e.checkIn(w, e.onlyShift(w.token).ShiftRef, siteLat, siteLng)
	require.Equal(t, http.StatusCreated, in.Status, "body: %s", in.Raw)
	present := data[record](t, in)

	r := e.api(admin, http.MethodPatch, "/admin/attendances/"+strconv.FormatInt(present.ID, 10),
		map[string]any{"status": "ABSENT", "reason": "Ternyata tidak hadir"}, "")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)

	e.api(danru, http.MethodDelete, "/attendances/absent/"+strconv.FormatInt(present.ID, 10), nil, "").
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", "ATTENDANCE_NOT_FOUND")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1", present.ID))
}

func TestACorrectionCannotCheckOutBeforeCheckIn(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	admin := e.orgAdmin(w.org)
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	absent := e.markAbsent(danru, w.personID)

	r := e.api(admin, http.MethodPatch, "/admin/attendances/"+strconv.FormatInt(absent.ID, 10),
		map[string]any{"status": "PRESENT", "check_in_at": "2026-03-10T05:00:00Z", "check_out_at": "2026-03-10T04:00:00Z", "reason": "Koreksi"}, "")
	require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE id = $1 AND status = 'ABSENT'", absent.ID))
}

func TestTheAdminListTotalFollowsItsFilters(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	admin := e.orgAdmin(w.org)
	_, danru := e.danru(w.org, "1K-IBMI-S1")
	secondID, _ := e.guard(w.org, w.shiftID)
	e.markAbsent(danru, w.personID)
	e.markAbsent(danru, secondID)
	e.exec(w.org, "UPDATE attendances SET personel_sync_status = 'SYNCED' WHERE personnel_id = $1", secondID)
	presentID, presentToken := e.guard(w.org, w.shiftID)
	pw := w
	pw.personID, pw.token = presentID, presentToken
	require.Equal(t, http.StatusCreated, e.checkIn(pw, e.onlyShift(presentToken).ShiftRef, siteLat, siteLng).Status)

	total := func(query string) int {
		r := e.api(admin, http.MethodGet, "/admin/attendances?"+query, nil, "")
		require.Equal(t, http.StatusOK, r.Status, "%s: %s", query, r.Raw)
		var env struct {
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		require.NoError(t, json.Unmarshal(r.Raw, &env))
		return env.Pagination.Total
	}
	require.Equal(t, 3, total(""))
	require.Equal(t, 2, total("status=ABSENT"))
	require.Equal(t, 1, total("status=PRESENT"))
	require.Equal(t, 1, total("sync_status=SYNCED"))
	require.Equal(t, 1, total("status=ABSENT&sync_status=PENDING&limit=1"))
}
