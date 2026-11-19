package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Phase 10.11: patrol schedules.

type scheduleOut struct {
	ID          string `json:"id"`
	ProjectCode string `json:"project_code"`
	Zone        struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"zone"`
	Personnel struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"personnel"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      time.Time  `json:"ends_at"`
	WorkDate    string     `json:"work_date"`
	Notes       *string    `json:"notes"`
	Status      string     `json:"status"`
	CancelledAt *time.Time `json:"cancelled_at"`
	ScanCount   int        `json:"scan_count"`
	Patrol      *struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"patrol"`
	StartDelayMinutes *int `json:"start_delay_minutes"`
}

const schedulesPath = "/api/v1/admin/patrol-schedules"

// slot is a window starting `in` from now and lasting `d`, with the offset a client sends.
func slot(in, d time.Duration) map[string]any {
	start := time.Now().Add(in).Truncate(time.Second)
	return slotAt(start, d)
}

func slotAt(start time.Time, d time.Duration) map[string]any {
	loc, _ := time.LoadLocation("Asia/Jakarta")
	return map[string]any{"starts_at": start.In(loc).Format(time.RFC3339), "ends_at": start.Add(d).In(loc).Format(time.RFC3339)}
}

func (f *flow) createSchedules(zone int64, people []int64, slots ...map[string]any) (int, []scheduleOut, []byte) {
	f.t.Helper()
	body := map[string]any{"zone_id": zone, "personnel_ids": people, "slots": slots}
	code, raw := f.do(http.MethodPost, schedulesPath, f.adminTok, jsonBody(f.t, body), nil)
	var env struct {
		Data []scheduleOut `json:"data"`
	}
	if code == http.StatusCreated {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) scheduleList(token, path string) (int, []scheduleOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, path, token, nil, nil)
	var env struct {
		Data []scheduleOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) scheduleCount() int {
	f.t.Helper()
	var n int
	f.query([]any{&n}, "SELECT count(*) FROM patrol_schedules")
	return n
}

func TestPatrolScheduleCreate(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	other, _ := f.addMember("1K-IBMI-S1", "MEMBER", 900801, "JADWAL0001", "Anggota Jadwal")

	t.Run("two people x two slots make four rows", func(t *testing.T) {
		code, got, raw := f.createSchedules(z.ID, []int64{f.person, other}, slot(2*time.Hour, 2*time.Hour), slot(6*time.Hour, 2*time.Hour))
		require.Equal(t, http.StatusCreated, code, string(raw))
		require.Len(t, got, 4)
		for _, s := range got {
			require.Equal(t, "SCHEDULED", s.Status)
			require.Equal(t, z.ID, s.Zone.ID)
			require.Equal(t, z.Name, s.Zone.Name)
			require.Equal(t, "PATROL", s.Zone.Type)
			require.NotEmpty(t, s.Personnel.FullName)
		}
		require.Equal(t, 4, f.scheduleCount())
		require.Equal(t, 1, f.countInbox(f.personTok, "PATROL_SCHEDULE_CHANGED"), "one notice per person per request, not per slot")
	})

	t.Run("an overlap rejects the whole request, lists every conflict and writes nothing", func(t *testing.T) {
		// Overlaps the first existing slot of f.person and (with its own second slot) the second one.
		code, _, raw := f.createSchedules(z.ID, []int64{f.person, other},
			slot(3*time.Hour, 30*time.Minute), slot(7*time.Hour, 30*time.Minute), slot(20*time.Hour, time.Hour))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_OVERLAP")
		var env struct {
			Error struct {
				Details struct {
					Conflicts []map[string]any `json:"conflicts"`
				} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Len(t, env.Error.Details.Conflicts, 4, "two people x two clashes")
		require.Equal(t, 4, f.scheduleCount(), "nothing was written")
	})

	t.Run("two slots of one request may not overlap each other", func(t *testing.T) {
		code, _, raw := f.createSchedules(z.ID, []int64{f.person}, slot(30*time.Hour, 2*time.Hour), slot(31*time.Hour, 2*time.Hour))
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_OVERLAP")
	})

	t.Run("a person who is not in the zone's project is refused by name", func(t *testing.T) {
		stranger := f.addPerson(900802, "ORANGLAIN7", "Orang Lain Tujuh")
		code, _, raw := f.createSchedules(z.ID, []int64{stranger}, slot(40*time.Hour, time.Hour))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "SCHEDULE_PERSON_NOT_ALLOWED")
		require.Contains(t, string(raw), strconv.FormatInt(stranger, 10))
	})

	t.Run("a person of another regu than the zone's is refused", func(t *testing.T) {
		zs2 := f.newZone(map[string]any{"name": "Zona Regu Dua", "team_code": "1K-IBMI-S2"})
		code, _, raw := f.createSchedules(zs2.ID, []int64{f.person}, slot(41*time.Hour, time.Hour))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "SCHEDULE_PERSON_NOT_ALLOWED")
	})

	t.Run("a window that is over, too long, backwards or without an offset is refused", func(t *testing.T) {
		code, _, raw := f.createSchedules(z.ID, []int64{f.person}, slot(-3*time.Hour, time.Hour))
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Contains(t, fields, "slots[0].ends_at")

		code, _, raw = f.createSchedules(z.ID, []int64{f.person}, slot(50*time.Hour, 25*time.Hour))
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, _, raw = f.createSchedules(z.ID, []int64{f.person}, map[string]any{
			"starts_at": time.Now().Add(60 * time.Hour).Format(time.RFC3339), "ends_at": time.Now().Add(59 * time.Hour).Format(time.RFC3339),
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))

		code, raw = f.do(http.MethodPost, schedulesPath, f.adminTok, []byte(`{"zone_id":`+strconv.FormatInt(z.ID, 10)+`,"personnel_ids":[`+
			strconv.FormatInt(f.person, 10)+`],"slots":[{"starts_at":"2031-01-01T10:00:00","ends_at":"2031-01-01T11:00:00"}]}`), nil)
		// A time with no offset is not a time: the body decoder refuses it, as it does any value of the wrong shape.
		require.Equal(t, http.StatusBadRequest, code, string(raw))
	})

	t.Run("more than 200 rows is refused", func(t *testing.T) {
		var slots []map[string]any
		for i := range 101 {
			slots = append(slots, slot(time.Duration(100+i*2)*time.Hour, time.Hour))
		}
		code, _, raw := f.createSchedules(z.ID, []int64{f.person, other}, slots...)
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "PATROL_SCHEDULE_TOO_MANY")
	})

	t.Run("an unknown or inactive zone and an unknown person are refused", func(t *testing.T) {
		code, _, raw := f.createSchedules(99999999, []int64{f.person}, slot(70*time.Hour, time.Hour))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
		code, _, raw = f.createSchedules(z.ID, []int64{99999999}, slot(70*time.Hour, time.Hour))
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		dead := f.newZone(map[string]any{"name": "Zona Mati"})
		f.exec("UPDATE patrol_zones SET active = false WHERE id = $1", dead.ID)
		code, _, raw = f.createSchedules(dead.ID, []int64{f.person}, slot(70*time.Hour, time.Hour))
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a member cannot use the admin routes", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, schedulesPath, f.personTok, nil, nil)
		require.Equal(t, http.StatusForbidden, code, string(raw))
	})
}

