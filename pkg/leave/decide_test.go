package leave_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/leave"
)

// sentNotices records the notices the service sends, in place of the notification module.
type sentNotices struct {
	mu   sync.Mutex
	list []leave.Notice
}

func (s *sentNotices) NotifyLeave(_ context.Context, n leave.Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, n)
}

func (s *sentNotices) all() []leave.Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]leave.Notice(nil), s.list...)
}

// decide is POST {prefix}/permissions/{id}/{action}, checked against the contract. prefix is ""
// for the DANRU's route and "/admin" for an admin's.
func (e *env) decide(token, prefix, action string, id int64, body map[string]any) reply {
	e.t.Helper()
	e.client.Token = token
	defer func() { e.client.Token = "" }()
	var payload any
	if body != nil {
		payload = body
	}
	res := e.client.DoWithHeaders(http.MethodPost, prefix+"/permissions/"+strconv.FormatInt(id, 10)+"/"+action, payload,
		map[string]string{"Idempotency-Key": e.key()})
	return reply{Status: res.Status, Raw: res.Body}
}

func (e *env) approve(token string, id int64) reply {
	e.t.Helper()
	return e.decide(token, "", "approve", id, nil)
}

func (e *env) decline(token string, id int64, note string) reply {
	e.t.Helper()
	return e.decide(token, "", "decline", id, map[string]any{"note": note})
}

// shift adds the Day shift (08:00-17:00) and returns its id.
func (e *env) shift(w world) (id int64) {
	e.t.Helper()
	e.inTenant(w.org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO shifts (name, start_minute, end_minute, tolerance_minutes) VALUES ('Day', 480, 1020, 15) RETURNING id").Scan(&id))
	})
	return id
}

func (e *env) schedule(w world, personnelID, shiftID int64, days ...string) {
	e.t.Helper()
	for _, d := range days {
		e.exec(w.org, "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES ($1, $2, $3)", personnelID, d, shiftID)
	}
}

func (e *env) member(w world, team string, role string) (int64, string) {
	e.t.Helper()
	id, token := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, $3::team_role, '2000-01-01')", team, id, role)
	return id, token
}

func (e *env) outboxTypes(org int64) (types []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		rows, err := tx.Query(e.t.Context(), "SELECT payload->>'type' FROM personel_outbox WHERE kind = 'LEAVE' ORDER BY id")
		require.NoError(e.t, err)
		defer rows.Close()
		for rows.Next() {
			var s string
			require.NoError(e.t, rows.Scan(&s))
			types = append(types, s)
		}
		require.NoError(e.t, rows.Err())
	})
	return types
}

func (e *env) leaveRows(org, requestID int64) int {
	e.t.Helper()
	return e.count(org, "SELECT count(*) FROM attendances WHERE status = 'LEAVE' AND leave_request_id = $1", requestID)
}

// ---------- the DANRU's decision ----------

