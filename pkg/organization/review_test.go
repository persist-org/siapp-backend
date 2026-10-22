package organization_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/testkit"
)

// Regression tests for the edge cases the pre-PR review found, one function per area.

func TestRequestsThatWouldBreakTheDatabaseAreRefusedFirst(t *testing.T) {
	e := newEnv(t)
	admin := e.person(testkit.NewOrg(t), "ORG_ADMIN", "")
	e.tree(admin, 1)

	t.Run("a page past the limit is 422, not an overflowed OFFSET", func(t *testing.T) {
		res := e.raw(admin, http.MethodGet, "/admin/regions?page=9223372036854775807", nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		require.Contains(t, string(res.Raw), `"page"`)
		require.Equal(t, http.StatusOK, e.raw(admin, http.MethodGet, "/admin/regions?page=1000000", nil).Status)
	})

	t.Run("NUL in a path, a query or a body never reaches Postgres", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, e.raw(admin, http.MethodGet, "/admin/regions/OS1%00", nil).Status)
		require.Equal(t, http.StatusBadRequest, e.raw(admin, http.MethodGet, "/admin/regions?q=%00", nil).Status)
		require.Equal(t, http.StatusBadRequest, e.raw(admin, http.MethodPost, "/admin/regions", map[string]any{"code": "OS9", "description": "a\x00b"}).Status)
	})

	t.Run("a search is literal: % and _ are not wildcards", func(t *testing.T) {
		require.Empty(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/branches?q=%25", nil)))
		require.Empty(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/branches?q=_", nil)))
		require.Len(t, data[[]row](t, e.raw(admin, http.MethodGet, "/admin/branches?q=cabang", nil)), 1, "case-insensitive substring")
	})
}

func TestRegionEdgeCases(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)

	t.Run("a region code longer than 10 is refused", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPost, "/admin/regions", map[string]any{"code": "OS123456789", "description": "x"}).Status)
	})

	t.Run("an empty change changes nothing and is not audited", func(t *testing.T) {
		before := len(e.auditRows(org))
		res := e.api(admin, http.MethodPatch, "/admin/regions/OS1", map[string]any{})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Len(t, e.auditRows(org), before)
	})
}

func TestHierarchyEdgeCases(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status)
	officer := e.personNIK(org, "N-RO", "Officer", "ACTIVE")
	gone := e.personNIK(org, "N-GONE", "Gone", "INACTIVE")

	t.Run("an empty company prefix is CLIENT_PREFIX_INVALID, not a 500", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity,
			e.raw(admin, http.MethodPost, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "", "description": "x"}).Status)
		expect(t, e.raw(admin, http.MethodPost, "/admin/projects", map[string]any{
			"client_code": "1K-IBMI", "business_line": "cleaning", "description": "x", "timezone": "Asia/Jakarta", "company_prefix": "",
		}), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonClientPrefixInvalid)
	})

	t.Run("a radius or patrol interval past its bound is refused, not clamped", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPatch, "/admin/projects/1K-IBMI-S", map[string]any{"attendance_radius_meters": 20000000}).Status)
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPatch, "/admin/projects/1K-IBMI-S", map[string]any{"patrol_interval_minutes": 1441}).Status)
	})

	t.Run("a relationship officer must be active, and can be removed with null", func(t *testing.T) {
		path := "/admin/teams/1K-IBMI-S1"
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodPatch, path, map[string]any{"relationship_officer_personnel_id": gone}).Status)
		res := e.api(admin, http.MethodPatch, path, map[string]any{"relationship_officer_personnel_id": officer})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.EqualValues(t, officer, data[row](t, res)["relationship_officer_personnel_id"])

		require.NoError(t, execInTenant(t, org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = "+strconv.FormatInt(officer, 10)))
		res = e.api(admin, http.MethodPatch, path, map[string]any{"description": "Regu A"})
		require.Equal(t, http.StatusOK, res.Status, "an officer who left since does not block other changes: %s", res.Raw)
		require.EqualValues(t, officer, data[row](t, res)["relationship_officer_personnel_id"], "a change of something else keeps the officer")

		res = e.raw(admin, http.MethodPatch, path, map[string]any{"relationship_officer_personnel_id": nil})
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		require.Nil(t, data[row](t, res)["relationship_officer_personnel_id"])
	})
}