func TestPatrolScheduleWorkDateOfAWindowThatCrossesMidnight(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	loc, _ := time.LoadLocation("Asia/Jakarta")
	tomorrow := time.Now().In(loc).AddDate(0, 0, 1)
	start := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 22, 0, 0, 0, loc)

	code, got, raw := f.createSchedules(z.ID, []int64{f.person}, slotAt(start, 8*time.Hour))
	require.Equal(t, http.StatusCreated, code, string(raw))
	require.Equal(t, start.Format(time.DateOnly), got[0].WorkDate, "the date the window started on")
}

func TestPatrolScheduleConcurrentAdminsTakeTurns(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _, _ = f.createSchedules(z.ID, []int64{f.person}, slot(5*time.Hour, 2*time.Hour))
		}()
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusCreated, http.StatusConflict}, codes, "one of two overlapping requests wins")
	require.Equal(t, 1, f.scheduleCount())
}

func TestPatrolScheduleEditCancelAndStatus(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(1, nil)
	other, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900803, "JADWAL0003", "Anggota Tiga")
	_, made, _ := f.createSchedules(z.ID, []int64{f.person}, slot(2*time.Hour, 2*time.Hour))
	sc := made[0]
	path := schedulesPath + "/" + sc.ID
	before := f.countInbox(f.personTok, "PATROL_SCHEDULE_CHANGED")

	t.Run("editing the notes works and the person is told once", func(t *testing.T) {
		code, raw := f.do(http.MethodPatch, path, f.adminTok, jsonBody(t, map[string]any{"notes": "bawa senter"}), nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, before+1, f.countInbox(f.personTok, "PATROL_SCHEDULE_CHANGED"))
		var got string
		f.query([]any{&got}, "SELECT data->>'change' FROM notifications WHERE type = 'PATROL_SCHEDULE_CHANGED' ORDER BY id DESC LIMIT 1")
		require.Equal(t, "UPDATED", got)
	})

	t.Run("moving the window keeps the rules: an overlap with another schedule of the person is refused", func(t *testing.T) {
		_, _, _ = f.createSchedules(z.ID, []int64{f.person}, slot(10*time.Hour, time.Hour))
		code, raw := f.do(http.MethodPatch, path, f.adminTok, jsonBody(t, map[string]any{
			"starts_at": time.Now().Add(10 * time.Hour).Format(time.RFC3339), "ends_at": time.Now().Add(11 * time.Hour).Format(time.RFC3339),
		}), nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_OVERLAP")
	})

	t.Run("moving it to another person tells the old one it is cancelled and the new one it was made", func(t *testing.T) {
		oldBefore := f.countInbox(f.personTok, "PATROL_SCHEDULE_CHANGED")
		newBefore := f.countInbox(otherTok, "PATROL_SCHEDULE_CHANGED")
		code, raw := f.do(http.MethodPatch, path, f.adminTok, jsonBody(t, map[string]any{"personnel_id": other}), nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, oldBefore+1, f.countInbox(f.personTok, "PATROL_SCHEDULE_CHANGED"))
		require.Equal(t, newBefore+1, f.countInbox(otherTok, "PATROL_SCHEDULE_CHANGED"))
	})

	t.Run("cancelling keeps the row, shows CANCELLED, notifies, and locks it", func(t *testing.T) {
		n := f.countInbox(otherTok, "PATROL_SCHEDULE_CHANGED")
		code, raw := f.do(http.MethodDelete, path, f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, n+1, f.countInbox(otherTok, "PATROL_SCHEDULE_CHANGED"))
		code, list, raw := f.scheduleList(f.adminTok, schedulesPath+"?status=CANCELLED")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 1)
		require.Equal(t, "CANCELLED", list[0].Status)
		require.NotNil(t, list[0].CancelledAt)

		code, raw = f.do(http.MethodPatch, path, f.adminTok, jsonBody(t, map[string]any{"notes": "x"}), nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_LOCKED")
		code, raw = f.do(http.MethodDelete, path, f.adminTok, nil, nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_LOCKED")

		// The window it held is free again.
		code, _, raw = f.createSchedules(z.ID, []int64{other}, slot(2*time.Hour, 2*time.Hour))
		require.Equal(t, http.StatusCreated, code, string(raw))
	})

	t.Run("a window that is over with no patrol is MISSED, and locked", func(t *testing.T) {
		var id string
		f.query([]any{&id}, `INSERT INTO patrol_schedules (project_code, zone_id, personnel_id, starts_at, ends_at, work_date, created_by_id)
		    VALUES ('1K-IBMI-S', $1, $2, now() - interval '5 hours', now() - interval '4 hours', current_date, $2) RETURNING id::text`, z.ID, f.person)
		code, list, raw := f.scheduleList(f.adminTok, schedulesPath+"?status=MISSED")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 1)
		require.Equal(t, "MISSED", list[0].Status)
		code, raw = f.do(http.MethodDelete, schedulesPath+"/"+id, f.adminTok, nil, nil)
		reasonIs(t, code, raw, http.StatusConflict, "PATROL_SCHEDULE_LOCKED")
	})

	t.Run("an unknown or non-uuid id is 404", func(t *testing.T) {
		for _, id := range []string{"nope", "00000000-0000-0000-0000-000000000000"} {
			code, raw := f.do(http.MethodPatch, schedulesPath+"/"+id, f.adminTok, jsonBody(t, map[string]any{"notes": "x"}), nil)
			reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")
		}
	})
}