func TestADanruApprovalWritesTheLeaveDaysAndTellsTheOwner(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	day := e.shift(w)
	// The guard serves in two regu, so every leave day is two records.
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-OTHR-S1', $1, 'MEMBER', '2000-01-01')", w.guardID)
	e.schedule(w, w.guardID, day, "2026-03-18", "2026-03-19", "2026-03-20")
	// The guard already came to work in the first regu on the 19th.
	e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status, check_in_at, check_in_method)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', '2026-03-19', $2, '2026-03-19T01:00:00Z', '2026-03-19T10:00:00Z', 'Asia/Jakarta', 'PRESENT', '2026-03-19T01:05:00Z', 'SELFIE')`, w.guardID, day)

	body := leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-20")
	body["team_code"] = w.team // the guard serves in two regu: the request says which
	got := e.mustSubmit(w.guard, body)
	require.Equal(t, 3, got.WorkingDays)
	sent := e.sent.all()
	require.Len(t, sent, 1)
	require.Equal(t, leave.NoticeRequested, sent[0].Kind)
	require.Equal(t, []int64{w.danruID}, sent[0].RecipientIDs, "the DANRU of the regu is told")

	r := e.decide(w.danru, "", "approve", got.ID, map[string]any{"note": "Silakan"})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	approved := data[permission](t, r)
	require.Equal(t, "APPROVED", approved.Status)
	require.False(t, approved.CanCancel)
	require.Equal(t, "DANRU", approved.Decision.Level)
	require.Equal(t, "Pak", approved.Decision.ByName)
	require.Equal(t, "Silakan", *approved.Decision.Note)
	require.Equal(t, []skipped{{WorkDate: "2026-03-19", TeamCode: "1K-IBMI-S1"}}, approved.Skipped, "the day the guard worked")

	// 3 days x 2 regu, less the day already worked.
	require.Equal(t, 5, e.leaveRows(w.org, got.ID))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE status = 'PRESENT' AND leave_request_id IS NULL"), "the worked day stays")
	require.Equal(t, []string{"leave.approved"}, e.outboxTypes(w.org))

	sent = e.sent.all()
	require.Len(t, sent, 2)
	require.Equal(t, leave.NoticeDecided, sent[1].Kind)
	require.Equal(t, []int64{w.guardID}, sent[1].RecipientIDs)
	require.True(t, sent[1].Approved)

	// Nobody decides it twice, in either direction.
	e.approve(w.danru, got.ID).requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	e.decline(w.danru, got.ID, "Berubah pikiran").requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	require.Equal(t, 5, e.leaveRows(w.org, got.ID))
}

func TestADanruRejectionNeedsANoteAndGivesAnnualDaysBack(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))
	require.Equal(t, 2, e.balance(w, w.guardID))

	// A rejection without a reason is refused and changes nothing; a reason of only spaces too.
	for _, note := range []string{"", "   "} {
		r := e.decline(w.danru, got.ID, note)
		require.Equal(t, http.StatusUnprocessableEntity, r.Status, "body: %s", r.Raw)
		require.Equal(t, []string{"note"}, fieldsOf(t, r))
	}
	require.Equal(t, 2, e.balance(w, w.guardID))

	r := e.decline(w.danru, got.ID, "Tugas padat")
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	rejected := data[permission](t, r)
	require.Equal(t, "REJECTED", rejected.Status)
	require.Equal(t, "Tugas padat", *rejected.Decision.Note)
	require.Equal(t, 5, e.balance(w, w.guardID), "the days are back")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'REFUND'"))
	require.Empty(t, e.outboxTypes(w.org), "a request that was never approved was never reported to Personel")
	require.Zero(t, e.leaveRows(w.org, got.ID))

	// Final: no decision, no cancel; and the days are free to ask again.
	e.approve(w.danru, got.ID).requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	e.cancel(w.guard, got.ID).requireFailure(t, http.StatusConflict, "CONFLICT", "LEAVE_CANNOT_CANCEL")
	e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))

	sent := e.sent.all()
	require.Equal(t, leave.NoticeDecided, sent[1].Kind)
	require.False(t, sent[1].Approved)
}

func TestTheDecisionsOfTheDanruAndAnAdminAtOnceLetOneThrough(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-19"))

	id := strconv.FormatInt(got.ID, 10)
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, c := range []struct {
		token, path string
		body        any
	}{
		{w.danru, "/permissions/" + id + "/decline", map[string]any{"note": "Tidak"}},
		{w.admin, "/admin/permissions/" + id + "/approve", nil},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = e.keyed(c.token, http.MethodPost, c.path, c.body, e.key()).Status
		}()
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	// Whichever won, the days are accounted for exactly once.
	switch e.count(w.org, "SELECT count(*) FROM leave_requests WHERE status = 'APPROVED'") {
	case 1:
		require.Equal(t, 3, e.balance(w, w.guardID))
		require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'REFUND'"))
	default:
		require.Equal(t, 5, e.balance(w, w.guardID))
		require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_ledger WHERE kind = 'REFUND'"))
	}
}

// ---------- the admin ----------

