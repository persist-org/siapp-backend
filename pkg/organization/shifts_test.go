package organization_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/testkit"
)

// dateOf is day, under a name the tests' shift variables (often called day) do not hide.
var dateOf = day

// 2026-03-01 is a Sunday, so 2026-03-10 ("now" in these tests, 12:00 in Jakarta) is a Tuesday.

func id(r row) int64 { return int64(r["id"].(float64)) } //nolint:forcetypeassert // test helper

// shift creates a working shift; one that ends before it starts is declared overnight ("HH:mm"
// compares as text).
func (e *env) shift(admin, name, start, end string) int64 {
	e.t.Helper()
	return id(e.mustCreate(admin, "/admin/shifts", map[string]any{"name": name, "start_time": start, "end_time": end, "overnight": end < start}))
}

func (e *env) offShift(admin string) int64 {
	e.t.Helper()
	return id(e.mustCreate(admin, "/admin/shifts", map[string]any{"name": "OFF", "is_off": true}))
}

// weekGroup creates a group with weekdays on `work` and the weekend on `off`.
func (e *env) weekGroup(admin, name string, work, off int64) int64 {
	e.t.Helper()
	return id(e.mustCreate(admin, "/admin/shift-groups", map[string]any{"name": name, "days": map[string]any{
		"monday": work, "tuesday": work, "wednesday": work, "thursday": work, "friday": work, "saturday": off, "sunday": off,
	}}))
}

func (e *env) assign(token string, personnelID int64, body map[string]any) reply {
	e.t.Helper()
	return e.api(token, http.MethodPut, "/admin/personnel/"+strconv.FormatInt(personnelID, 10)+"/shift-assignment", body)
}

func (e *env) scheduleOf(org, personnelID int64) (rows []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		r, err := tx.Query(e.t.Context(), `
			SELECT s.work_date::text || ' ' || sh.name || ' ' || CASE WHEN s.shift_group_id IS NULL THEN 'manual' ELSE 'group' END
			FROM schedules s JOIN shifts sh ON sh.id = s.shift_id WHERE s.personnel_id = $1 ORDER BY s.work_date`, personnelID)
		require.NoError(e.t, err)
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		require.NoError(e.t, err)
	})
	return rows
}

func (e *env) scheduleCount(org, personnelID int64) int {
	e.t.Helper()
	return len(e.scheduleOf(org, personnelID))
}

func (e *env) manual(token string, personnelID int64, entries ...map[string]any) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(personnelID, 10)+"/schedules", map[string]any{"entries": entries})
}

func entry(date string, shiftID int64) map[string]any {
	return map[string]any{"work_date": date, "shift_id": shiftID}
}