func TestPatrolScheduleFieldRead(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedRound(1, nil)
	other, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900804, "JADWAL0004", "Anggota Empat")
	stranger := f.addPerson(900805, "ORANGLAIN8", "Orang Lain Delapan")
	strangerTok := f.token(stranger, f.addSession(stranger, "tok-stranger8"))
	_, mine, _ := f.createSchedules(z.ID, []int64{f.person}, slot(2*time.Hour, time.Hour))
	_, theirs, _ := f.createSchedules(z.ID, []int64{other}, slot(2*time.Hour, time.Hour))

	t.Run("a member sees only their own", func(t *testing.T) {
		code, list, raw := f.scheduleList(f.personTok, "/api/v1/patrol-schedules")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 1)
		require.Equal(t, mine[0].ID, list[0].ID)
		code, list, _ = f.scheduleList(f.personTok, "/api/v1/patrol-schedules?personnel_id="+strconv.FormatInt(other, 10))
		require.Equal(t, http.StatusOK, code)
		require.Empty(t, list, "asking for another person's gives nothing")
		code, raw = f.do(http.MethodGet, "/api/v1/patrol-schedules/"+theirs[0].ID, f.personTok, nil, nil)
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")
	})

	t.Run("a DANRU sees the people of their regu", func(t *testing.T) {
		_, list, _ := f.scheduleList(danruTok, "/api/v1/patrol-schedules")
		require.Len(t, list, 2)
		code, raw := f.do(http.MethodGet, "/api/v1/patrol-schedules/"+theirs[0].ID, danruTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
	})

	t.Run("the default range is today to a week ahead, and a range can be given", func(t *testing.T) {
		_, far, _ := f.createSchedules(z.ID, []int64{f.person}, slot(20*24*time.Hour, time.Hour))
		_, list, _ := f.scheduleList(f.personTok, "/api/v1/patrol-schedules")
		require.Len(t, list, 1, "a schedule three weeks ahead is outside the default range")
		loc, _ := time.LoadLocation("Asia/Jakarta")
		day := far[0].StartsAt.In(loc).Format(time.DateOnly)
		_, list, _ = f.scheduleList(f.personTok, "/api/v1/patrol-schedules?date_from="+day+"&date_to="+day)
		require.Len(t, list, 1)
		require.Equal(t, far[0].ID, list[0].ID)
		code, _, raw := f.scheduleList(f.personTok, "/api/v1/patrol-schedules?date_from=2031-02-01&date_to=2031-01-01")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, _, raw = f.scheduleList(f.personTok, "/api/v1/patrol-schedules?status=BOGUS")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("someone outside the project sees none, and another organization's schedule is 404", func(t *testing.T) {
		_, list, _ := f.scheduleList(strangerTok, "/api/v1/patrol-schedules")
		require.Empty(t, list)
		code, raw := f.do(http.MethodGet, "/api/v1/patrol-schedules/"+mine[0].ID, strangerTok, nil, nil)
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")

		foreign, _, err := f.tokens.NewAccessToken(900999, 2, 5999)
		require.NoError(t, err)
		code, raw = f.do(http.MethodGet, "/api/v1/patrol-schedules/"+mine[0].ID, foreign, nil, nil)
		reasonIs(t, code, raw, http.StatusNotFound, "PATROL_SCHEDULE_NOT_FOUND")
	})
	_ = otherTok
}

func TestPatrolScheduleSchemaRefusesWhatTheDatabaseOwns(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")
	zone := f.addZone("1K-IBMI-S", "Zona Skema Jadwal")
	otherZone := f.addZone("1K-IBMI-C", "Zona Skema Lain")
	insert := func(project string, zoneID int64, from, to string) error {
		return f.tryExec(`INSERT INTO patrol_schedules (project_code, zone_id, personnel_id, starts_at, ends_at, work_date, created_by_id)
		    VALUES ($1, $2, $3, now() + $4::interval, now() + $5::interval, current_date, $3)`, project, zoneID, f.person, from, to)
	}

	require.NoError(t, insert("1K-IBMI-S", zone, "1 hour", "2 hours"))
	require.Error(t, insert("1K-IBMI-S", zone, "2 hours", "2 hours"), "a window must have a length")
	require.Error(t, insert("1K-IBMI-S", zone, "2 hours", "1 hour"), "and go forward")
	require.Error(t, insert("1K-IBMI-S", zone, "1 hour", "26 hours"), "and be at most 24 hours")
	require.NoError(t, insert("1K-IBMI-S", zone, "1 hour", "25 hours"), "exactly 24 hours is allowed")
	require.Error(t, insert("1K-IBMI-S", otherZone, "1 hour", "2 hours"), "a zone of another project than the row's")
	require.Error(t, f.tryExec("DELETE FROM patrol_schedules"), "the app role cannot delete a schedule")
}
