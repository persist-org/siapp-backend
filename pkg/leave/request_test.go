package leave_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/testkit"
)

// The clock of these tests is Tuesday 2026-03-10, 08:30 in Jakarta, so a request number of the
// month starts with 202603.

type permission struct {
	ID           int64   `json:"id"`
	Number       string  `json:"number"`
	TeamCode     string  `json:"team_code"`
	Type         string  `json:"type"`
	CategoryCode string  `json:"category_code"`
	CategoryName string  `json:"category_name"`
	Status       string  `json:"status"`
	StartDate    string  `json:"start_date"`
	EndDate      string  `json:"end_date"`
	StartTime    *string `json:"start_time"`
	EndTime      *string `json:"end_time"`
	WorkingDays  int     `json:"working_days"`
	Reason       string  `json:"reason"`
	CanCancel    bool    `json:"can_cancel"`
	Decision     *struct {
		Level  string  `json:"level"`
		ByName string  `json:"by_name"`
		Note   *string `json:"note"`
	} `json:"decision"`
	Sick *struct {
		Hospitalization bool    `json:"hospitalization"`
		LetterStatus    bool    `json:"letter_status"`
		Disease         *string `json:"disease"`
	} `json:"sick"`
	Attachment *struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	} `json:"attachment"`

	// For someone who decides it.
	Person *struct {
		PersonnelID int64  `json:"personnel_id"`
		Name        string `json:"name"`
	} `json:"person"`
	OverlapsWith []struct {
		Name      string `json:"name"`
		StartDate string `json:"start_date"`
		EndDate   string `json:"end_date"`
	} `json:"overlaps_with"`
	Coverage     *coverage `json:"coverage"`
	LeaveBalance *balance  `json:"leave_balance"`
	Skipped      []skipped `json:"skipped"`
}

type skipped struct {
	WorkDate string `json:"work_date"`
	TeamCode string `json:"team_code"`
}

type coverage struct {
	Date    string `json:"date"`
	Present int    `json:"present"`
	Size    int    `json:"size"`
}

type balance struct {
	RemainingDays     int `json:"remaining_days"`
	AfterApprovalDays int `json:"after_approval_days"`
}

func (e *env) key() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.keys++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", e.keys)
}

// submit is POST /permissions, checked against the contract.
func (e *env) submit(token string, body map[string]any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.DoWithHeaders(http.MethodPost, "/permissions", body, map[string]string{"Idempotency-Key": e.key()})
	return reply{Status: res.Status, Raw: res.Body}
}

// keyed is a request outside the contract's checks, with an Idempotency-Key: for bodies the
// contract itself would refuse, and for several goroutines at once.
func (e *env) keyed(token, method, path string, body any, key string) reply {
	e.t.Helper()
	return e.rawHeaders(token, method, path, body, map[string]string{"Idempotency-Key": key})
}

func (e *env) mustSubmit(token string, body map[string]any) permission {
	e.t.Helper()
	r := e.submit(token, body)
	require.Equal(e.t, http.StatusCreated, r.Status, "body: %s", r.Raw)
	return data[permission](e.t, r)
}

// cancel is POST /permissions/{id}/cancel, checked against the contract.
func (e *env) cancel(token string, id int64) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	res := e.client.DoWithHeaders(http.MethodPost, "/permissions/"+strconv.FormatInt(id, 10)+"/cancel", nil,
		map[string]string{"Idempotency-Key": e.key()})
	return reply{Status: res.Status, Raw: res.Body}
}

func (e *env) detail(token string, id int64) reply {
	e.t.Helper()
	return e.api(token, http.MethodGet, "/permissions/"+strconv.FormatInt(id, 10), nil)
}

