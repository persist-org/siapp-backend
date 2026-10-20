package organization_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/organization"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel/personeltest"
	"siapp/pkg/platform/testkit"
	"siapp/pkg/platform/timeutil"
)

// now is "now" for these tests: 12:00 on 2026-03-10 in Jakarta (05:00 UTC).
var now = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

func day(s string) timeutil.Date {
	d, err := timeutil.ParseDate(s)
	if err != nil {
		panic(err)
	}
	return d
}

// localPeople finds a person by NIK among the rows already in the organization, the way
// identity.People does once the person is known.
type localPeople struct{ t *testing.T }

func (p localPeople) Ensure(ctx context.Context, org int64, nik string) (int64, error) {
	var id int64
	err := database.WithTenantTx(ctx, testkit.Pool(p.t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM personnels WHERE nik = $1", nik).Scan(&id)
	})
	if err != nil {
		return 0, apperr.ErrNotFound
	}
	return id, nil
}

// newMemberEnv is a clean database served with a fixed clock and the local people lookup.
func newMemberEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, localPeople{t}, clock.Fixed{T: now})
}

// personNIK adds a person with a NIK to an organization and returns their personnel id.
func (e *env) personNIK(org int64, nik, name, status string) int64 {
	e.t.Helper()
	e.userID++
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(),
			"INSERT INTO personnels (personel_user_id, nik, full_name, status) VALUES ($1, $2, $3, $4::personnel_status) RETURNING id",
			e.userID, nik, name, status).Scan(&id))
	})
	return id
}

func (e *env) addMember(token, team, nik, role, start string) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/teams/"+team+"/members", map[string]any{"nik": nik, "role": role, "start_date": start})
}

func (e *env) mustAdd(token, team, nik, role, start string) row {
	e.t.Helper()
	res := e.addMember(token, team, nik, role, start)
	require.Equal(e.t, http.StatusCreated, res.Status, "add %s to %s: %s", nik, team, res.Raw)
	return data[row](e.t, res)
}

func (e *env) move(token string, id int64, to, effective string) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/team-members/"+strconv.FormatInt(id, 10)+"/move",
		map[string]any{"to_team_code": to, "effective_date": effective})
}

func (e *env) changeRole(token string, id int64, role, effective string) reply {
	e.t.Helper()
	return e.api(token, http.MethodPost, "/admin/team-members/"+strconv.FormatInt(id, 10)+"/change-role",
		map[string]any{"role": role, "effective_date": effective})
}

func (e *env) membersOn(token, team, on string) []row {
	e.t.Helper()
	path := "/admin/teams/" + team + "/members"
	res := e.raw(token, http.MethodGet, path+"?on_date="+on, nil)
	require.Equal(e.t, http.StatusOK, res.Status, "body: %s", res.Raw)
	return data[[]row](e.t, res)
}

func (e *env) endMember(token, team string, personnelID int64) reply {
	e.t.Helper()
	return e.api(token, http.MethodDelete, "/admin/teams/"+team+"/members/"+strconv.FormatInt(personnelID, 10), nil)
}

func (e *env) historyOf(org int64, personnelID int64) (rows []string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		r, err := tx.Query(e.t.Context(),
			"SELECT team_code || ' ' || role || ' ' || start_date || ' ' || coalesce(end_date::text, '-') FROM team_members WHERE personnel_id = $1 ORDER BY start_date, id",
			personnelID)
		require.NoError(e.t, err)
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		require.NoError(e.t, err)
	})
	return rows
}

// fixture: OS1 with project 1K-IBMI-S (teams S1, S2) and 1K-IBMI-C (team C1), all in Jakarta.
func (e *env) fixture(admin string) {
	e.t.Helper()
	e.tree(admin, 1)
	e.mustCreate(admin, "/admin/projects", map[string]any{"client_code": "1K-IBMI", "business_line": "cleaning", "description": "Kebersihan", "timezone": "Asia/Jakarta"})
	for _, project := range []string{"1K-IBMI-S", "1K-IBMI-S", "1K-IBMI-C"} {
		require.Equal(e.t, http.StatusCreated, e.newTeam(admin, project).Status)
	}
}