func TestTheShiftMaster(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)
	regionAdmin := e.person(org, "REGION_ADMIN", "OS1")

	t.Run("working, overnight and OFF shifts", func(t *testing.T) {
		day := e.mustCreate(admin, "/admin/shifts", map[string]any{"name": "Day Shift", "start_time": "08:00", "end_time": "17:00"})
		require.Equal(t, "08:00", day.str("start_time"))
		require.Equal(t, false, day["overnight"])
		require.EqualValues(t, 15, day["tolerance_minutes"], "the default tolerance")
		night := e.mustCreate(admin, "/admin/shifts", map[string]any{"name": "Night Shift", "start_time": "22:00", "end_time": "06:00", "overnight": true, "tolerance_minutes": 10})
		require.Equal(t, true, night["overnight"], "it ends the next day")
		off := e.mustCreate(admin, "/admin/shifts", map[string]any{"name": "OFF", "is_off": true})
		require.Equal(t, true, off["is_off"])
		require.Equal(t, "00:00", off.str("start_time"))
		require.Equal(t, false, off["overnight"])
		require.EqualValues(t, 0, off["tolerance_minutes"], "an OFF day has no tolerance")
	})

	t.Run("bad times are SHIFT_INVALID_TIME", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"the same start and end":            {"name": "X1", "start_time": "08:00", "end_time": "08:00"},
			"no end":                            {"name": "X2", "start_time": "08:00"},
			"no times at all":                   {"name": "X2b"},
			"not HH:mm":                         {"name": "X3", "start_time": "8:00", "end_time": "17:00"},
			"hour 24":                           {"name": "X4", "start_time": "24:00", "end_time": "17:00"},
			"an empty time":                     {"name": "X4b", "start_time": "", "end_time": "17:00"},
			"an end before the start":           {"name": "X6", "start_time": "17:00", "end_time": "08:00"},
			"an end before the start, said so":  {"name": "X7", "start_time": "17:00", "end_time": "08:00", "overnight": false},
			"overnight with an end after start": {"name": "X8", "start_time": "08:00", "end_time": "17:00", "overnight": true},
			"an OFF day with times":             {"name": "X5", "is_off": true, "start_time": "08:00", "end_time": "17:00"},
			"an OFF day with 00:00":             {"name": "X9", "is_off": true, "start_time": "00:00", "end_time": "00:00"},
			"an OFF day with overnight":         {"name": "X10", "is_off": true, "overnight": false},
			"an OFF day with a tolerance":       {"name": "X11", "is_off": true, "tolerance_minutes": 0},
		} {
			t.Run(name, func(t *testing.T) {
				expect(t, e.raw(admin, http.MethodPost, "/admin/shifts", body), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonShiftInvalidTime)
			})
		}
		reversed := e.raw(admin, http.MethodPost, "/admin/shifts", map[string]any{"name": "X6", "start_time": "17:00", "end_time": "08:00"})
		require.Contains(t, string(reversed.Raw), `"end_time"`, "the field to fix is named")
		offFields := e.raw(admin, http.MethodPost, "/admin/shifts", map[string]any{"name": "X12", "is_off": true, "overnight": true, "tolerance_minutes": 5})
		require.Contains(t, string(offFields.Raw), `"overnight"`)
		require.Contains(t, string(offFields.Raw), `"tolerance_minutes"`)
	})

	t.Run("the tolerance must be shorter than the shift", func(t *testing.T) {
		short := e.raw(admin, http.MethodPost, "/admin/shifts", map[string]any{"name": "Short", "start_time": "08:00", "end_time": "08:10"})
		require.Equal(t, http.StatusUnprocessableEntity, short.Status, "the default 15 minutes is longer than 10")
		require.Contains(t, string(short.Raw), `"tolerance_minutes"`)
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPost, "/admin/shifts",
			map[string]any{"name": "Short", "start_time": "23:50", "end_time": "00:20", "overnight": true, "tolerance_minutes": 30}).Status, "30 minutes across midnight")
		require.Equal(t, http.StatusCreated, e.api(admin, http.MethodPost, "/admin/shifts",
			map[string]any{"name": "Short", "start_time": "23:50", "end_time": "00:20", "overnight": true, "tolerance_minutes": 29}).Status)
	})

	t.Run("a name is unique, ignoring case", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodPost, "/admin/shifts", map[string]any{"name": "day shift", "start_time": "07:00", "end_time": "15:00"}),
			http.StatusConflict, "DUPLICATE", organization.ReasonShiftNameTaken)
	})

	t.Run("a change keeps what is not sent, and an OFF day gets no times", func(t *testing.T) {
		list := e.raw(admin, http.MethodGet, "/admin/shifts?q=night", nil)
		night := data[[]row](t, list)[0]
		res := e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(id(night), 10), map[string]any{"end_time": "07:00"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[row](t, res)
		require.Equal(t, "22:00", got.str("start_time"))
		require.Equal(t, "07:00", got.str("end_time"))
		require.EqualValues(t, 10, got["tolerance_minutes"])

		nightPath := "/admin/shifts/" + strconv.FormatInt(id(night), 10)
		expect(t, e.api(admin, http.MethodPatch, nightPath, map[string]any{"end_time": "23:00"}),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonShiftInvalidTime) // still overnight: 23:00 is after 22:00
		res = e.api(admin, http.MethodPatch, nightPath, map[string]any{"end_time": "23:00", "overnight": false})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Equal(t, false, data[row](t, res)["overnight"])
		expect(t, e.api(admin, http.MethodPatch, nightPath, map[string]any{"overnight": true}),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonShiftInvalidTime) // 22:00-23:00 does not cross midnight
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPatch, nightPath, map[string]any{"tolerance_minutes": 60}).Status, "as long as the shift")
		res = e.api(admin, http.MethodPatch, nightPath, map[string]any{"end_time": "07:00", "overnight": true})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Equal(t, true, data[row](t, res)["overnight"])

		off := data[[]row](t, e.raw(admin, http.MethodGet, "/admin/shifts?q=off", nil))[0]
		offPath := "/admin/shifts/" + strconv.FormatInt(id(off), 10)
		for name, body := range map[string]map[string]any{
			"a start":     {"start_time": "08:00"},
			"an end":      {"end_time": "17:00"},
			"overnight":   {"overnight": false},
			"a tolerance": {"tolerance_minutes": 5},
		} {
			t.Run("an OFF day refuses "+name, func(t *testing.T) {
				expect(t, e.api(admin, http.MethodPatch, offPath, body), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonShiftInvalidTime)
			})
		}
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, offPath, map[string]any{"name": "Libur"}).Status, "an OFF day can still be renamed")
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPatch, offPath, map[string]any{"is_off": false}).Status, "is_off cannot change")
	})

	t.Run("a REGION_ADMIN reads the master but cannot change it", func(t *testing.T) {
		require.Len(t, data[[]row](t, e.api(regionAdmin, http.MethodGet, "/admin/shifts", nil)), 4)
		expect(t, e.api(regionAdmin, http.MethodPost, "/admin/shifts", map[string]any{"name": "Mine", "start_time": "07:00", "end_time": "15:00"}),
			http.StatusForbidden, "ROLE_DENIED", access.ReasonOrgAdminOnly)
		expect(t, e.api(regionAdmin, http.MethodDelete, "/admin/shifts/1", nil), http.StatusForbidden, "ROLE_DENIED", access.ReasonOrgAdminOnly)
		expect(t, e.api(regionAdmin, http.MethodPost, "/admin/shift-groups", map[string]any{"name": "G", "days": map[string]any{
			"monday": 1, "tuesday": 1, "wednesday": 1, "thursday": 1, "friday": 1, "saturday": 1, "sunday": 1}}),
			http.StatusForbidden, "ROLE_DENIED", access.ReasonOrgAdminOnly)
	})

	t.Run("a shift that does not exist", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodGet, "/admin/shifts/999999", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonShiftNotFound)
		expect(t, e.raw(admin, http.MethodGet, "/admin/shifts/abc", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonShiftNotFound)
	})
}