func (e *env) mine(token, query string) (list []permission, total int) {
	e.t.Helper()
	r := e.api(token, http.MethodGet, "/permissions/me"+query, nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var env struct {
		Data       []permission `json:"data"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	require.NoError(e.t, json.Unmarshal(r.Raw, &env))
	return env.Data, env.Pagination.Total
}

// leaveBody is the body of a request of a category over [from, to].
func leaveBody(typ, category, from, to string) map[string]any {
	return map[string]any{"type": typ, "category_code": category, "start_date": from, "end_date": to, "reason": "Keperluan keluarga"}
}

// fieldsOf are the fields a VALIDATION_ERROR names.
func fieldsOf(t *testing.T, r reply) []string {
	t.Helper()
	var env struct {
		Error struct {
			Errors []struct {
				Field string `json:"field"`
			} `json:"errors"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env), "body: %s", r.Raw)
	var out []string
	for _, f := range env.Error.Errors {
		out = append(out, f.Field)
	}
	return out
}

// balance is the person's annual-leave balance, read from the ledger.
func (e *env) balance(w world, personnelID int64) int {
	e.t.Helper()
	r := e.api(w.admin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(personnelID, 10)+"/leave-ledger", nil)
	require.Equal(e.t, http.StatusOK, r.Status, "body: %s", r.Raw)
	rows := data[[]ledgerEntry](e.t, r)
	if len(rows) == 0 {
		return 0
	}
	return rows[0].BalanceAfter
}

// requestWorld is a world where UNPAID has 10 days for the client and the guard has 5 days of annual
// leave.
func (e *env) requestWorld() world {
	e.t.Helper()
	w := e.world()
	require.Equal(e.t, http.StatusOK, e.setQuotas(w, "UNPAID", map[string]any{"default_days": 10}).Status)
	require.Equal(e.t, http.StatusCreated, e.grant(w, w.guardID, 5).Status)
	return w
}

func (e *env) setStatus(org, id int64, status string) {
	e.t.Helper()
	e.exec(org, `UPDATE leave_requests SET status = $2::text::leave_request_status,
  decided_level = CASE WHEN $2::text IN ('APPROVED', 'REJECTED') THEN 'DANRU'::leave_decided_level END,
  decided_at = CASE WHEN $2::text IN ('APPROVED', 'REJECTED') THEN now() END,
  cancelled_at = CASE WHEN $2::text = 'CANCELLED' THEN now() END
WHERE id = $1`, id, status)
}

// ---------- submit ----------

func TestAnnualLeaveTakesItsDaysAtSubmitAndNumbersTheRequest(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()

	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))
	require.Equal(t, "LR20260300001", got.Number)
	require.Equal(t, "PENDING", got.Status)
	require.Equal(t, 3, got.WorkingDays, "no schedule at all: every calendar day counts")
	require.Equal(t, "1K-IBMI-S1", got.TeamCode, "the only regu of the person")
	require.Equal(t, "Cuti Tahunan", got.CategoryName)
	require.True(t, got.CanCancel)
	require.Nil(t, got.Decision)
	require.Nil(t, got.Sick)

	require.Equal(t, 2, e.balance(w, w.guardID), "the days left the balance at submit")
	q := data[allowance](t, e.quota(w.guard, "ANNUAL", ""))
	require.Equal(t, allowance{CategoryCode: "ANNUAL", QuotaDays: 2, UsedDays: 3, RemainingDays: 2}, q)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'DEDUCT' AND leave_request_id = $1", got.ID))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_requests WHERE approver_personnel_id = $1", w.danruID), "the DANRU is who will decide")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM audit_logs WHERE action = 'leave.request.submit'"))

	// The same type counts on, and a sick request has a counter of its own.
	second := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-23", "2026-03-24"))
	require.Equal(t, "LR20260300002", second.Number)
	sick := leaveBody("SICK", "", "2026-04-06", "2026-04-07")
	sick["hospitalization"], sick["letter_status"] = false, false
	require.Equal(t, "SR20260300001", e.mustSubmit(w.guard, sick).Number)
	require.Equal(t, 0, e.balance(w, w.guardID))

	// Nothing is left: a day more is refused, and writes nothing.
	r := e.submit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-30", "2026-03-30"))
	r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")
	require.Equal(t, 3, e.count(w.org, "SELECT count(*) FROM leave_requests"))
}