func TestAPersonIsAddedByNIKAndTheResponseMasksIt(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "1234567890", "Budi", "ACTIVE")

	got := e.mustAdd(admin, "1K-IBMI-S1", "1234567890", "MEMBER", "2026-01-01")
	require.Equal(t, "1K-IBMI-S1", got.str("team_code"))
	require.Equal(t, "MEMBER", got.str("role"))
	require.Equal(t, "2026-01-01", got.str("start_date"))
	require.EqualValues(t, budi, got["personnel_id"])
	require.Equal(t, "Budi", got.str("full_name"))
	require.Equal(t, "******7890", got.str("nik"), "only the last four characters show")
	require.Nil(t, got["end_date"], "a membership has no end until it is ended")
	require.Contains(t, got, "id")
}

func TestAMoveKeepsBothRowsAndTheActiveQueryFollows(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	query := organization.NewQuery(testkit.Pool(t))

	first := e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	moved := e.move(admin, int64(first["id"].(float64)), "1K-IBMI-S2", "2026-02-01") //nolint:forcetypeassert // test
	require.Equal(t, http.StatusOK, moved.Status, "body: %s", moved.Raw)
	got := data[row](t, moved)
	require.Equal(t, "1K-IBMI-S2", got.str("team_code"))
	require.Equal(t, "MEMBER", got.str("role"), "the role is carried")
	require.Equal(t, "2026-02-01", got.str("start_date"))

	require.Equal(t, []string{
		"1K-IBMI-S1 MEMBER 2026-01-01 2026-01-31",
		"1K-IBMI-S2 MEMBER 2026-02-01 -",
	}, e.historyOf(org, budi), "history has two rows; the old one ends the day before")

	active := func(on string) []string {
		ms, err := query.ActiveMemberships(t.Context(), org, budi, day(on))
		require.NoError(t, err)
		var codes []string
		for _, m := range ms {
			codes = append(codes, m.TeamCode)
		}
		return codes
	}
	require.Equal(t, []string{"1K-IBMI-S1"}, active("2026-01-15"))
	require.Equal(t, []string{"1K-IBMI-S1"}, active("2026-01-31"), "end_date is inclusive")
	require.Equal(t, []string{"1K-IBMI-S2"}, active("2026-02-01"))
	require.Empty(t, active("2025-12-31"), "before the first start")

	require.Len(t, e.membersOn(admin, "1K-IBMI-S1", "2026-01-15"), 1)
	require.Empty(t, e.membersOn(admin, "1K-IBMI-S1", "2026-02-01"), "S1 no longer has them")
	require.Len(t, e.membersOn(admin, "1K-IBMI-S2", "2026-02-01"), 1)

	t.Run("an ended membership cannot be moved again", func(t *testing.T) {
		expect(t, e.move(admin, int64(first["id"].(float64)), "1K-IBMI-C1", "2026-03-01"), //nolint:forcetypeassert // test
			http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)
	})

	t.Run("the moves are audited by membership number", func(t *testing.T) {
		require.Equal(t, []string{
			"region.create region OS1", "branch.create branch 1K", "client.create client 1K-IBMI", "project.create project 1K-IBMI-S",
			"project.create project 1K-IBMI-C", "team.create team 1K-IBMI-S1", "team.create team 1K-IBMI-S2", "team.create team 1K-IBMI-C1",
			"team_member.add team_member ", "team_member.move team_member ",
		}, e.auditRows(org))
	})
}