func TestMembershipEdgeCases(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin) // 1K-IBMI-S1, S2; today is 2026-03-10
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	dodi := e.personNIK(org, "N-DODI", "Dodi", "ACTIVE")

	t.Run("leaving cancels a role change planned for later", func(t *testing.T) {
		m := e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
		require.Equal(t, http.StatusOK, e.changeRole(admin, id(m), "DANRU", "2026-04-01").Status)
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", budi).Status)
		require.Equal(t, []string{"1K-IBMI-S1 MEMBER 2026-01-01 2026-03-10"}, e.historyOf(org, budi),
			"ended today, not on 03-31, and the planned DANRU row is gone")
	})

	t.Run("leaving the old regu of a planned move works, and cancels the move", func(t *testing.T) {
		m := e.mustAdd(admin, "1K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")
		require.Equal(t, http.StatusOK, e.move(admin, id(m), "1K-IBMI-S2", "2026-04-01").Status)
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", cici).Status)
		require.Equal(t, []string{"1K-IBMI-S1 MEMBER 2026-01-01 2026-03-10", "1K-IBMI-S2 MEMBER 2026-04-01 -"}, e.historyOf(org, cici),
			"the S2 row is another regu's: leaving S1 does not touch it")
	})

	t.Run("deleting a regu ends a membership that was planned to end later", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status) // S3
		m := e.mustAdd(admin, "1K-IBMI-S3", "N-DODI", "MEMBER", "2026-01-01")
		require.Equal(t, http.StatusOK, e.changeRole(admin, id(m), "DANRU", "2026-04-01").Status)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/teams/1K-IBMI-S3", nil).Status)
		require.Equal(t, []string{"1K-IBMI-S3 MEMBER 2026-01-01 2026-03-10"}, e.historyOf(org, dodi))
	})

	t.Run("someone who has left cannot be moved or given another role", func(t *testing.T) {
		eka := e.personNIK(org, "N-EKA", "Eka", "ACTIVE")
		m := e.mustAdd(admin, "1K-IBMI-S2", "N-EKA", "MEMBER", "2026-01-01")
		require.NoError(t, execInTenant(t, org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = "+strconv.FormatInt(eka, 10)))
		expect(t, e.move(admin, id(m), "1K-IBMI-S1", "2026-04-01"), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
		expect(t, e.changeRole(admin, id(m), "DANRU", "2026-04-01"), http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S2", eka).Status, "but can still be taken out")
	})
}

func TestScheduleReviewEdgeCases(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.papua(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)
	group := e.weekGroup(admin, "5 Hari", day, off)

	t.Run("the nightly extension never fills the past of a window left behind", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.assign(admin, budi, map[string]any{"schedule_type": "FIXED", "shift_group_id": group, "effective_date": "2026-01-01"}).Status)
		require.NoError(t, execInTenant(t, org, "UPDATE personnel_shift_assignments SET generated_until = '2026-02-01' WHERE personnel_id = "+strconv.FormatInt(budi, 10)))
		_, err := organization.NewScheduleExtender(testkit.Pool(t), clock.Fixed{T: now}).Extend(t.Context())
		require.NoError(t, err)
		var past int
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT count(*) FROM schedules WHERE personnel_id = $1 AND work_date <= '2026-03-10'", budi).Scan(&past))
		})
		require.Zero(t, past, "02-02 .. today stay as they were (empty)")
	})

	t.Run("a schedule on a deleted shift does not move to today or later", func(t *testing.T) {
		old := e.shift(admin, "Old", "06:00", "14:00")
		cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S2", "N-CICI", "MEMBER", "2026-01-01")
		past := id(data[[]row](t, e.manual(admin, cici, entry("2026-03-01", old)))[0])
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/shifts/"+strconv.FormatInt(old, 10), nil).Status, "only a past date uses it")
		path := "/admin/schedules/" + strconv.FormatInt(past, 10)
		bad := e.api(admin, http.MethodPatch, path, map[string]any{"work_date": "2026-03-20"})
		require.Equal(t, http.StatusUnprocessableEntity, bad.Status, "body: %s", bad.Raw)
		require.Contains(t, string(bad.Raw), `"shift_id"`)
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, path, map[string]any{"work_date": "2026-03-02"}).Status, "within the past it may")
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, path, map[string]any{"work_date": "2026-03-20", "shift_id": day}).Status, "with another shift it may")
	})

	t.Run("Current fails on an ambiguous day only when nothing else is current", func(t *testing.T) {
		dodi := e.personNIK(org, "N-DODI", "Dodi", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S2", "N-DODI", "MEMBER", "2026-01-01")
		require.Equal(t, http.StatusCreated, e.manual(admin, dodi, entry("2026-03-10", day), entry("2026-03-11", night)).Status)
		e.mustAdd(admin, "1K-PAPUA-S1", "N-DODI", "MEMBER", "2026-03-11") // 03-11 is now in WIB and WIT
		shifts := organization.NewShifts(testkit.Pool(t))

		got, err := shifts.Current(t.Context(), org, dodi, time.Date(2026, 3, 10, 2, 0, 0, 0, time.UTC)) // 09:00 WIB
		require.NoError(t, err, "today's Day shift is current; tomorrow's ambiguity does not matter")
		require.Len(t, got, 1)
		_, err = shifts.Current(t.Context(), org, dodi, time.Date(2026, 3, 10, 16, 30, 0, 0, time.UTC)) // 23:30 WIB
		require.ErrorIs(t, err, organization.ErrScheduleTimezoneAmbiguous, "only the ambiguous night could be current now")
		_, err = shifts.Instances(t.Context(), org, dodi, dateOf("2026-03-11"), dateOf("2026-03-11"))
		require.ErrorIs(t, err, organization.ErrScheduleTimezoneAmbiguous)
	})
}

func TestReguScreenEdgeCases(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	_, budiToken := e.field(org, "N-BUDI", "Budi")
	gone, _ := e.field(org, "N-GONE", "Gone")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "DANRU", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-GONE", "MEMBER", "2026-01-01")
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S1", map[string]any{"min_personnel": 2}).Status)
	require.NoError(t, execInTenant(t, org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = "+strconv.FormatInt(gone, 10)))

	t.Run("someone who has left is not listed, counted or shown", func(t *testing.T) {
		detail := data[row](t, e.api(budiToken, http.MethodGet, "/teams/1K-IBMI-S1/members", nil))
		require.Len(t, membersOf(t, detail), 1)
		require.EqualValues(t, 1, detail["member_count"])
		require.Equal(t, true, detail["understaffed"], "1 of the 2 it needs: the one who left does not count")
		expect(t, e.api(budiToken, http.MethodGet, "/personnels/"+strconv.FormatInt(gone, 10), nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
	})

	t.Run("an empty team code is not found, not every team", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, e.raw(budiToken, http.MethodGet, "/teams//members", nil).Status)
	})
}