func TestAnAnnualRequestLargerThanTheBalanceIsRefused(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()

	r := e.submit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-24")) // 7 days, balance 5
	r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")
	require.Equal(t, []string{"category_code"}, fieldsOf(t, r))
	require.Equal(t, 5, e.balance(w, w.guardID))
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_requests"))
}

func TestSubmittingTwiceWithOneKeyMakesOneRequestAndOneDeduction(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	key := e.key()
	body := leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-19")

	first := e.keyed(w.guard, http.MethodPost, "/permissions", body, key)
	again := e.keyed(w.guard, http.MethodPost, "/permissions", body, key)
	require.Equal(t, http.StatusCreated, first.Status, "body: %s", first.Raw)
	require.Equal(t, http.StatusCreated, again.Status, "body: %s", again.Raw)
	require.Equal(t, data[permission](t, first).ID, data[permission](t, again).ID)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_requests"))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'DEDUCT'"))
	require.Equal(t, 3, e.balance(w, w.guardID))
}

func TestTheQuotaOfOtherCategoriesCountsTheDaysOfPendingRequests(t *testing.T) {
	e := newEnv(t)
	w := e.world()
	require.Equal(t, http.StatusOK, e.setQuotas(w, "UNPAID", map[string]any{"default_days": 4}).Status)
	require.Equal(t, http.StatusOK, e.setQuotas(w, "MATERNITY", map[string]any{"default_days": 1}).Status)

	first := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-20")) // 3 days
	require.Equal(t, 3, first.WorkingDays)
	require.Equal(t, allowance{CategoryCode: "UNPAID", QuotaDays: 4, UsedDays: 3, RemainingDays: 1}, data[allowance](t, e.quota(w.guard, "UNPAID", "")))
	require.Zero(t, e.balance(w, w.guardID), "no ledger for a category that is not annual")

	// 2 days against 1 left fails, 1 day passes.
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-23", "2026-03-24")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")
	e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-23", "2026-03-23"))
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-25", "2026-03-25")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")

	// Canceling gives the days back: a canceled request stops counting.
	require.Equal(t, http.StatusOK, e.cancel(w.guard, first.ID).Status)
	require.Equal(t, 3, data[allowance](t, e.quota(w.guard, "UNPAID", "")).RemainingDays)

	// Maternity leave skips the comparison of days (but one must be left).
	long := e.mustSubmit(w.guard, leaveBody("PERMIT", "MATERNITY", "2026-04-01", "2026-06-30"))
	require.Equal(t, 91, long.WorkingDays)
	e.submit(w.guard, leaveBody("PERMIT", "MATERNITY", "2026-07-01", "2026-07-02")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")

	// A client switched off has no quota; sick leave has none to run out of.
	require.Equal(t, http.StatusOK, e.api(w.admin, http.MethodPut, "/admin/leave-client-settings/1K-IBMI", map[string]any{"requests_enabled": false}).Status)
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-08-03", "2026-08-03")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_QUOTA_EXCEEDED")
	sick := leaveBody("SICK", "", "2026-08-03", "2026-08-05")
	sick["hospitalization"], sick["letter_status"] = true, false
	got := e.mustSubmit(w.guard, sick)
	require.Zero(t, got.WorkingDays, "a category without a quota counts no days")
	require.Equal(t, "SICK", got.CategoryCode, "a sick request without a category uses SICK")
	require.True(t, got.Sick.Hospitalization)
}

// ---------- overlap ----------