func TestAPersonCanServeInSeveralTeamsButNotTwiceInOne(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	query := organization.NewQuery(testkit.Pool(t))

	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")

	t.Run("the same team on overlapping dates is MEMBERSHIP_OVERLAP", func(t *testing.T) {
		for _, start := range []string{"2026-01-01", "2026-03-01", "2025-06-01"} {
			expect(t, e.addMember(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", start),
				http.StatusConflict, "CONFLICT", organization.ReasonMembershipOverlap)
		}
	})

	t.Run("another team, also in another project, is allowed", func(t *testing.T) {
		e.mustAdd(admin, "1K-IBMI-S2", "N-BUDI", "MEMBER", "2026-01-01")
		e.mustAdd(admin, "1K-IBMI-C1", "N-BUDI", "MEMBER", "2026-01-01")
		ms, err := query.ActiveMemberships(t.Context(), org, budi, day("2026-02-01"))
		require.NoError(t, err)
		var codes []string
		for _, m := range ms {
			codes = append(codes, m.TeamCode)
		}
		require.Equal(t, []string{"1K-IBMI-C1", "1K-IBMI-S1", "1K-IBMI-S2"}, codes)
		require.Equal(t, "Kebersihan", ms[0].ProjectName)
	})
}

func TestOnlyOneDanruPerTeamPerDate(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.personNIK(org, "N-DODI", "Dodi", "ACTIVE")
	query := organization.NewQuery(testkit.Pool(t))

	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "DANRU", "2026-01-01")

	t.Run("a second DANRU on the same dates is TEAM_HAS_DANRU", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S1", "N-CICI", "DANRU", "2026-02-01"),
			http.StatusConflict, "CONFLICT", organization.ReasonTeamHasDanru)
		// A MEMBER is fine, and so is a DANRU of another team.
		e.mustAdd(admin, "1K-IBMI-S1", "N-CICI", "MEMBER", "2026-02-01")
		e.mustAdd(admin, "1K-IBMI-S2", "N-DODI", "DANRU", "2026-02-01")
	})

	t.Run("promoting a member to DANRU of a team that has one is refused too", func(t *testing.T) {
		var ciciRow int64
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(),
				"SELECT id FROM team_members WHERE personnel_id = $1", cici).Scan(&ciciRow))
		})
		expect(t, e.changeRole(admin, ciciRow, "DANRU", "2026-04-01"), http.StatusConflict, "CONFLICT", organization.ReasonTeamHasDanru)
		require.Equal(t, []string{"1K-IBMI-S1 MEMBER 2026-02-01 -"}, e.historyOf(org, cici), "the refused change left no trace")
	})

	t.Run("moving a DANRU into a team that has one is refused", func(t *testing.T) {
		var budiRow int64
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT id FROM team_members WHERE personnel_id = $1", budi).Scan(&budiRow))
		})
		expect(t, e.move(admin, budiRow, "1K-IBMI-S2", "2026-03-01"), http.StatusConflict, "CONFLICT", organization.ReasonTeamHasDanru)
		require.Equal(t, []string{"1K-IBMI-S1 DANRU 2026-01-01 -"}, e.historyOf(org, budi), "the old row was not ended")
	})

	t.Run("after the DANRU leaves, another can take over from the next day", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", budi).Status)
		// Today is 2026-03-10 in Jakarta, so Budi's row ends on it.
		e.mustAdd(admin, "1K-IBMI-S1", "N-DODI", "DANRU", "2026-03-11")
		danru, err := query.TeamDanru(t.Context(), org, "1K-IBMI-S1", day("2026-03-10"))
		require.NoError(t, err)
		require.NotNil(t, danru)
		require.Equal(t, "Budi", danru.FullName)
		danru, err = query.TeamDanru(t.Context(), org, "1K-IBMI-S1", day("2026-03-11"))
		require.NoError(t, err)
		require.Equal(t, "Dodi", danru.FullName)
		danru, err = query.TeamDanru(t.Context(), org, "1K-IBMI-S2", day("2026-01-01"))
		require.NoError(t, err)
		require.Nil(t, danru, "S2 had no DANRU yet")
	})
}

func TestChangingARoleEndsOneRowAndStartsAnother(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")

	first := e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	id := int64(first["id"].(float64)) //nolint:forcetypeassert // test

	got := e.changeRole(admin, id, "DANRU", "2026-03-01")
	require.Equal(t, http.StatusOK, got.Status, "body: %s", got.Raw)
	require.Equal(t, "DANRU", data[row](t, got).str("role"))
	require.Equal(t, []string{
		"1K-IBMI-S1 MEMBER 2026-01-01 2026-02-28",
		"1K-IBMI-S1 DANRU 2026-03-01 -",
	}, e.historyOf(org, budi))

	t.Run("the dates must make sense", func(t *testing.T) {
		var open int64
		e.inTenant(org, func(tx pgx.Tx) {
			require.NoError(t, tx.QueryRow(t.Context(), "SELECT id FROM team_members WHERE role = 'DANRU'").Scan(&open))
		})
		same := e.api(admin, http.MethodPost, "/admin/team-members/"+strconv.FormatInt(open, 10)+"/change-role",
			map[string]any{"role": "DANRU", "effective_date": "2026-04-01"})
		require.Equal(t, http.StatusUnprocessableEntity, same.Status, "the same role: %s", same.Raw)
		require.Contains(t, string(same.Raw), `"role"`)

		early := e.changeRole(admin, open, "MEMBER", "2026-03-01")
		require.Equal(t, http.StatusUnprocessableEntity, early.Status, "a change on the start date itself would leave an empty row: %s", early.Raw)
		require.Contains(t, string(early.Raw), `"effective_date"`)
		require.Equal(t, http.StatusUnprocessableEntity, e.changeRole(admin, open, "MEMBER", "2026-02-01").Status, "before the start")

		missing := e.raw(admin, http.MethodPost, "/admin/team-members/"+strconv.FormatInt(open, 10)+"/change-role", map[string]any{"role": "MEMBER"})
		require.Equal(t, http.StatusUnprocessableEntity, missing.Status, "no date: %s", missing.Raw)
	})

	t.Run("the replaced row is history: it cannot be changed again", func(t *testing.T) {
		expect(t, e.changeRole(admin, id, "MEMBER", "2026-05-01"), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)
	})
}

