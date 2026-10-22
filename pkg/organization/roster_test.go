package organization_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/testkit"
)

// field adds a person with a NIK, a phone and a position, and returns their id and a token.
func (e *env) field(org int64, nik, name string) (int64, string) {
	e.t.Helper()
	id := e.personNIK(org, nik, name, "ACTIVE")
	require.NoError(e.t, execInTenant(e.t, org,
		"UPDATE personnels SET phone = '0812-"+nik+"', position_name = 'Anggota Satpam', photo_url = 'https://img.test/"+nik+"' WHERE id = "+strconv.FormatInt(id, 10)))
	return id, testkit.Token(e.t, org, id)
}

// papua adds the client 1K-PAPUA with project 1K-PAPUA-S (WIT) and its team 1K-PAPUA-S1.
func (e *env) papua(admin string) {
	e.t.Helper()
	e.mustCreate(admin, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "PAPUA", "description": "Klien Papua"})
	e.mustCreate(admin, "/admin/projects", map[string]any{"client_code": "1K-PAPUA", "business_line": "security", "description": "Papua", "timezone": "Asia/Jayapura"})
	require.Equal(e.t, http.StatusCreated, e.newTeam(admin, "1K-PAPUA-S").Status)
}

func membersOf(t *testing.T, detail row) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range detail["members"].([]any) { //nolint:forcetypeassert // test
		out = append(out, m.(map[string]any)) //nolint:forcetypeassert // test
	}
	return out
}