func TestARequestCannotShareADayWithAnotherThatIsStillLive(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()

	first := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-20"))
	for _, body := range []map[string]any{
		leaveBody("LEAVE", "UNPAID", "2026-03-20", "2026-03-22"),
		leaveBody("LEAVE", "ANNUAL", "2026-03-17", "2026-03-18"),
		func() map[string]any {
			b := leaveBody("SICK", "", "2026-03-19", "2026-03-19")
			b["hospitalization"], b["letter_status"] = false, false
			return b
		}(),
	} {
		r := e.submit(w.guard, body)
		r.requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_OVERLAP")
		require.Equal(t, []string{"start_date"}, fieldsOf(t, r))
	}
	require.Equal(t, 5, e.balance(w, w.guardID), "a refused request deducted nothing")

	// Canceled and rejected requests do not block.
	require.Equal(t, http.StatusOK, e.cancel(w.guard, first.ID).Status)
	again := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-20"))
	e.setStatus(w.org, again.ID, "REJECTED")
	e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-19", "2026-03-19"))

	// Someone else's request on the same days is no concern.
	otherID, other := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", otherID)
	e.mustSubmit(other, leaveBody("LEAVE", "UNPAID", "2026-03-19", "2026-03-19"))
}

func TestTwoPartOfDayRequestsShareADayUnlessTheirWindowsClash(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	require.Equal(t, http.StatusOK, e.setQuotas(w, "OTHER_PART_DAY", map[string]any{"default_days": 20}).Status)
	timed := func(day, from, to string) map[string]any {
		b := leaveBody("PERMIT", "OTHER_PART_DAY", day, day)
		b["start_time"], b["end_time"] = from, to
		return b
	}

	got := e.mustSubmit(w.guard, timed("2026-03-18", "08:00", "10:00"))
	require.Equal(t, "08:00", *got.StartTime)
	require.Equal(t, "10:00", *got.EndTime)
	e.submit(w.guard, timed("2026-03-18", "09:00", "11:00")).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_OVERLAP")
	e.mustSubmit(w.guard, timed("2026-03-18", "10:00", "12:00")) // touching is not clashing
	// Two are already there: nothing else fits, even a window clear of both.
	e.submit(w.guard, timed("2026-03-18", "13:00", "14:00")).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_OVERLAP")

	// A normal request over a timed one, and the other way round, conflict.
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-19")).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_OVERLAP")
	e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-24", "2026-03-24"))
	e.submit(w.guard, timed("2026-03-24", "08:00", "09:00")).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_OVERLAP")
}

// ---------- working days ----------

func TestWorkingDaysAreTheScheduledDaysThatAreNotDaysOff(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	var day, off int64
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute) VALUES ('Day', 480, 1020) RETURNING id").Scan(&day))
		require.NoError(t, tx.QueryRow(t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, is_off) VALUES ('Libur', 0, 0, true) RETURNING id").Scan(&off))
	})
	for date, shift := range map[string]int64{"2026-03-18": day, "2026-03-19": day, "2026-03-20": off, "2026-03-21": day, "2026-03-24": off} {
		e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", w.guardID, date, shift)
	}

	// A gap is named, and refused.
	r := e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-22"))
	r.requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_SCHEDULE_MISSING")
	require.Contains(t, string(r.Raw), "2026-03-22")
	// A range of days off has no working day.
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-24", "2026-03-24")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_NO_WORKING_DAYS")
	// The day off is not counted.
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-21"))
	require.Equal(t, 3, got.WorkingDays)
	// No schedule row in the range at all: every calendar day.
	require.Equal(t, 3, e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-04-01", "2026-04-03")).WorkingDays)
}

// ---------- validation ----------