func TestLeavingEndsTheMembershipToday(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")

	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-CICI", "MEMBER", "2026-06-01") // has not started yet

	t.Run("the row stays and ends on today in the project's timezone", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", budi).Status)
		require.Equal(t, []string{"1K-IBMI-S1 MEMBER 2026-01-01 2026-03-10"}, e.historyOf(org, budi))
	})

	t.Run("a membership that never started is removed", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", cici).Status)
		require.Empty(t, e.historyOf(org, cici))
	})

	t.Run("a person with no membership in force is MEMBERSHIP_NOT_FOUND", func(t *testing.T) {
		expect(t, e.endMember(admin, "1K-IBMI-S1", budi), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)
		expect(t, e.endMember(admin, "1K-IBMI-S1", 999999), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)
		expect(t, e.endMember(admin, "1K-IBMI-S9", budi), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	})

	t.Run("they can be added again afterwards", func(t *testing.T) {
		e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-03-11")
		require.Len(t, e.historyOf(org, budi), 2)
	})
}

func TestDeletingATeamEndsItsMemberships(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-C1", "N-BUDI", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-C1", "N-CICI", "MEMBER", "2026-06-01")

	require.Equal(t, http.StatusOK, e.api(admin, http.MethodDelete, "/admin/teams/1K-IBMI-C1", nil).Status)

	require.Equal(t, []string{"1K-IBMI-C1 MEMBER 2026-01-01 2026-03-10"}, e.historyOf(org, budi), "the one in force ends today")
	require.Empty(t, e.historyOf(org, cici), "the one that had not started goes away")

	var metadata string
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(),
			"SELECT metadata::text FROM audit_logs WHERE action = 'team.delete'").Scan(&metadata))
	})
	require.Contains(t, metadata, `"memberships_ended": 1`)
	require.Contains(t, metadata, `"memberships_cancelled": 1`)

	query := organization.NewQuery(testkit.Pool(t))
	ms, err := query.ActiveMemberships(t.Context(), org, budi, day("2026-03-05"))
	require.NoError(t, err)
	require.Empty(t, ms, "a deleted team is not one of a person's teams")
}