func TestAnAdminOverrulesADanruApproval(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	day := e.shift(w)
	e.schedule(w, w.guardID, day, "2026-03-18", "2026-03-19", "2026-03-20")
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-20"))
	require.Equal(t, http.StatusOK, e.approve(w.danru, got.ID).Status)
	require.Equal(t, 3, e.leaveRows(w.org, got.ID))
	require.Equal(t, 2, e.balance(w, w.guardID))

	// An admin cannot approve what the DANRU approved; the DANRU cannot take it back; but an admin
	// may reject it, with a reason.
	e.decide(w.admin, "/admin", "approve", got.ID, nil).requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	e.decline(w.danru, got.ID, "Mau saya batalkan").requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	r := e.decide(w.admin, "/admin", "decline", got.ID, map[string]any{"note": "Bentrok dengan jadwal besar"})
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	overruled := data[permission](t, r)
	require.Equal(t, "REJECTED", overruled.Status)
	require.Equal(t, "ADMIN", overruled.Decision.Level)

	require.Zero(t, e.leaveRows(w.org, got.ID), "the leave days are taken back")
	require.Equal(t, 5, e.balance(w, w.guardID), "and the annual days refunded")
	require.Equal(t, []string{"leave.approved", "leave.revoked"}, e.outboxTypes(w.org))
	// Final, whoever asks.
	e.decide(w.admin, "/admin", "approve", got.ID, nil).requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
	e.decide(w.admin, "/admin", "decline", got.ID, map[string]any{"note": "lagi"}).requireFailure(t, http.StatusConflict, "CONFLICT", leave.ReasonAlreadyDecided)
}

func TestADanruOwnRequestIsDecidedByAnAdminAndNobodyDecidesTheirOwn(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	require.Equal(t, http.StatusOK, e.setQuotas(w, "UNPAID", map[string]any{"default_days": 10}).Status)

	own := e.mustSubmit(w.danru, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))
	require.Zero(t, e.count(w.org, "SELECT count(*) FROM leave_requests WHERE approver_personnel_id IS NOT NULL"), "a DANRU has no approver: the admins decide")
	require.Equal(t, http.StatusForbidden, e.approve(w.danru, own.ID).Status)

	// A DANRU of another regu reaches nothing of this one.
	_, otherDanru := e.member(w, w.otherTeam, "DANRU")
	guardsRequest := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))
	require.Equal(t, http.StatusForbidden, e.approve(otherDanru, guardsRequest.ID).Status)
	// The owner has no authority to decide at all.
	require.Equal(t, http.StatusForbidden, e.approve(w.guard, guardsRequest.ID).Status)

	// An admin who is also the owner may not decide their own either.
	r := e.decide(w.admin, "/admin", "approve", own.ID, nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Equal(t, "ADMIN", data[permission](t, r).Decision.Level)
	e.setStatus(w.org, guardsRequest.ID, "CANCELLED")
}