func TestASubmitIsValidatedAgainstTheFlagsOfItsCategory(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	require.Equal(t, http.StatusOK, e.setQuotas(w, "OTHER_PART_DAY", map[string]any{"default_days": 20}).Status)
	require.Equal(t, http.StatusOK, e.setQuotas(w, "CHANGE_DAY", map[string]any{"default_days": 20}).Status)

	with := func(base map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for key, val := range base {
			out[key] = val
		}
		out[k] = v
		return out
	}
	normal := leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-19")
	timed := leaveBody("PERMIT", "OTHER_PART_DAY", "2026-03-18", "2026-03-18")
	changeDay := leaveBody("PERMIT", "CHANGE_DAY", "2026-03-18", "2026-03-18")
	sick := leaveBody("SICK", "", "2026-03-18", "2026-03-18")

	cases := []struct {
		name   string
		body   map[string]any
		fields []string
	}{
		{"no reason", with(normal, "reason", ""), []string{"reason"}},
		{"a reason of 501 characters", with(normal, "reason", strings.Repeat("a", 501)), []string{"reason"}},
		{"end before start", with(normal, "end_date", "2026-03-17"), []string{"end_date"}},
		{"a range of over a year", with(normal, "end_date", "2027-12-31"), []string{"end_date"}},
		{"no start date", with(normal, "start_date", nil), []string{"start_date"}},
		{"a time on a normal category", with(with(normal, "start_time", "08:00"), "end_time", "09:00"), []string{"start_time"}},
		{"one time only", with(timed, "start_time", "08:00"), []string{"end_time"}},
		{"a timed category without times", timed, []string{"start_time"}},
		{"a time that is not a time", with(with(timed, "start_time", "8am"), "end_time", "10:00"), []string{"start_time"}},
		{"end time before start time", with(with(timed, "start_time", "10:00"), "end_time", "09:00"), []string{"end_time"}},
		{"change days on a normal category", with(with(normal, "change_day_from", "2026-03-25"), "change_day_to", "2026-03-25"), []string{"change_day_from"}},
		{"change day without its days", changeDay, []string{"change_day_from"}},
		{"sick without the sick fields", sick, []string{"hospitalization", "letter_status"}},
		{"a letter without its photo", with(with(with(sick, "hospitalization", false), "letter_status", true), "media_ids", []int64{}), []string{"media_ids"}},
		{"sick fields on a leave", with(normal, "hospitalization", true), []string{"hospitalization"}},
		{"an unknown type", with(normal, "type", "OVERTIME"), []string{"type"}},
	}
	for _, c := range cases {
		r := e.keyed(w.guard, http.MethodPost, "/permissions", c.body, e.key())
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "%s: %s", c.name, r.Raw)
		require.ElementsMatch(t, c.fields, fieldsOf(t, r), c.name)
	}

	// Category and regu.
	e.submit(w.guard, leaveBody("PERMIT", "UNPAID", "2026-03-18", "2026-03-18")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_CATEGORY_INVALID")
	e.submit(w.guard, leaveBody("LEAVE", "NOPE", "2026-03-18", "2026-03-18")).
		requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_CATEGORY_NOT_FOUND")
	e.api(w.admin, http.MethodPatch, "/admin/permission-categories/UNPAID", map[string]any{"is_active": false})
	e.submit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18")).
		requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "LEAVE_CATEGORY_INVALID")
	body := leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-18")
	body["team_code"] = w.otherTeam
	require.Equal(t, http.StatusForbidden, e.submit(w.guard, body).Status, "a regu the person does not serve in")
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_requests"))
}