func TestMembersAreListedDanruFirstThenByName(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	for nik, name := range map[string]string{"N-A": "Andi", "N-B": "Budi", "N-C": "Citra", "N-D": "Dewi"} {
		e.personNIK(org, nik, name, "ACTIVE")
	}
	e.mustAdd(admin, "1K-IBMI-S1", "N-A", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-D", "DANRU", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-C", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-B", "MEMBER", "2026-01-01")

	names := func(rows []row) (out []string) {
		for _, r := range rows {
			out = append(out, r.str("full_name"))
		}
		return out
	}
	require.Equal(t, []string{"Dewi", "Andi", "Budi", "Citra"}, names(e.membersOn(admin, "1K-IBMI-S1", "2026-01-01")))

	t.Run("without on_date it is today in the project's timezone", func(t *testing.T) {
		res := e.api(admin, http.MethodGet, "/admin/teams/1K-IBMI-S1/members", nil)
		require.Equal(t, http.StatusOK, res.Status)
		require.Len(t, data[[]row](t, res), 4)
	})

	t.Run("it pages", func(t *testing.T) {
		res := e.raw(admin, http.MethodGet, "/admin/teams/1K-IBMI-S1/members?limit=2&page=2&on_date=2026-01-01", nil)
		require.Equal(t, []string{"Budi", "Citra"}, names(data[[]row](t, res)))
		require.Contains(t, string(res.Raw), `"total":4`)
	})

	t.Run("a bad date or an unknown team is refused", func(t *testing.T) {
		require.Equal(t, http.StatusUnprocessableEntity, e.raw(admin, http.MethodGet, "/admin/teams/1K-IBMI-S1/members?on_date=2026-13-40", nil).Status)
		expect(t, e.api(admin, http.MethodGet, "/admin/teams/1K-IBMI-S9/members", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	})
}

func TestARegionAdminManagesMembersOnlyInTheirRegion(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.tree(admin, 2)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "2K-IBMI-S").Status)
	e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	regionAdmin := e.person(org, "REGION_ADMIN", "OS1")

	inOS1 := e.mustAdd(regionAdmin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
	inOS2 := e.mustAdd(admin, "2K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")

	t.Run("a team of another region is SCOPE_DENIED for every action", func(t *testing.T) {
		deny := func(res reply) {
			t.Helper()
			expect(t, res, http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
		}
		deny(e.api(regionAdmin, http.MethodGet, "/admin/teams/2K-IBMI-S1/members", nil))
		deny(e.addMember(regionAdmin, "2K-IBMI-S1", "N-BUDI", "MEMBER", "2026-02-01"))
		deny(e.endMember(regionAdmin, "2K-IBMI-S1", 1))
		deny(e.move(regionAdmin, int64(inOS2["id"].(float64)), "1K-IBMI-S2", "2026-02-01"))  //nolint:forcetypeassert // test
		deny(e.changeRole(regionAdmin, int64(inOS2["id"].(float64)), "DANRU", "2026-02-01")) //nolint:forcetypeassert // test
		require.Equal(t, []string{"2K-IBMI-S1 MEMBER 2026-01-01 -"}, e.historyOfNIK(org, "N-CICI"), "nothing changed")
	})

	t.Run("moving into another region is refused too", func(t *testing.T) {
		expect(t, e.move(regionAdmin, int64(inOS1["id"].(float64)), "2K-IBMI-S1", "2026-02-01"), //nolint:forcetypeassert // test
			http.StatusForbidden, "SCOPE_DENIED", access.ReasonOutsideScope)
		require.Equal(t, []string{"1K-IBMI-S1 MEMBER 2026-01-01 -"}, e.historyOfNIK(org, "N-BUDI"))
	})

	t.Run("inside their region they add, move, change a role and end", func(t *testing.T) {
		moved := e.move(regionAdmin, int64(inOS1["id"].(float64)), "1K-IBMI-S2", "2026-02-01") //nolint:forcetypeassert // test
		require.Equal(t, http.StatusOK, moved.Status, "body: %s", moved.Raw)
		newID := int64(data[row](t, moved)["id"].(float64)) //nolint:forcetypeassert // test
		require.Equal(t, http.StatusOK, e.changeRole(regionAdmin, newID, "DANRU", "2026-02-15").Status)
		require.Equal(t, http.StatusOK, e.endMember(regionAdmin, "1K-IBMI-S2", int64(data[row](t, moved)["personnel_id"].(float64))).Status) //nolint:forcetypeassert // test
	})

	t.Run("a membership that does not exist is 404 even for a REGION_ADMIN", func(t *testing.T) {
		expect(t, e.move(regionAdmin, 999999, "1K-IBMI-S2", "2026-02-01"), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)
	})
}

func (e *env) historyOfNIK(org int64, nik string) (rows []string) {
	e.t.Helper()
	var id int64
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), "SELECT id FROM personnels WHERE nik = $1", nik).Scan(&id))
	})
	return e.historyOf(org, id)
}