func TestTheReguScreens(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin) // 1K-IBMI-S (S1, S2) and 1K-IBMI-C (C1)
	e.papua(admin)

	budi, budiToken := e.field(org, "N-BUDI", "Budi")
	cici, ciciToken := e.field(org, "N-CICI", "Cici")
	dodi, dodiToken := e.field(org, "N-DODI", "Dodi")
	eka, ekaToken := e.field(org, "N-EKA", "Eka")
	hani, _ := e.field(org, "N-HANI", "Hani")
	_, faniToken := e.field(org, "N-FANI", "Fani")
	_, gitaToken := e.field(org, "N-GITA", "Gita")
	loner, lonerToken := e.field(org, "N-LONE", "Lone")

	e.mustAdd(admin, "1K-IBMI-S2", "N-BUDI", "DANRU", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S2", "N-EKA", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S2", "N-HANI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-PAPUA-S1", "N-EKA", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-C1", "N-DODI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-FANI", "MEMBER", "2026-01-01")
	require.NoError(t, execInTenant(t, org, "UPDATE team_members SET end_date = '2026-03-01' WHERE personnel_id = (SELECT id FROM personnels WHERE nik = 'N-FANI')")) // left

	e.mustAdd(admin, "1K-IBMI-S1", "N-GITA", "MEMBER", "2026-03-20") // not yet

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S2", map[string]any{"min_personnel": 4}).Status)
	require.Equal(t, http.StatusOK, e.api(admin, http.MethodPatch, "/admin/teams/1K-IBMI-S1", map[string]any{"relationship_officer_personnel_id": dodi}).Status)

	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	off := e.offShift(admin)
	require.Equal(t, http.StatusCreated, e.manual(admin, budi, entry("2026-03-10", day)).Status)
	require.Equal(t, http.StatusCreated, e.manual(admin, hani, entry("2026-03-10", night)).Status)
	require.Equal(t, http.StatusCreated, e.manual(admin, cici, entry("2026-03-10", off)).Status)

	t.Run("a person sees the teams of the projects they serve in, and no other", func(t *testing.T) {
		res := e.api(ciciToken, http.MethodGet, "/teams", nil)
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S2"}, codesOf(t, res), "not 1K-IBMI-C1")
		teams := data[[]row](t, res)
		require.Equal(t, true, teams[0]["is_my_team"])
		require.Equal(t, "MEMBER", teams[0].str("my_role"))
		require.Equal(t, false, teams[1]["is_my_team"])
		require.Equal(t, "Budi", teams[1]["danru"].(map[string]any)["full_name"]) //nolint:forcetypeassert // test
		require.EqualValues(t, 3, teams[1]["member_count"])
		require.Equal(t, true, teams[1]["understaffed"], "3 of the 4 it needs")
		require.Equal(t, false, teams[0]["understaffed"])
		require.Equal(t, "2026-03-10", teams[0].str("business_date"))
		require.Equal(t, "Klien 1", teams[0].str("client_name"))

		require.Equal(t, []string{"1K-IBMI-C1"}, codesOf(t, e.api(dodiToken, http.MethodGet, "/teams", nil)))
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S2", "1K-PAPUA-S1"}, codesOf(t, e.api(ekaToken, http.MethodGet, "/teams", nil)),
			"every project they serve in")
	})

	t.Run("a DANRU sees their project and not another", func(t *testing.T) {
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S2"}, codesOf(t, e.raw(budiToken, http.MethodGet, "/teams?project_code=1K-IBMI-S", nil)))
		expect(t, e.raw(budiToken, http.MethodGet, "/teams?project_code=1K-IBMI-C", nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
		expect(t, e.raw(budiToken, http.MethodGet, "/teams?project_code=9K-NONE-S", nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
	})

	t.Run("someone in no regu today sees nothing", func(t *testing.T) {
		for name, token := range map[string]string{"no regu": lonerToken, "an ended membership": faniToken, "a membership that starts later": gitaToken} {
			require.Empty(t, codesOf(t, e.api(token, http.MethodGet, "/teams", nil)), name)
			require.Empty(t, data[[]row](t, e.api(token, http.MethodGet, "/teams/me", nil)), name)
			expect(t, e.api(token, http.MethodGet, "/teams/1K-IBMI-S1/members", nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
		}
	})

	t.Run("the team detail: DANRU first, today's shifts, the relationship officer", func(t *testing.T) {
		res := e.api(ciciToken, http.MethodGet, "/teams/1K-IBMI-S2/members", nil)
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		detail := data[row](t, res)
		members := membersOf(t, detail)
		require.Len(t, members, 3)
		require.Equal(t, "Budi", members[0]["full_name"], "the DANRU first")
		require.Equal(t, "DANRU", members[0]["role"])
		require.Equal(t, "Eka", members[1]["full_name"], "then by name")
		require.Equal(t, "Hani", members[2]["full_name"])
		require.Equal(t, "Anggota Satpam", members[0]["position_name"])
		require.Equal(t, "Day", members[0]["today_shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test
		require.Nil(t, members[1]["today_shift"], "Eka is not scheduled today")
		require.NotContains(t, members[0], "phone", "a member's phone is on their card, not in the list")

		shifts := detail["today_shifts"].([]any) //nolint:forcetypeassert // test
		require.Len(t, shifts, 2)
		require.Equal(t, "Day", shifts[0].(map[string]any)["name"], "by start time") //nolint:forcetypeassert // test
		require.Equal(t, true, shifts[1].(map[string]any)["overnight"])              //nolint:forcetypeassert // test
		require.EqualValues(t, 1, shifts[1].(map[string]any)["personnel_count"])     //nolint:forcetypeassert // test
		require.Equal(t, false, detail["is_my_team"], "Cici is in S1, the same project")

		s1 := data[row](t, e.api(ciciToken, http.MethodGet, "/teams/1K-IBMI-S1/members", nil))
		ro := s1["relationship_officer"].(map[string]any) //nolint:forcetypeassert // test
		require.Equal(t, "Dodi", ro["full_name"])
		require.Equal(t, "0812-N-DODI", ro["phone"])
		require.Len(t, membersOf(t, s1), 1, "Fani has left and Gita starts later")
		require.Equal(t, true, membersOf(t, s1)[0]["today_shift"].(map[string]any)["is_off"]) //nolint:forcetypeassert // test
		require.Empty(t, s1["today_shifts"], "an OFF day is no shift of the regu")
		require.Nil(t, s1["danru"], "S1 has no DANRU")
	})

	t.Run("another project's team is refused; a team that does not exist is not found", func(t *testing.T) {
		expect(t, e.api(dodiToken, http.MethodGet, "/teams/1K-IBMI-S2/members", nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
		expect(t, e.api(dodiToken, http.MethodGet, "/teams/1K-IBMI-S9/members", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	})

	t.Run("my teams lists every regu, in both projects", func(t *testing.T) {
		mine := data[[]row](t, e.api(ekaToken, http.MethodGet, "/teams/me", nil))
		require.Len(t, mine, 2)
		require.Equal(t, "1K-IBMI-S2", mine[0].str("code"))
		require.Equal(t, "1K-PAPUA-S1", mine[1].str("code"))
		for _, m := range mine {
			require.Equal(t, "MEMBER", m.str("my_role"))
			require.Equal(t, true, m["is_my_team"])
			require.NotEmpty(t, m["members"])
		}
	})

	t.Run("a card is shown to someone who shares a project, with the NIK masked", func(t *testing.T) {
		res := e.api(ciciToken, http.MethodGet, "/personnels/"+strconv.FormatInt(budi, 10), nil)
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		card := data[row](t, res)
		require.Equal(t, "Budi", card.str("full_name"))
		require.Equal(t, "**BUDI", card.str("nik"))
		require.Equal(t, "0812-N-BUDI", card.str("phone"))
		require.Equal(t, "Anggota Satpam", card.str("position_name"))
		require.NotContains(t, string(res.Raw), "birth", "no HR profile")

		shared := data[row](t, e.api(ciciToken, http.MethodGet, "/personnels/"+strconv.FormatInt(eka, 10), nil))
		memberships := shared["memberships"].([]any) //nolint:forcetypeassert // test
		require.Len(t, memberships, 1, "only the shared project: Eka's Papua regu is not Cici's business")
		require.Equal(t, "1K-IBMI-S2", memberships[0].(map[string]any)["team_code"]) //nolint:forcetypeassert // test

		own := data[row](t, e.api(ekaToken, http.MethodGet, "/personnels/"+strconv.FormatInt(eka, 10), nil))
		require.Len(t, own["memberships"], 2, "their own card shows every regu")
		require.Equal(t, http.StatusOK, e.api(lonerToken, http.MethodGet, "/personnels/"+strconv.FormatInt(loner, 10), nil).Status,
			"anyone sees their own card")
	})

	t.Run("a card is refused across projects, and not found for nobody", func(t *testing.T) {
		expect(t, e.api(dodiToken, http.MethodGet, "/personnels/"+strconv.FormatInt(budi, 10), nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
		expect(t, e.api(lonerToken, http.MethodGet, "/personnels/"+strconv.FormatInt(cici, 10), nil), http.StatusForbidden, "SCOPE_DENIED", organization.ReasonNotProjectMember)
		expect(t, e.api(ciciToken, http.MethodGet, "/personnels/999999", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
		expect(t, e.raw(ciciToken, http.MethodGet, "/personnels/abc", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
	})

	t.Run("a deleted team is gone from the screens", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/teams/1K-IBMI-C1", nil).Status)
		require.Empty(t, codesOf(t, e.api(dodiToken, http.MethodGet, "/teams", nil)))
		expect(t, e.api(ciciToken, http.MethodGet, "/teams/1K-IBMI-C1/members", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	})
}

// At 16:00 UTC it is 23:00 on 03-10 in Jakarta and already 01:00 on 03-11 in Papua: each
// project's screen shows its own today, and the shift of that day.
func TestTheReguScreensUseEachProjectsOwnToday(t *testing.T) {
	late := time.Date(2026, 3, 10, 16, 0, 0, 0, time.UTC)
	e := newEnvWith(t, localPeople{t}, clock.Fixed{T: late})
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status)
	e.papua(admin)
	jakarta, jakartaToken := e.field(org, "N-JKT", "Jakarta")
	papua, papuaToken := e.field(org, "N-PAPUA", "Papua")
	e.mustAdd(admin, "1K-IBMI-S1", "N-JKT", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-PAPUA-S1", "N-PAPUA", "MEMBER", "2026-03-11") // starts on Papua's today, still tomorrow in Jakarta
	day := e.shift(admin, "Day", "08:00", "17:00")
	night := e.shift(admin, "Night", "22:00", "06:00")
	require.Equal(t, http.StatusCreated, e.manual(admin, jakarta, entry("2026-03-10", day), entry("2026-03-11", night)).Status)
	require.Equal(t, http.StatusCreated, e.manual(admin, papua, entry("2026-03-10", night), entry("2026-03-11", day)).Status)

	jkt := data[[]row](t, e.api(jakartaToken, http.MethodGet, "/teams/me", nil))
	require.Len(t, jkt, 1)
	require.Equal(t, "2026-03-10", jkt[0].str("business_date"))
	require.Equal(t, "Day", membersOf(t, jkt[0])[0]["today_shift"].(map[string]any)["name"]) //nolint:forcetypeassert // test

	pap := data[[]row](t, e.api(papuaToken, http.MethodGet, "/teams/me", nil))
	require.Len(t, pap, 1, "the membership has started in Papua")
	require.Equal(t, "2026-03-11", pap[0].str("business_date"))
	require.Equal(t, "Day", membersOf(t, pap[0])[0]["today_shift"].(map[string]any)["name"], "03-11's shift, not 03-10's night") //nolint:forcetypeassert // test
}