func TestASickLetterIsAttachedAndAFailedSubmitUsesNoNumber(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	photo := func(owner int64, purpose string) int64 {
		var id int64
		e.inTenant(w.org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), `
INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
VALUES ($1, $2, 'image/jpeg', 1000, 'test/' || gen_random_uuid()::text, 'UPLOADED', 'CAMERA') RETURNING id`, owner, purpose).Scan(&id))
		})
		return id
	}
	sick := func(media int64) map[string]any {
		b := leaveBody("SICK", "SICK", "2026-03-18", "2026-03-19")
		b["hospitalization"], b["letter_status"], b["disease"], b["media_ids"] = false, true, "Demam", []int64{media}
		return b
	}

	// Another person's photo, and one of another purpose, are refused; the request is not left behind.
	r := e.submit(w.guard, sick(photo(w.danruID, "LEAVE_ATTACHMENT")))
	require.Equal(t, http.StatusNotFound, r.Status, "body: %s", r.Raw)
	e.submit(w.guard, sick(photo(w.guardID, "ATTENDANCE_SELFIE"))).requireFailure(t, http.StatusUnprocessableEntity, "UNPROCESSABLE", "MEDIA_PURPOSE_MISMATCH")
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_requests"))
	require.Zero(t, e.count(w.org, "SELECT coalesce(sum(counter), 0) FROM leave_request_counters"), "the number was not used up")

	mine := photo(w.guardID, "LEAVE_ATTACHMENT")
	got := e.mustSubmit(w.guard, sick(mine))
	require.Equal(t, "SR20260300001", got.Number)
	require.Equal(t, mine, got.Attachment.ID)
	require.Contains(t, got.Attachment.URL, "https://files.test/")
	require.True(t, got.Sick.LetterStatus)
	require.Equal(t, "Demam", *got.Sick.Disease)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM media WHERE id = $1 AND attached_entity_type = 'LEAVE_REQUEST' AND attached_entity_id = $2", mine, got.ID))
	// The photo is attached once.
	again := sick(mine)
	again["start_date"], again["end_date"] = "2026-06-01", "2026-06-02"
	e.submit(w.guard, again).requireFailure(t, http.StatusConflict, "CONFLICT", "MEDIA_ALREADY_ATTACHED")
}

func TestTheNumbersOfParallelSubmitsAreDifferent(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	const people = 6
	tokens := make([]string, people)
	for i := range tokens {
		id, token := e.person(w.org, "")
		e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", id)
		tokens[i] = token
	}
	numbers := make([]string, people)
	var wg sync.WaitGroup
	for i, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := leaveBody("SICK", "", "2026-03-18", "2026-03-18")
			body["hospitalization"], body["letter_status"] = false, false
			r := e.keyed(token, http.MethodPost, "/permissions", body, e.key())
			if r.Status == http.StatusCreated {
				var env struct {
					Data permission `json:"data"`
				}
				_ = json.Unmarshal(r.Raw, &env)
				numbers[i] = env.Data.Number
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, n := range numbers {
		require.NotEmpty(t, n, "every submit succeeded")
		require.False(t, seen[n], "number %s given twice", n)
		seen[n] = true
	}
	require.Len(t, seen, people)
}

// ---------- cancel ----------

func TestCancelGivesAnnualDaysBackOnceAndOnlyToTheOwnerWhilePending(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))
	require.Equal(t, 2, e.balance(w, w.guardID))

	// Someone else's request is not found (never 403).
	otherID, otherToken := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", otherID)
	e.cancel(otherToken, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	e.cancel(w.danru, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")

	r := e.cancel(w.guard, got.ID)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	canceled := data[permission](t, r)
	require.Equal(t, "CANCELLED", canceled.Status)
	require.False(t, canceled.CanCancel)
	require.Equal(t, 5, e.balance(w, w.guardID))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM audit_logs WHERE action = 'leave.request.cancel'"))

	e.cancel(w.guard, got.ID).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_ALREADY_CANCELLED")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'REFUND'"))

	// A decided request cannot be canceled.
	for _, status := range []string{"APPROVED", "REJECTED"} {
		again := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-04-01", "2026-04-01"))
		e.setStatus(w.org, again.ID, status)
		e.cancel(w.guard, again.ID).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_CANNOT_CANCEL")
		e.setStatus(w.org, again.ID, "CANCELLED") // out of the way of the next one
	}
}

func TestTwoCancelsAtOnceRefundOnce(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = e.keyed(w.guard, http.MethodPost, "/permissions/"+strconv.FormatInt(got.ID, 10)+"/cancel", nil, e.key()).Status
		}()
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'REFUND'"))
	require.Equal(t, 5, e.balance(w, w.guardID))
}

// ---------- list and detail ----------