func TestPeopleAreValidatedBeforeTheyJoin(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.personNIK(org, "N-OFF", "Off", "INACTIVE")
	e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")

	t.Run("a NIK Personalia does not know", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S1", "N-NOBODY", "MEMBER", "2026-01-01"),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
	})
	t.Run("a person Personalia reports as inactive", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S1", "N-OFF", "MEMBER", "2026-01-01"),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
	})
	t.Run("a team that does not exist", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S9", "N-BUDI", "MEMBER", "2026-01-01"),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	})
	t.Run("a missing date, a bad role and an unknown field", func(t *testing.T) {
		path := "/admin/teams/1K-IBMI-S1/members"
		for name, body := range map[string]map[string]any{
			"no date":     {"nik": "N-BUDI", "role": "MEMBER"},
			"a bad date":  {"nik": "N-BUDI", "role": "MEMBER", "start_date": "01/02/2026"},
			"a bad role":  {"nik": "N-BUDI", "role": "CHIEF", "start_date": "2026-01-01"},
			"no NIK":      {"role": "MEMBER", "start_date": "2026-01-01"},
			"a blank NIK": {"nik": "  ", "role": "MEMBER", "start_date": "2026-01-01"},
			"extra field": {"nik": "N-BUDI", "role": "MEMBER", "start_date": "2026-01-01", "team_code": "1K-IBMI-S2"},
		} {
			res := e.raw(admin, http.MethodPost, path, body)
			require.Equal(t, http.StatusUnprocessableEntity, res.Status, "%s: %s", name, res.Raw)
		}
	})
	t.Run("a move to the same team or an unknown one", func(t *testing.T) {
		first := e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")
		id := int64(first["id"].(float64)) //nolint:forcetypeassert // test
		res := e.move(admin, id, "1K-IBMI-S1", "2026-02-01")
		require.Equal(t, http.StatusUnprocessableEntity, res.Status, "body: %s", res.Raw)
		require.Contains(t, string(res.Raw), `"to_team_code"`)
		expect(t, e.move(admin, id, "1K-IBMI-S9", "2026-02-01"), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
		require.Equal(t, http.StatusUnprocessableEntity, e.move(admin, id, "1K-IBMI-S2", "2026-01-01").Status, "on the start date")
	})
	t.Run("a membership id that is not a number", func(t *testing.T) {
		res := e.raw(admin, http.MethodPost, "/admin/team-members/abc/move", map[string]any{"to_team_code": "1K-IBMI-S2", "effective_date": "2026-02-01"})
		require.Equal(t, http.StatusNotFound, res.Status)
	})
}

func TestMembersStayInsideTheirOrganization(t *testing.T) {
	e := newMemberEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	adminA, adminB := e.person(orgA, "ORG_ADMIN", ""), e.person(orgB, "ORG_ADMIN", "")
	e.fixture(adminA)
	e.personNIK(orgA, "N-BUDI", "Budi", "ACTIVE")
	first := e.mustAdd(adminA, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-01-01")

	expect(t, e.api(adminB, http.MethodGet, "/admin/teams/1K-IBMI-S1/members", nil), http.StatusNotFound, "NOT_FOUND", organization.ReasonTeamNotFound)
	expect(t, e.move(adminB, int64(first["id"].(float64)), "1K-IBMI-S2", "2026-02-01"), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound)  //nolint:forcetypeassert // test
	expect(t, e.changeRole(adminB, int64(first["id"].(float64)), "DANRU", "2026-02-01"), http.StatusNotFound, "NOT_FOUND", organization.ReasonMembershipNotFound) //nolint:forcetypeassert // test

	query := organization.NewQuery(testkit.Pool(t))
	_, err := query.TeamWithProject(t.Context(), orgB, "1K-IBMI-S1")
	require.ErrorIs(t, err, organization.ErrTeamNotFound)
}

func TestAPersonWhoNeverSignedInCanBeAddedByNIK(t *testing.T) {
	fake, personelClient := testkit.NewPersonel(t)
	people := identity.NewPeople(testkit.Pool(t), personelClient, clock.Fixed{T: now})
	e := newEnvWith(t, people, clock.Fixed{T: now})

	org := personeltest.OrgSatu
	require.NoError(t, database.WithoutTenant(t.Context(), testkit.Pool(t), func(q *db.Queries) error {
		_, err := q.UpsertOrganizationSeen(t.Context(), db.UpsertOrganizationSeenParams{ID: org, Name: "PT Siaga Satu (seed)"})
		return err
	}))
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)

	res := e.addMember(admin, "1K-IBMI-S1", personeltest.NIKAnggota2Satu, "MEMBER", "2026-01-01")
	require.Equal(t, http.StatusCreated, res.Status, "body: %s", res.Raw)
	got := data[row](t, res)
	require.Equal(t, "Anggota Satu B", got.str("full_name"))
	require.Equal(t, 1, fake.CallCount("/employees"), "Personalia was asked once")

	// Known now: adding them to a second team is a local lookup.
	e.mustAdd(admin, "1K-IBMI-S2", personeltest.NIKAnggota2Satu, "MEMBER", "2026-01-01")
	require.Equal(t, 1, fake.CallCount("/employees"))

	t.Run("a NIK of another company is not found", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S1", personeltest.NIKAnggota1Dua, "MEMBER", "2026-01-01"),
			http.StatusNotFound, "NOT_FOUND", organization.ReasonPersonnelNotFound)
	})

	t.Run("an inactive person is cached and refused", func(t *testing.T) {
		expect(t, e.addMember(admin, "1K-IBMI-S1", personeltest.NIKNonaktifSatu, "MEMBER", "2026-01-01"),
			http.StatusUnprocessableEntity, "VALIDATION_ERROR", organization.ReasonPersonnelInactive)
	})

	t.Run("Personalia down is a 503, not a 500", func(t *testing.T) {
		fake.Down()
		defer fake.Up()
		res := e.addMember(admin, "1K-IBMI-S1", personeltest.NIKAnggota1Satu, "MEMBER", "2026-01-01")
		require.Equal(t, http.StatusServiceUnavailable, res.Status, "body: %s", res.Raw)
		_, reason := res.reason(t)
		require.Equal(t, apperr.ReasonPersonelUnavailable, reason)
	})
}