func TestARegionAdminDecidesOnlyInTheirRegion(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))

	// Under OS1, which this project belongs to.
	_, region1 := e.person(w.org, "REGION_ADMIN")
	// Another region.
	id2, region2 := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO regions (code, description) VALUES ('OS2', 'Outsourcing 2') ON CONFLICT DO NOTHING")
	e.exec(w.org, "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS2')", id2)

	id := strconv.FormatInt(got.ID, 10)
	e.api(region2, http.MethodGet, "/admin/permissions/"+id, nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	e.decide(region2, "/admin", "approve", got.ID, nil).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	e.decide(region2, "/admin", "decline", got.ID, map[string]any{"note": "x"}).requireFailure(t, http.StatusNotFound, "NOT_FOUND", "LEAVE_NOT_FOUND")
	require.Zero(t, listTotal(t, e, region2, "/admin/permissions"), "their list has nothing of OS1")
	require.Equal(t, 1, listTotal(t, e, region1, "/admin/permissions"))
	require.Equal(t, 1, listTotal(t, e, w.admin, "/admin/permissions?region_code=OS1&status=PENDING&team_code=1K-IBMI-S1&type=LEAVE"))
	require.Zero(t, listTotal(t, e, w.admin, "/admin/permissions?status=APPROVED"))
	require.Zero(t, listTotal(t, e, w.admin, "/admin/permissions?project_code=1K-OTHR-S"))
	require.Equal(t, 1, listTotal(t, e, w.admin, "/admin/permissions?q=Orang"))
	require.Zero(t, listTotal(t, e, w.admin, "/admin/permissions?q=Nobody"))

	detail := data[permission](t, e.api(region1, http.MethodGet, "/admin/permissions/"+id, nil))
	require.Equal(t, got.ID, detail.ID)
	require.NotNil(t, detail.Person)

	r := e.decide(region1, "/admin", "approve", got.ID, nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	require.Equal(t, "ADMIN", data[permission](t, r).Decision.Level)

	// A field user has no admin routes at all.
	require.Equal(t, http.StatusForbidden, e.raw(w.guard, http.MethodGet, "/admin/permissions", nil).Status)
}

func listTotal(t *testing.T, e *env, token, path string) int {
	t.Helper()
	r := e.api(token, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, r.Status, "body: %s", r.Raw)
	var env struct {
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(r.Raw, &env))
	return env.Pagination.Total
}

// ---------- the queue ----------

func TestTheQueueIsByTheLiveRosterNotTheFrozenApprover(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	otherID, _ := e.member(w, w.team, "MEMBER") // another guard, away on the days around
	away := e.request(w, otherID)
	e.setStatus(w.org, away, "APPROVED") // 2026-04-01..2026-04-03

	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-04-02", "2026-04-02"))

	list := func(token string) ([]permission, int) {
		r := e.api(token, http.MethodGet, "/permissions?status=PENDING", nil)
		if r.Status != http.StatusOK {
			return nil, r.Status
		}
		var env struct {
			Data []permission `json:"data"`
		}
		require.NoError(t, json.Unmarshal(r.Raw, &env))
		return env.Data, r.Status
	}
	items, status := list(w.danru)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, items, 1)
	item := items[0]
	require.Equal(t, got.ID, item.ID)
	require.NotEmpty(t, item.Person.Name)
	require.Equal(t, w.guardID, item.Person.PersonnelID)
	require.Len(t, item.OverlapsWith, 1, "another member is away on those days")
	require.Equal(t, "2026-04-01", item.OverlapsWith[0].StartDate)
	// 3 people in the regu (guard, DANRU, the one away): one is away, and the person asking would be too.
	require.Equal(t, coverage{Date: "2026-04-02", Present: 1, Size: 3}, *item.Coverage)

	// The DANRU changes. The request still names the old one, but the new one is who sees it.
	e.exec(w.org, "UPDATE team_members SET end_date = '2026-03-09' WHERE personnel_id = $1", w.danruID)
	newID, newDanru := e.person(w.org, "")
	e.exec(w.org, "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, 'DANRU', '2026-03-10')", w.team, newID)
	items, status = list(newDanru)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, items, 1)
	_, status = list(w.danru)
	require.Equal(t, http.StatusForbidden, status, "the old DANRU leads nothing now")
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM leave_requests WHERE approver_personnel_id = $1", w.danruID))
	require.Equal(t, http.StatusOK, e.approve(newDanru, got.ID).Status)

	// Naming a regu that is not theirs, or a member asking, is refused.
	e.api(newDanru, http.MethodGet, "/permissions?team_code=1K-OTHR-S1", nil).requireFailure(t, http.StatusForbidden, "FORBIDDEN", "PERMISSION_DENIED")
	require.Equal(t, http.StatusForbidden, e.raw(w.guard, http.MethodGet, "/permissions", nil).Status)
}

func TestAnAnnualRequestShowsTheBalanceBeforeAndAfterToWhoDecides(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "ANNUAL", "2026-03-18", "2026-03-19"))

	detail := data[permission](t, e.detail(w.danru, got.ID))
	require.Equal(t, balance{RemainingDays: 3, AfterApprovalDays: 1}, *detail.LeaveBalance)
	require.NotNil(t, detail.Person)
	// The owner sees none of it.
	own := data[permission](t, e.detail(w.guard, got.ID))
	require.Nil(t, own.LeaveBalance)
	require.Nil(t, own.Person)
}

// ---------- what an approval does to lateness ----------