func TestTheOwnListIsNewestFirstAndFiltered(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	a := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-18"))
	b := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-04-01", "2026-04-02"))
	sick := leaveBody("SICK", "", "2026-05-04", "2026-05-04")
	sick["hospitalization"], sick["letter_status"] = false, false
	c := e.mustSubmit(w.guard, sick)
	require.Equal(t, http.StatusOK, e.cancel(w.guard, b.ID).Status)

	list, total := e.mine(w.guard, "")
	require.Equal(t, 3, total)
	require.Equal(t, []int64{c.ID, b.ID, a.ID}, []int64{list[0].ID, list[1].ID, list[2].ID})

	list, _ = e.mine(w.guard, "?type=SICK")
	require.Len(t, list, 1)
	list, _ = e.mine(w.guard, "?type=SICK,LEAVE")
	require.Len(t, list, 3)
	list, _ = e.mine(w.guard, "?status=CANCELLED")
	require.Equal(t, b.ID, list[0].ID)
	list, _ = e.mine(w.guard, "?status=PENDING,CANCELLED")
	require.Len(t, list, 3)
	list, _ = e.mine(w.guard, "?from_date=2026-04-02&to_date=2026-04-30")
	require.Len(t, list, 1, "the days intersect the range")
	require.Equal(t, b.ID, list[0].ID)
	list, total = e.mine(w.guard, "?limit=2&page=2")
	require.Equal(t, 3, total)
	require.Len(t, list, 1)
	require.Equal(t, a.ID, list[0].ID)

	// Other people's requests are not on it, and a bad filter is refused.
	memberID, token := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', '2000-01-01')", memberID)
	list, total = e.mine(token, "")
	require.Empty(t, list)
	require.Zero(t, total)
	e.raw(w.guard, http.MethodGet, "/permissions/me?type=NOPE", nil).requireFailure(t, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "")
}

func TestARequestIsReadByItsOwnerAndTheDanruOfItsRegu(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))

	require.Equal(t, got.ID, data[permission](t, e.detail(w.guard, got.ID)).ID)
	require.Equal(t, got.ID, data[permission](t, e.detail(w.danru, got.ID)).ID, "the DANRU of the regu")

	// Anyone else, a missing id and a malformed one are the same 404.
	_, stranger := e.person(w.org, "")
	e.detail(stranger, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	otherDanruID, otherDanru := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, 'DANRU', '2000-01-01')", w.otherTeam, otherDanruID)
	e.detail(otherDanru, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	e.detail(w.guard, got.ID+1000).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	e.raw(w.guard, http.MethodGet, "/permissions/abc", nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")

	// Another organization's token finds nothing either.
	org2 := testkit.NewOrg(t)
	_, token2 := e.person(org2, "")
	e.detail(token2, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
}

// The detail follows the same rule as the queue and the decision: whoever leads the regu today,
// not whoever led it on the request's start date.
func TestADanruReadsARequestByTodayNotByItsStartDate(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))

	// The DANRU changes before the start date: the old one steps down, a new one takes over today.
	e.exec(w.org, "UPDATE team_members SET end_date = '2026-03-09' WHERE team_code = '1K-IBMI-S1' AND personnel_id = $1 AND role = 'DANRU'", w.danruID)
	newID, newDanru := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'DANRU', '2026-03-10')", newID)

	require.Equal(t, got.ID, data[permission](t, e.detail(newDanru, got.ID)).ID, "the DANRU of today")
	e.detail(w.danru, got.ID).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	require.Equal(t, got.ID, data[permission](t, e.detail(w.guard, got.ID)).ID, "the owner still reads it")
}

func TestADecidedRequestShowsWhoDecidedIt(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))
	e.exec(w.org, "UPDATE leave_requests SET status = 'APPROVED', decided_level = 'DANRU', decided_by_id = $2, decided_at = now(), decision_note = 'Silakan' WHERE id = $1", got.ID, w.danruID)

	d := data[permission](t, e.detail(w.guard, got.ID))
	require.Equal(t, "APPROVED", d.Status)
	require.False(t, d.CanCancel)
	require.Equal(t, "DANRU", d.Decision.Level)
	require.Equal(t, "Pak", d.Decision.ByName, "the first name of the person who decided")
	require.Equal(t, "Silakan", *d.Decision.Note)
}