func TestTheDirectoryFollowsEachProjectsOwnToday(t *testing.T) {
	// 16:30 UTC on 1 March is 23:30 on 1 March in Jakarta (UTC+7) but already 01:30 on
	// 2 March in Jayapura (UTC+9): "today" is the project's, not UTC's.
	instant := time.Date(2026, 3, 1, 16, 30, 0, 0, time.UTC)
	e := newEnvWith(t, localPeople{t}, clock.Fixed{T: instant})
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.tree(admin, 1) // 1K-IBMI-S in Asia/Jakarta
	e.mustCreate(admin, "/admin/clients", map[string]any{"branch_code": "1K", "company_prefix": "PAPUA", "description": "Klien Papua"})
	e.mustCreate(admin, "/admin/projects", map[string]any{"client_code": "1K-PAPUA", "business_line": "security", "description": "Papua", "timezone": "Asia/Jayapura"})
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-IBMI-S").Status)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "1K-PAPUA-S").Status)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "MEMBER", "2026-03-02")
	e.mustAdd(admin, "1K-PAPUA-S1", "N-BUDI", "DANRU", "2026-03-02")

	directory := organization.NewDirectory(clock.Fixed{T: instant})
	var memberships []identity.Membership
	var regions []string
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		var err error
		if memberships, err = directory.MembershipsOf(t.Context(), q, budi); err != nil {
			return err
		}
		regions, err = directory.RegionCodesOf(t.Context(), q, budi)
		return err
	}))

	require.Len(t, memberships, 1, "only the Jayapura team has started")
	require.Equal(t, "1K-PAPUA-S1", memberships[0].TeamCode)
	require.Equal(t, "Papua", memberships[0].ProjectName)
	require.Equal(t, "DANRU", memberships[0].Role)
	require.Equal(t, "2026-03-02", memberships[0].StartDate.String())
	require.Equal(t, []string{"OS1"}, regions)

	later := organization.NewDirectory(clock.Fixed{T: instant.Add(2 * time.Hour)}) // 18:30 UTC: 2 March in Jakarta too
	require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
		var err error
		memberships, err = later.MembershipsOf(t.Context(), q, budi)
		return err
	}))
	require.Len(t, memberships, 2)
}

func TestTheQueryAnswersOtherModulesQuestions(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "DANRU", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S2", "N-CICI", "MEMBER", "2026-01-01")
	query := organization.NewQuery(testkit.Pool(t))
	on := day("2026-02-01")

	member, err := query.IsMemberOfProject(t.Context(), org, cici, "1K-IBMI-S", on)
	require.NoError(t, err)
	require.True(t, member, "a MEMBER belongs to the project of their regu")
	danru, err := query.IsDanruOfProject(t.Context(), org, cici, "1K-IBMI-S", on)
	require.NoError(t, err)
	require.False(t, danru)
	danru, err = query.IsDanruOfProject(t.Context(), org, budi, "1K-IBMI-S", on)
	require.NoError(t, err)
	require.True(t, danru, "a DANRU of S1 is the DANRU of the project")
	member, err = query.IsMemberOfProject(t.Context(), org, budi, "1K-IBMI-C", on)
	require.NoError(t, err)
	require.False(t, member, "not in the cleaning project")
	member, err = query.IsMemberOfProject(t.Context(), org, budi, "1K-IBMI-S", day("2025-12-31"))
	require.NoError(t, err)
	require.False(t, member, "before the start")

	members, err := query.TeamMembers(t.Context(), org, "1K-IBMI-S2", on)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "Cici", members[0].FullName)

	tp, err := query.TeamWithProject(t.Context(), org, "1K-IBMI-S2")
	require.NoError(t, err)
	require.Equal(t, "1K-IBMI-S", tp.Project.Code)
	require.Equal(t, "1K-IBMI-S2", tp.Team.Code)
	require.Equal(t, "Asia/Jakarta", tp.Project.Timezone)
	_, err = query.TeamWithProject(t.Context(), org, "1K-IBMI-S9")
	require.ErrorIs(t, err, organization.ErrTeamNotFound)
}