func TestShiftGroups(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)

	group := e.weekGroup(admin, "5 Hari", day, off)

	t.Run("a group has seven days, Monday first", func(t *testing.T) {
		got := data[row](t, e.api(admin, http.MethodGet, "/admin/shift-groups/"+strconv.FormatInt(group, 10), nil))
		days := got["days"].([]any) //nolint:forcetypeassert // test
		require.Len(t, days, 7)
		first := days[0].(map[string]any)  //nolint:forcetypeassert // test
		sunday := days[6].(map[string]any) //nolint:forcetypeassert // test
		require.Equal(t, "monday", first["weekday"])
		require.Equal(t, "Day", first["shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test
		require.Equal(t, "sunday", sunday["weekday"])
		require.Equal(t, true, sunday["shift"].(map[string]any)["is_off"]) //nolint:forcetypeassert // test
	})

	t.Run("every day is required, and must be an active shift", func(t *testing.T) {
		missing := e.raw(admin, http.MethodPost, "/admin/shift-groups", map[string]any{"name": "Kurang", "days": map[string]any{"monday": day}})
		require.Equal(t, http.StatusUnprocessableEntity, missing.Status)
		require.Contains(t, string(missing.Raw), `"days.tuesday"`)

		unknown := e.raw(admin, http.MethodPost, "/admin/shift-groups", map[string]any{"name": "Salah", "days": map[string]any{
			"monday": 999999, "tuesday": day, "wednesday": day, "thursday": day, "friday": day, "saturday": off, "sunday": off}})
		require.Equal(t, http.StatusUnprocessableEntity, unknown.Status)
		require.Contains(t, string(unknown.Raw), `"days.monday"`)

		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(night, 10), map[string]any{"is_active": false}).Status)
		inactive := e.raw(admin, http.MethodPatch, "/admin/shift-groups/"+strconv.FormatInt(group, 10), map[string]any{"days": map[string]any{"friday": night}})
		require.Equal(t, http.StatusUnprocessableEntity, inactive.Status)
		require.Contains(t, string(inactive.Raw), "tidak aktif")
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(night, 10), map[string]any{"is_active": true}).Status)

		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPatch, "/admin/shift-groups/"+strconv.FormatInt(group, 10), map[string]any{"days": map[string]any{"funday": day}}).Status)
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPatch, "/admin/shift-groups/"+strconv.FormatInt(group, 10), map[string]any{"days": map[string]any{"Monday": day}}).Status,
			"names are exact, so Monday and monday cannot both arrive")
	})

	t.Run("a shift a group uses cannot be deactivated", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(day, 10), map[string]any{"is_active": false}),
			http.StatusConflict, "CONFLICT", organization.ReasonShiftInUse)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(day, 10), map[string]any{"is_active": true}).Status,
			"already active: nothing to refuse")
	})

	t.Run("a change of some days leaves the others", func(t *testing.T) {
		res := e.api(admin, http.MethodPatch, "/admin/shift-groups/"+strconv.FormatInt(group, 10), map[string]any{"name": "Malam Jumat", "days": map[string]any{"friday": night}})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		days := data[row](t, res)["days"].([]any)                                             //nolint:forcetypeassert // test
		require.Equal(t, "Night", days[4].(map[string]any)["shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test
		require.Equal(t, "Day", days[3].(map[string]any)["shift"].(map[string]any)["name"])   //nolint:forcetypeassert // test
		require.Equal(t, "Malam Jumat", data[row](t, res).str("name"))
	})

	t.Run("names are unique", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodPost, "/admin/shift-groups", map[string]any{"name": "malam jumat", "days": map[string]any{
			"monday": day, "tuesday": day, "wednesday": day, "thursday": day, "friday": day, "saturday": off, "sunday": off}}),
			http.StatusConflict, "DUPLICATE", organization.ReasonShiftGroupNameTaken)
	})

	t.Run("a shift a group uses cannot be deleted; an unused one can, and frees its name", func(t *testing.T) {
		expect(t, e.api(admin, http.MethodDelete, "/admin/shifts/"+strconv.FormatInt(day, 10), nil), http.StatusConflict, "CONFLICT", organization.ReasonShiftInUse)
		spare := e.shift(admin, "Spare", "10:00", "18:00")
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/shifts/"+strconv.FormatInt(spare, 10), nil).Status)
		e.shift(admin, "Spare", "10:00", "18:00")
	})
}