func TestApprovedPermitsLowerTheLatenessOfACheckedInDayAndRevokingGivesItBack(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	day := e.shift(w)
	require.Equal(t, http.StatusOK, e.setQuotas(w, "ON_DUTY_FULL_DAY", map[string]any{"default_days": 10}).Status)
	require.Equal(t, http.StatusOK, e.setQuotas(w, "OTHER_PART_DAY", map[string]any{"default_days": 10}).Status)
	// The guard checked in at 08:30 for a shift that began at 08:00: 30 minutes late.
	e.exec(w.org, `INSERT INTO attendances (personnel_id, team_code, project_code, work_date, shift_id, shift_starts_at, shift_ends_at, timezone, status, check_in_at, check_in_method, late_minutes, late_minutes_raw)
VALUES ($1, '1K-IBMI-S1', '1K-IBMI-S', '2026-03-18', $2, '2026-03-18T01:00:00Z', '2026-03-18T10:00:00Z', 'Asia/Jakarta', 'PRESENT', '2026-03-18T01:30:00Z', 'SELFIE', 30, 30)`, w.guardID, day)
	late := func() (late, raw int) {
		e.inTenant(w.org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT late_minutes, late_minutes_raw FROM attendances WHERE work_date = '2026-03-18'").Scan(&late, &raw))
		})
		return late, raw
	}

	// A window of the morning takes its minutes off.
	timed := leaveBody("PERMIT", "OTHER_PART_DAY", "2026-03-18", "2026-03-18")
	timed["start_time"], timed["end_time"] = "08:00", "08:20"
	part := e.mustSubmit(w.guard, timed)
	require.Equal(t, http.StatusOK, e.approve(w.danru, part.ID).Status)
	l, raw := late()
	require.Equal(t, 10, l)
	require.Equal(t, 30, raw, "what the clock said is kept")
	require.Zero(t, e.leaveRows(w.org, part.ID), "a part of a day is no leave day")

	// Revoking it by an admin gives the minutes back.
	require.Equal(t, http.StatusOK, e.decide(w.admin, "/admin", "decline", part.ID, map[string]any{"note": "Keliru"}).Status)
	l, _ = late()
	require.Equal(t, 30, l)

	// A full day on duty excuses the lateness altogether.
	full := e.mustSubmit(w.guard, leaveBody("PERMIT", "ON_DUTY_FULL_DAY", "2026-03-18", "2026-03-18"))
	require.Equal(t, http.StatusOK, e.approve(w.danru, full.ID).Status)
	l, _ = late()
	require.Zero(t, l)
	require.Zero(t, e.leaveRows(w.org, full.ID))
	require.Equal(t, 1, e.count(w.org, "SELECT count(*) FROM attendances WHERE status = 'PRESENT'"), "the day stays a presence")
	require.Equal(t, http.StatusOK, e.decide(w.admin, "/admin", "decline", full.ID, map[string]any{"note": "Keliru"}).Status)
	l, _ = late()
	require.Equal(t, 30, l)
}

func TestACanceledRequestTellsTheSameRecipientsAsItsSubmit(t *testing.T) {
	e := newEnv(t)
	w := e.requestWorld()
	got := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))
	require.Equal(t, http.StatusOK, e.cancel(w.guard, got.ID).Status)

	sent := e.sent.all()
	require.Len(t, sent, 2)
	require.Equal(t, leave.NoticeRequested, sent[0].Kind)
	require.Equal(t, leave.NoticeCancelled, sent[1].Kind)
	require.Equal(t, sent[0].RecipientIDs, sent[1].RecipientIDs)

	// A regu with no DANRU tells the admins.
	e.exec(w.org, "UPDATE team_members SET end_date = '2026-03-09' WHERE role = 'DANRU'")
	again := e.mustSubmit(w.guard, leaveBody("LEAVE", "UNPAID", "2026-03-18", "2026-03-18"))
	require.NotZero(t, again.ID)
	last := e.sent.all()[2]
	require.Equal(t, leave.NoticeRequested, last.Kind)
	require.NotEmpty(t, last.RecipientIDs)
	require.NotContains(t, last.RecipientIDs, w.danruID)
	require.Contains(t, last.RecipientIDs, int64(e.count(w.org, "SELECT personnel_id FROM web_role_assignments WHERE role = 'ORG_ADMIN' LIMIT 1")))
}