func TestTheTableKeepsMembershipsConsistent(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "DANRU", "2026-01-01")
	id := strconv.FormatInt(budi, 10)

	// The API says all of this first; these are what the database alone refuses.
	for name, sql := range map[string]string{
		"an overlapping row of the same person": "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', " + id + ", 'MEMBER', '2026-05-01')",
		"a row that ends before it starts":      "INSERT INTO team_members (team_code, personnel_id, role, start_date, end_date) VALUES ('1K-IBMI-S2', " + id + ", 'MEMBER', '2026-05-01', '2026-04-30')",
		"a team that does not exist":            "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S9', " + id + ", 'MEMBER', '2026-05-01')",
		"a person of nobody's organization":     "INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S2', 987654, 'MEMBER', '2026-05-01')",
	} {
		require.Error(t, execInTenant(t, org, sql), name)
	}
	// Touching is allowed: a row ending on D and another starting on D+1.
	require.NoError(t, execInTenant(t, org,
		"UPDATE team_members SET end_date = '2026-01-31' WHERE personnel_id = "+id))
	require.NoError(t, execInTenant(t, org,
		"INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', "+id+", 'DANRU', '2026-02-01')"))
}

func TestIdentityReadsTheMembershipsThroughTheDirectory(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	e.tree(admin, 2)
	require.Equal(t, http.StatusCreated, e.newTeam(admin, "2K-IBMI-S").Status)
	budi := e.personNIK(org, "N-BUDI", "Budi", "ACTIVE")
	cici := e.personNIK(org, "N-CICI", "Cici", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-BUDI", "DANRU", "2026-01-01")
	e.mustAdd(admin, "2K-IBMI-S1", "N-CICI", "MEMBER", "2026-01-01")

	directory := organization.NewDirectory(clock.Fixed{T: now})
	usecase := identity.NewUsecase(testkit.Pool(t), nil, testkit.Tokens(t), testkit.NewRevocations(t, nil), clock.Fixed{T: now}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	usecase.UseRegionLookup(directory)
	usecase.UseMembershipLookup(directory)

	t.Run("/auth/me lists the regu the person serves in today", func(t *testing.T) {
		me, err := usecase.Me(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: budi})
		require.NoError(t, err)
		require.Len(t, me.Memberships, 1)
		require.Equal(t, "1K-IBMI-S1", me.Memberships[0].TeamCode)
		require.Equal(t, "DANRU", me.Memberships[0].Role)
		require.Equal(t, "1K-IBMI-S", me.Memberships[0].ProjectCode)

		none, err := usecase.Me(t.Context(), auth.Principal{OrganizationID: org, PersonnelID: e.personNIK(org, "N-NEW", "Baru", "ACTIVE")})
		require.NoError(t, err)
		require.Empty(t, none.Memberships)
	})

	t.Run("a REGION_ADMIN reaches only people who serve under their region", func(t *testing.T) {
		regionAdmin := auth.Principal{OrganizationID: org, PersonnelID: e.personNIK(org, "N-RA", "Admin Wilayah", "ACTIVE")}
		scope := access.AdminScope{RegionCode: "OS1"}
		require.NoError(t, usecase.ResetDeviceBinding(t.Context(), regionAdmin, scope, budi), "Budi serves in OS1")
		require.ErrorIs(t, usecase.ResetDeviceBinding(t.Context(), regionAdmin, scope, cici), access.ErrOutsideAdminScope, "Cici serves in OS2")
	})

	t.Run("a person who left the regu is out of reach", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.endMember(admin, "1K-IBMI-S1", budi).Status)
		// The end date is today (2026-03-10) and still counts that day; from tomorrow on it does not.
		later := organization.NewDirectory(clock.Fixed{T: now.Add(24 * time.Hour)})
		require.NoError(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
			regions, err := later.RegionCodesOf(t.Context(), q, budi)
			require.Empty(t, regions)
			return err
		}))
	})
}