func TestAFixedAssignmentGeneratesTheScheduleLikePJ48(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)
	weekdays := e.weekGroup(admin, "5 Hari", day, off)
	nights := e.weekGroup(admin, "Malam", night, off)

	// A past date entered by hand, which no generation may touch.
	require.Equal(t, http.StatusCreated, e.manual(admin, budi, entry("2026-03-05", night)).Status)

	t.Run("FIXED fills today to today + 60 from the weekly pattern", func(t *testing.T) {
		res := e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": weekdays, "effective_date": "2026-03-01"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[row](t, res)
		require.Equal(t, "2026-03-10", got.str("from"), "today in Jakarta; the past is not generated")
		require.EqualValues(t, 61, got["generated"])
		require.EqualValues(t, 0, got["deleted"])
		assignment := got["assignment"].(map[string]any)              //nolint:forcetypeassert // test
		require.Equal(t, "2026-05-09", assignment["generated_until"]) // 2026-03-10 + 60

		rows := e.scheduleOf(org, budi)
		require.Equal(t, "2026-03-05 Night manual", rows[0], "the past stays")
		require.Equal(t, "2026-03-10 Day group", rows[1], "Tuesday")
		require.Equal(t, "2026-03-14 OFF group", rows[5], "Saturday")
		require.Equal(t, "2026-03-15 OFF group", rows[6], "Sunday")
		require.Equal(t, "2026-03-16 Day group", rows[7], "Monday")
		require.Equal(t, "2026-05-09 OFF group", rows[len(rows)-1], "Saturday, the last day of the window")
	})

	t.Run("a new group replaces from tomorrow when today's shift has already started", func(t *testing.T) {
		// 12:00 in Jakarta: today's 08:00 Day shift is under way, so today is locked.
		res := e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": nights, "effective_date": "2026-03-01"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[row](t, res)
		require.Equal(t, "2026-03-11", got.str("from"))
		require.EqualValues(t, 60, got["deleted"])
		require.EqualValues(t, 60, got["generated"])
		rows := e.scheduleOf(org, budi)
		require.Equal(t, "2026-03-10 Day group", rows[1], "today keeps its shift")
		require.Equal(t, "2026-03-11 Night group", rows[2])
	})

	t.Run("a future effective date keeps the dates before it", func(t *testing.T) {
		res := e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": weekdays, "effective_date": "2026-04-01"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		rows := e.scheduleOf(org, budi)
		// The old effective date (03-01) was earlier, so its rows from tomorrow on are replaced;
		// before 04-01 nothing is generated.
		require.Contains(t, rows, "2026-03-10 Day group")
		require.NotContains(t, rows, "2026-03-11 Night group")
		require.Contains(t, rows, "2026-04-01 Day group", "Wednesday")
		require.Equal(t, "2026-04-01 Day group", rows[2])
	})

	t.Run("FLEXIBLE generates and deletes nothing", func(t *testing.T) {
		before := e.scheduleCount(org, budi)
		res := e.assign(admin, budi, map[string]any{"schedule_type": "FLEXIBLE", "effective_date": "2026-03-01"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.EqualValues(t, 0, data[row](t, res)["generated"])
		require.Equal(t, before, e.scheduleCount(org, budi))
		got := data[row](t, e.api(admin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/shift-assignment", nil))
		require.Equal(t, "FLEXIBLE", got.str("schedule_type"))
		require.Nil(t, got["shift_group_id"])
		require.Nil(t, got["generated_until"])
	})

	t.Run("a group in use cannot be deleted", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": weekdays, "effective_date": "2026-03-01"}).Status)
		expect(t, e.api(admin, http.MethodDelete, "/admin/shift-groups/"+strconv.FormatInt(weekdays, 10), nil),
			http.StatusConflict, "CONFLICT", organization.ReasonShiftGroupInUse)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/shift-groups/"+strconv.FormatInt(nights, 10), nil).Status, "nobody uses it")
		expect(t, e.api(admin, http.MethodDelete, "/admin/shifts/"+strconv.FormatInt(day, 10), nil),
			http.StatusConflict, "CONFLICT", organization.ReasonShiftInUse)
	})

	t.Run("the request is validated", func(t *testing.T) {
		missing := e.raw(admin, http.MethodPut, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/shift-assignment", map[string]any{"schedule_type": "FIXED", "effective_date": "2026-03-01"})
		require.Equal(t, http.StatusUnprocessableEntity, missing.Status)
		require.Contains(t, string(missing.Raw), `"shift_group_id"`)
		extra := e.raw(admin, http.MethodPut, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/shift-assignment", map[string]any{"schedule_type": "FLEXIBLE", "shift_group_id": weekdays, "effective_date": "2026-03-01"})
		require.Equal(t, http.StatusUnprocessableEntity, extra.Status)
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPut, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/shift-assignment", map[string]any{"schedule_type": "FIXED", "shift_group_id": weekdays}).Status, "no date")
		expect(t, e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": 999999, "effective_date": "2026-03-01"}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonShiftGroupNotFound)
		expect(t, e.assign(admin, 999999, map[string]any{"schedule_type": "FLEXIBLE", "effective_date": "2026-03-01"}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
		nobody := e.personNIK(org, "N-NEW", "Baru", "ACTIVE")
		expect(t, e.api(admin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(nobody, 10)+"/shift-assignment", nil),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonShiftAssignmentNotFound)
	})

	t.Run("it is audited", func(t *testing.T) {
		var metadata string
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"SELECT metadata::text FROM audit_logs WHERE action = 'shift_assignment.set' ORDER BY id LIMIT 1").Scan(&metadata))
		})
		require.Contains(t, metadata, `"generated": 61`)
		require.Contains(t, metadata, `"schedule_type": "FIXED"`)
	})
}

func TestTheNightlyExtensionKeepsTheWindowAndManualEntries(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)
	group := e.weekGroup(admin, "5 Hari", day, off)
	require.Equal(t, http.StatusOK, e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-03-01"}).Status)

	// An admin's own entry just past the window: the extension must not overwrite it.
	require.Equal(t, http.StatusCreated, e.manual(admin, budi, entry("2026-05-10", night)).Status)

	tomorrow := organization.NewScheduleExtender(testkit.Pool(t), clock.Fixed{T: now.Add(24 * time.Hour)})
	made, err := tomorrow.Extend(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, made, "05-11 is new; 05-10 already had the manual entry")
	rows := e.scheduleOf(org, budi)
	require.Contains(t, rows, "2026-05-10 Night manual")
	require.Equal(t, "2026-05-11 Day group", rows[len(rows)-1], "Monday")

	again, err := tomorrow.Extend(t.Context())
	require.NoError(t, err)
	require.Zero(t, again, "a second run the same day does nothing")

	got := data[row](t, e.api(admin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/shift-assignment", nil))
	require.Equal(t, "2026-05-11", got.str("generated_until"))
}

func TestManualSchedulesLikePJ48(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S2", "N-CICI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")

	t.Run("several dates at once", func(t *testing.T) {
		res := e.manual(admin, budi, entry("2026-03-20", day), entry("2026-03-21", night))
		require.Equal(t, http.StatusCreated, res.Status, "body: %s", res.Raw)
		created := data[[]row](t, res)
		require.Len(t, created, 2)
		require.Equal(t, "2026-03-20", created[0].str("work_date"))
		require.Nil(t, created[0]["shift_group_id"], "entered by hand")
		require.Equal(t, true, created[1]["shift"].(map[string]any)["overnight"]) //nolint:forcetypeassert // test
		require.Equal(t, "**BUDI", created[0].str("nik"), "the NIK is masked")
	})

	t.Run("a date that already has one is refused, and nothing is saved", func(t *testing.T) {
		expect(t, e.manual(admin, budi, entry("2026-03-22", day), entry("2026-03-20", night)),
			http.StatusConflict, "CONFLICT", organization.ReasonScheduleExists)
		require.Len(t, e.scheduleOf(org, budi), 2, "03-22 was not saved either")
	})

	t.Run("bad entries", func(t *testing.T) {
		twice := e.raw(admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/schedules",
			map[string]any{"entries": []map[string]any{entry("2026-04-01", day), entry("2026-04-01", night)}})
		require.Equal(t, http.StatusUnprocessableEntity, twice.Status)
		require.Contains(t, string(twice.Raw), `entries[1].work_date`)
		unknown := e.manual(admin, budi, entry("2026-04-01", 999999))
		require.Equal(t, http.StatusUnprocessableEntity, unknown.Status)
		require.Contains(t, string(unknown.Raw), `entries[0].shift_id`)
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/schedules", map[string]any{"entries": []any{}}).Status)
		var many []map[string]any
		for i := range 63 {
			many = append(many, entry(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.DateOnly), day))
		}
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPost, "/admin/personnel/"+strconv.FormatInt(budi, 10)+"/schedules", map[string]any{"entries": many}).Status)
		expect(t, e.manual(admin, 999999, entry("2026-04-01", day)), http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
	})

	var first int64
	t.Run("the list needs a range of at most 62 days and filters", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.manual(admin, cici, entry("2026-03-20", night)).Status)
		list := e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-03-31", nil)
		require.Equal(t, http.StatusOK, list.Status, "body: %s", list.Raw)
		rows := data[[]row](t, list)
		require.Len(t, rows, 3)
		require.Equal(t, "Budi", rows[0].str("full_name"), "by date, then name")
		require.Equal(t, "Cici", rows[1].str("full_name"))
		first = id(rows[0])

		require.Len(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-03-31&team_code=1K-IBMI-S2", nil)), 1)
		require.Len(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-03-31&personnel_id="+strconv.FormatInt(budi, 10), nil)), 2)
		require.Len(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-03-31&q=cic", nil)), 1)
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/schedules", nil).Status, "no range")
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-06-01", nil).Status, "too long")
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/schedules?date_from=2026-03-10&date_to=2026-03-01", nil).Status, "backwards")
	})

	t.Run("a change moves the date or the shift; an occupied date is refused", func(t *testing.T) {
		path := "/admin/schedules/" + strconv.FormatInt(first, 10)
		expect(t, e.api(admin, http.MethodPatch, path, map[string]any{"work_date": "2026-03-21"}), http.StatusConflict, "CONFLICT", organization.ReasonScheduleExists)
		res := e.api(admin, http.MethodPatch, path, map[string]any{"work_date": "2026-03-25", "shift_id": night})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Equal(t, "2026-03-25", data[row](t, res).str("work_date"))
		require.Equal(t, "Night", data[row](t, res)["shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test
		expect(t, e.api(admin, http.MethodPatch, "/admin/schedules/999999", map[string]any{"shift_id": day}), http.StatusNotFound, "NOT_FOUND", organization.ReasonScheduleNotFound)
		bad := e.api(admin, http.MethodPatch, path, map[string]any{"shift_id": 999999})
		require.Equal(t, http.StatusUnprocessableEntity, bad.Status)
		require.Contains(t, string(bad.Raw), `"shift_id"`)
	})

	t.Run("bulk delete is all or nothing", func(t *testing.T) {
		before := e.scheduleCount(org, budi)
		expect(t, e.api(admin, http.MethodPost, "/admin/schedules/bulk-delete", map[string]any{"ids": []int64{first, 999999}}),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonScheduleNotFound)
		require.Equal(t, before, e.scheduleCount(org, budi))
		res := e.api(admin, http.MethodPost, "/admin/schedules/bulk-delete", map[string]any{"ids": []int64{first, first}})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.EqualValues(t, 1, data[row](t, res)["deleted"], "a repeated id counts once")
		expect(t, e.api(admin, http.MethodDelete, "/admin/schedules/"+strconv.FormatInt(first, 10), nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonScheduleNotFound)
	})
}

func TestARegionAdminSchedulesOnlyTheirPeople(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.tree(admin, 2)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "2K-IBMI-S").Status)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "2K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	off := e.offShift(admin)
	group := e.weekGroup(admin, "5 Hari", day, off)
	ciciRow := id(data[[]row](t, e.manual(admin, cici, entry("2026-03-20", day)))[0])
	regionAdmin := e.person(org, "REGION_ADMIN", "OS1")
	deny := func(res reply) {
		t.Helper()
		expect(t, res, http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
	}

	t.Run("their own people", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.manual(regionAdmin, budi, entry("2026-03-20", day)).Status)
		require.Equal(t, http.StatusOK, e.assign(regionAdmin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-03-01"}).Status)
		rows := data[[]row](t, e.raw(regionAdmin, http.MethodGet, "/admin/schedules?date_from=2026-03-20&date_to=2026-03-20", nil))
		require.Len(t, rows, 1, "Cici's row is not theirs")
		require.Equal(t, "Budi", rows[0].str("full_name"))
	})

	t.Run("people of another region are SCOPE_DENIED", func(t *testing.T) {
		deny(e.manual(regionAdmin, cici, entry("2026-03-21", day)))
		deny(e.assign(regionAdmin, cici, map[string]any{"schedule_type": "FLEXIBLE", "effective_date": "2026-03-01"}))
		deny(e.api(regionAdmin, http.MethodGet, "/admin/personnel/"+strconv.FormatInt(cici, 10)+"/shift-assignment", nil))
		deny(e.api(regionAdmin, http.MethodGet, "/admin/schedules/"+strconv.FormatInt(ciciRow, 10), nil))
		deny(e.api(regionAdmin, http.MethodPatch, "/admin/schedules/"+strconv.FormatInt(ciciRow, 10), map[string]any{"shift_id": off}))
		deny(e.api(regionAdmin, http.MethodDelete, "/admin/schedules/"+strconv.FormatInt(ciciRow, 10), nil))
		deny(e.api(regionAdmin, http.MethodPost, "/admin/schedules/bulk-delete", map[string]any{"ids": []int64{ciciRow}}))
		deny(e.raw(regionAdmin, http.MethodGet, "/admin/schedules?date_from=2026-03-01&date_to=2026-03-31&team_code=2K-IBMI-S1", nil))
		require.Equal(t, []string{"2026-03-20 Day manual"}, e.scheduleOf(org, cici), "nothing changed")
	})

	t.Run("a date on which their person is not in their region is refused", func(t *testing.T) {
		expect(t, e.manual(regionAdmin, budi, entry("2025-12-31", day)), http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
	})
}

func TestTwoTimezonesOnOneDayAreRefused(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.mustCreate(admin, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "PAPUA", "description": "Klien Papua"})
	e.mustCreate(admin, "/admin/projects", map[string]any{"client_code": "1K-PAPUA", "business_line": "security", "description": "Papua", "timezone": "Asia/Jayapura"})
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-PAPUA-S").Status)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-PAPUA-S1", "N-BUDI", "MEMBER", "2026-04-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	off := e.offShift(admin)
	group := e.weekGroup(admin, "5 Hari", day, off)

	require.Equal(t, http.StatusCreated, e.manual(admin, budi, entry("2026-03-20", day)).Status, "only Jakarta that day")
	expect(t, e.manual(admin, budi, entry("2026-04-02", day)), http.StatusUnprocessableEntity, "UNPROCESSABLE", organization.ReasonScheduleTimezoneAmbiguous)

	// Today (03-10) is unambiguous, but 04-01 on lies inside the window FIXED would generate.
	before := e.scheduleCount(org, budi)
	ambiguous := e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-03-01"})
	expect(t, ambiguous, http.StatusUnprocessableEntity, "UNPROCESSABLE", organization.ReasonScheduleTimezoneAmbiguous)
	require.Contains(t, string(ambiguous.Raw), "2026-04-01", "the first such date is named")
	require.Equal(t, before, e.scheduleCount(org, budi), "nothing was generated")

	// A membership added after the schedule was generated can still make a day ambiguous, so
	// the instances check it too.
	require.Equal(t, http.StatusOK, e.assign(admin, cici, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-03-01"}).Status)
	e.mustAdd(admin, "1K-PAPUA-S1", "N-CICI", "MEMBER", "2026-04-01")
	shifts := organization.NewShifts(testkit.Pool(t))
	_, err := shifts.Instances(t.Context(), org, cici, dateOf("2026-04-01"), dateOf("2026-04-03"))
	require.ErrorIs(t, err, organization.ErrScheduleTimezoneAmbiguous)
}

func TestScheduleEdgeCases(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	spare := e.shift(admin, "Spare", "10:00", "18:00")
	off := e.offShift(admin)
	group := e.weekGroup(admin, "5 Hari", day, off)
	require.Equal(t, http.StatusOK, e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-03-01"}).Status)
	rowOn := func(date string) int64 {
		t.Helper()
		rows := data[[]row](t, e.raw(admin, http.MethodGet, "/admin/schedules?date_from="+date+"&date_to="+date, nil))
		require.Len(t, rows, 1)
		return id(rows[0])
	}

	t.Run("a change that changes nothing keeps the row the group's", func(t *testing.T) {
		path := "/admin/schedules/" + strconv.FormatInt(rowOn("2026-03-16"), 10)
		for _, body := range []map[string]any{{}, {"shift_id": day}, {"work_date": "2026-03-16"}} {
			res := e.api(admin, http.MethodPatch, path, body)
			require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
			require.NotNil(t, data[row](t, res)["shift_group_id"])
		}
		require.Contains(t, e.scheduleOf(org, budi), "2026-03-16 Day group")
	})

	t.Run("a date moves with a shift deactivated since; a new shift must be active", func(t *testing.T) {
		manual := id(data[[]row](t, e.manual(admin, budi, entry("2026-06-01", spare)))[0])
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(spare, 10), map[string]any{"is_active": false}).Status,
			"no group uses it")
		res := e.api(admin, http.MethodPatch, "/admin/schedules/"+strconv.FormatInt(manual, 10), map[string]any{"work_date": "2026-06-02"})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		bad := e.api(admin, http.MethodPatch, "/admin/schedules/"+strconv.FormatInt(rowOn("2026-03-17"), 10), map[string]any{"shift_id": spare})
		require.Equal(t, http.StatusUnprocessableEntity, bad.Status)
		require.Contains(t, string(bad.Raw), "tidak aktif")
	})

	t.Run("someone who has left gets no new assignment or schedule", func(t *testing.T) {
		gone := e.personNIK(org, "N-GONE", "Gone", "INACTIVE")
		expect(t, e.assign(admin, gone, map[string]any{"schedule_type": "FLEXIBLE", "effective_date": "2026-03-01"}),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
		expect(t, e.manual(admin, gone, entry("2026-03-20", day)), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
		require.Zero(t, e.scheduleCount(org, gone))
	})

	t.Run("a personnel id that does not exist is said so, not as a NIK", func(t *testing.T) {
		res := e.assign(admin, 999999, map[string]any{"schedule_type": "FLEXIBLE", "effective_date": "2026-03-01"})
		expect(t, res, http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
		require.Contains(t, string(res.Raw), "Personil tidak ditemukan.")
	})

	t.Run("the nightly extension skips someone who has left", func(t *testing.T) {
		require.NoError(t, execInTenant(t, org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = "+strconv.FormatInt(budi, 10)))
		made, err := organization.NewScheduleExtender(testkit.Pool(t), clock.Fixed{T: now.Add(24 * time.Hour)}).Extend(t.Context())
		require.NoError(t, err)
		require.Zero(t, made)
	})
}

func TestShiftInstancesUseTheProjectsTimezone(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)
	e.mustCreate(admin, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "PAPUA", "description": "Klien Papua"})
	e.mustCreate(admin, "/admin/projects", map[string]any{"client_code": "1K-PAPUA", "business_line": "security", "description": "Papua", "timezone": "Asia/Jayapura"})
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-PAPUA-S").Status)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status)
	papua := e.personNIK(org, "N-PAPUA", "Papua", "ACTIVE")
	jakarta := e.personNIK(org, "N-JKT", "Jakarta", "ACTIVE")
	loner := e.personNIK(org, "N-LONE", "Lone", "ACTIVE")
	e.mustAdd(admin, "1K-PAPUA-S1", "N-PAPUA", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-JKT", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)
	require.Equal(t, http.StatusCreated, e.manual(admin, papua, entry("2026-03-10", night), entry("2026-03-11", off)).Status)
	require.Equal(t, http.StatusCreated, e.manual(admin, jakarta, entry("2026-03-10", day)).Status)
	require.Equal(t, http.StatusCreated, e.manual(admin, loner, entry("2026-03-10", day)).Status)
	shifts := organization.NewShifts(testkit.Pool(t))

	t.Run("an overnight shift in WIT ends the next morning", func(t *testing.T) {
		got, err := shifts.Instances(t.Context(), org, papua, dateOf("2026-03-10"), dateOf("2026-03-11"))
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "Asia/Jayapura", got[0].Location.String())
		require.Equal(t, []string{"1K-PAPUA-S"}, got[0].ProjectCodes)
		require.Equal(t, time.Date(2026, 3, 10, 13, 0, 0, 0, time.UTC), got[0].StartsAt, "22:00 WIT")
		require.Equal(t, time.Date(2026, 3, 10, 21, 0, 0, 0, time.UTC), got[0].EndsAt, "06:00 WIT on 03-11")
		require.True(t, got[1].IsOff)
		require.True(t, got[1].StartsAt.IsZero(), "an OFF day has no times")
	})

	t.Run("Current finds a shift from 2 hours before it to 4 hours after it", func(t *testing.T) {
		at := func(h, m int, d int) time.Time { return time.Date(2026, 3, d, h, m, 0, 0, time.UTC) }
		for _, tc := range []struct {
			at   time.Time
			want bool
		}{
			{at(10, 59, 10), false}, // 19:59 WIT, before 20:00
			{at(11, 0, 10), true},   // 20:00 WIT, two hours early
			{at(16, 0, 10), true},   // 01:00 WIT, after midnight, mid-shift
			{at(1, 0, 11), true},    // 10:00 WIT on 03-11, four hours after 06:00
			{at(1, 1, 11), false},   // too late
		} {
			got, err := shifts.Current(t.Context(), org, papua, tc.at)
			require.NoError(t, err)
			require.Equal(t, tc.want, len(got) == 1, tc.at.String())
		}
	})

	t.Run("a day shift in WIB", func(t *testing.T) {
		got, err := shifts.Current(t.Context(), org, jakarta, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)) // 07:00 WIB
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, time.Date(2026, 3, 10, 1, 0, 0, 0, time.UTC), got[0].StartsAt, "08:00 WIB")
	})

	t.Run("someone in no regu is in WIB without a site", func(t *testing.T) {
		got, err := shifts.Instances(t.Context(), org, loner, dateOf("2026-03-10"), dateOf("2026-03-10"))
		require.NoError(t, err)
		require.Equal(t, "Asia/Jakarta", got[0].Location.String())
		require.Empty(t, got[0].ProjectCodes)
	})
}

func TestTheShiftTablesKeepTheirRules(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	day := e.shift(admin, "Day", "08:00", "17:00")
	require.Equal(t, http.StatusCreated, e.manual(admin, budi, entry("2026-03-20", day)).Status)
	p, d := strconv.FormatInt(budi, 10), strconv.FormatInt(day, 10)
	for name, sql := range map[string]string{
		"an OFF day with times":        "INSERT INTO shifts (name, start_minute, end_minute, is_off) VALUES ('X', 480, 1020, true)",
		"a working shift without time": "INSERT INTO shifts (name, start_minute, end_minute) VALUES ('Y', 480, 480)",
		"a minute past midnight":       "INSERT INTO shifts (name, start_minute, end_minute) VALUES ('Z', 1440, 60)",
		"two schedules on one date":    "INSERT INTO schedules (personnel_id, work_date, shift_id) VALUES (" + p + ", '2026-03-20', " + d + ")",
		"FIXED without a group":        "INSERT INTO personnel_shift_assignments (personnel_id, schedule_type, effective_date) VALUES (" + p + ", 'FIXED', '2026-03-01')",
		"a weekday 8":                  "INSERT INTO shift_groups (name) VALUES ('W'); INSERT INTO shift_group_days (shift_group_id, weekday, shift_id) SELECT max(id), 8, " + d + " FROM shift_groups",
	} {
		require.Error(t, execInTenant(t, org, sql), name)
	}
}

func TestTheSeedGivesPJ48sStartingShiftsOnce(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	pool := testkit.Pool(t)

	res, err := organization.SeedDefaultShifts(t.Context(), pool, clock.Fixed{T: now}, org)
	require.NoError(t, err)
	require.Equal(t, []string{"OFF", "Day Shift", "Night Shift"}, res.ShiftsCreated)
	require.True(t, res.GroupCreated)

	shifts := data[[]row](t, e.api(admin, http.MethodGet, "/admin/shifts", nil))
	byName := map[string]row{}
	for _, s := range shifts {
		byName[s.str("name")] = s
	}
	require.Equal(t, true, byName["OFF"]["is_off"])
	require.EqualValues(t, 0, byName["OFF"]["tolerance_minutes"])
	require.Equal(t, "08:00", byName["Day Shift"].str("start_time"))
	require.Equal(t, "17:00", byName["Day Shift"].str("end_time"))
	require.Equal(t, true, byName["Night Shift"]["overnight"])

	groups := data[[]row](t, e.api(admin, http.MethodGet, "/admin/shift-groups", nil))
	require.Len(t, groups, 1)
	days := groups[0]["days"].([]any)                                                         //nolint:forcetypeassert // test
	require.Equal(t, "Day Shift", days[0].(map[string]any)["shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test
	require.Equal(t, "OFF", days[5].(map[string]any)["shift"].(map[string]any)["name"])       //nolint:forcetypeassert // test

	t.Run("a second run changes nothing, not even a shift an admin edited", func(t *testing.T) {
		dayID := id(byName["Day Shift"])
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/shifts/"+strconv.FormatInt(dayID, 10), map[string]any{"start_time": "07:00"}).Status)
		again, err := organization.SeedDefaultShifts(t.Context(), pool, clock.Fixed{T: now}, org)
		require.NoError(t, err)
		require.Empty(t, again.ShiftsCreated)
		require.False(t, again.GroupCreated)
		require.Equal(t, "07:00", data[row](t, e.api(admin, http.MethodGet, "/admin/shifts/"+strconv.FormatInt(dayID, 10), nil)).str("start_time"))
	})

	t.Run("an organization nobody has registered is refused", func(t *testing.T) {
		_, err := organization.SeedDefaultShifts(t.Context(), pool, clock.Fixed{T: now}, 987654321)
		require.ErrorContains(t, err, "not registered")
	})

	t.Run("the rows are audited as made by the system", func(t *testing.T) {
		var system int
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"SELECT count(*) FROM audit_logs WHERE actor_personnel_id IS NULL AND action IN ('shift.create', 'shift_group.create')").Scan(&system))
		})
		require.Equal(t, 4, system)
	})
}
