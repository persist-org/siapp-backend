package organization_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/organization"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

// meClient serves the real identity handler (GET /auth/me) on the same database as the admin
// routes of e, with the memberships read through the real Directory at the given "now". Every
// request and response is checked against the contract: a scope the contract does not list
// (NONE) or a missing permission list fails the test.
func (e *env) meClient(clk clock.Clock) *testkit.Client {
	e.t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	revocations := testkit.NewRevocations(e.t, logger)
	usecase := identity.NewUsecase(testkit.Pool(e.t), nil, testkit.Tokens(e.t), revocations, clk, logger)
	directory := organization.NewDirectory(clk)
	usecase.UseRegionLookup(directory)
	usecase.UseMembershipLookup(directory)
	failures := httpx.NewFailureLimiter(testkit.Redis(e.t), testkit.RandomPrefix(e.t), logger)
	handler := identity.NewHandler(usecase, httpx.ClientIPFunc(""), failures, nil)
	srv := testkit.NewServer(e.t, testkit.Deps{Revocations: revocations, Mount: handler.Mount})
	return testkit.NewClient(e.t, srv)
}

type meView struct {
	Memberships []struct {
		TeamCode string `json:"team_code"`
		Role     string `json:"role"`
	} `json:"memberships"`
	Permissions []struct {
		Permission string `json:"permission"`
		Scope      string `json:"scope"`
	} `json:"permissions"`
}

// scopes is the permissions of the response by key.
func (m meView) scopes() map[string]string {
	out := map[string]string{}
	for _, p := range m.Permissions {
		out[p.Permission] = p.Scope
	}
	return out
}

func (e *env) meOf(c *testkit.Client, org, personnelID int64) (meView, []byte) {
	e.t.Helper()
	c.Token = testkit.Token(e.t, org, personnelID)
	res := c.Do(http.MethodGet, "/auth/me", nil)
	require.Equal(e.t, http.StatusOK, res.Status, "body: %s", res.Body)
	return data[meView](e.t, reply{Status: res.Status, Raw: res.Body}), res.Body
}

// wantFromCatalog is what a person holding these roles in projects of these lines gets from
// the untouched default matrix, worked out from the catalog and not from the database.
func wantFromCatalog(held ...access.Held) map[string]string {
	out := map[string]string{}
	for _, e := range access.Catalog() {
		best := access.ScopeNone
		for _, h := range held {
			if len(e.BusinessLines) > 0 && !contains(e.BusinessLines, h.Line) {
				continue
			}
			best = access.Widest(best, e.Defaults[h.Role])
		}
		if best != access.ScopeNone {
			out[string(e.Key)] = string(best)
		}
	}
	return out
}

func contains(lines []access.BusinessLine, line access.BusinessLine) bool {
	for _, l := range lines {
		if l == line {
			return true
		}
	}
	return false
}

func (e *env) editMatrix(org int64, role access.TeamRole, permission access.Permission, scope access.Scope) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		tag, err := tx.Exec(e.t.Context(),
			"UPDATE role_permissions SET scope = $3::permission_scope WHERE team_role = $1::team_role AND permission = $2", role, permission, scope)
		require.NoError(e.t, err)
		require.EqualValues(e.t, 1, tag.RowsAffected(), "%s of %s is seeded", permission, role)
	})
}

func (e *env) matrixRows(org int64) (n int) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(e.t, tx.QueryRow(e.t.Context(), "SELECT count(*) FROM role_permissions").Scan(&n))
	})
	return n
}

func TestMeListsThePermissionsOfTheRolesThePersonHoldsToday(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	admin := e.person(org, "ORG_ADMIN", "")
	e.fixture(admin)
	danru := e.personNIK(org, "N-DANRU", "Danru", "ACTIVE")
	member := e.personNIK(org, "N-MEMBER", "Anggota", "ACTIVE")
	cleaner := e.personNIK(org, "N-CLEAN", "Petugas Kebersihan", "ACTIVE")
	e.mustAdd(admin, "1K-IBMI-S1", "N-DANRU", "DANRU", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-S1", "N-MEMBER", "MEMBER", "2026-01-01")
	e.mustAdd(admin, "1K-IBMI-C1", "N-CLEAN", "MEMBER", "2026-01-01")
	c := e.meClient(clock.Fixed{T: now})
	security, cleaning := access.LineSecurity, access.LineCleaning

	// Nobody has signed in: the matrix is not seeded yet. The first /auth/me of a person who
	// serves in a regu gives the organization its defaults, so a person who signed in before
	// the matrix existed does not see every menu hidden.
	require.Equal(t, 0, e.matrixRows(org))
	first, raw := e.meOf(c, org, member)
	require.Equal(t, len(access.Catalog())*len(access.TeamRoles()), e.matrixRows(org))
	require.Contains(t, string(raw), `"permissions":[`)
	require.Equal(t, wantFromCatalog(access.Held{Role: access.RoleMember, Line: security}), first.scopes())

	t.Run("a member and a DANRU of the same regu differ", func(t *testing.T) {
		m, _ := e.meOf(c, org, member)
		d, _ := e.meOf(c, org, danru)

		require.Equal(t, "SELF", m.scopes()["attendance.checkin"])
		require.Equal(t, "TEAM", m.scopes()["report.view"])
		require.Equal(t, "PROJECT", m.scopes()["cctv.view"])
		require.NotContains(t, m.scopes(), "attendance.leave.approve", "a member cannot approve leave")

		require.Equal(t, "TEAM", d.scopes()["attendance.leave.approve"])
		require.Equal(t, "PROJECT", d.scopes()["report.view"])
		require.NotContains(t, d.scopes(), "report.create.danru_only", "DANRU has no one above to send DANRU-only reports to")

		require.Equal(t, wantFromCatalog(access.Held{Role: access.RoleDanru, Line: security}), d.scopes())
	})

	t.Run("ememo.use appears only for someone in a cleaning project", func(t *testing.T) {
		clean, _ := e.meOf(c, org, cleaner)
		require.Equal(t, "TEAM", clean.scopes()["ememo.use"])
		require.Equal(t, wantFromCatalog(access.Held{Role: access.RoleMember, Line: cleaning}), clean.scopes())

		for _, who := range []int64{member, danru} {
			got, _ := e.meOf(c, org, who)
			require.NotContains(t, got.scopes(), "ememo.use", "a security project has no E-Memo")
		}
	})

	t.Run("the permissions follow the catalog's order and each key is listed once", func(t *testing.T) {
		got, _ := e.meOf(c, org, danru)
		var want []string
		for _, entry := range access.Catalog() {
			if _, ok := got.scopes()[string(entry.Key)]; ok {
				want = append(want, string(entry.Key))
			}
		}
		var listed []string
		for _, p := range got.Permissions {
			listed = append(listed, p.Permission)
		}
		require.Equal(t, want, listed)
	})

	t.Run("an admin's edit shows at the next call, and a NONE is never listed", func(t *testing.T) {
		e.editMatrix(org, access.RoleDanru, access.AttendanceLeaveApprove, access.ScopeNone)
		e.editMatrix(org, access.RoleMember, access.AttendanceLeaveApprove, access.ScopeProject)

		d, _ := e.meOf(c, org, danru)
		require.NotContains(t, d.scopes(), "attendance.leave.approve")
		m, _ := e.meOf(c, org, member)
		require.Equal(t, "PROJECT", m.scopes()["attendance.leave.approve"])
		for _, p := range m.Permissions {
			require.NotEqual(t, "NONE", p.Scope, p.Permission)
		}
	})

	t.Run("a person in two projects gets the widest scope, and a business line counts only through its own", func(t *testing.T) {
		both := e.personNIK(org, "N-BOTH", "Dua Proyek", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S2", "N-BOTH", "DANRU", "2026-01-01") // security
		e.mustAdd(admin, "1K-IBMI-C1", "N-BOTH", "MEMBER", "2026-01-01")
		// ⚠ DANRU is wide on E-Memo, MEMBER narrow. The wide grant belongs to the security
		// membership, which has no E-Memo, so it must not widen the cleaning one.
		e.editMatrix(org, access.RoleDanru, access.EMemoUse, access.ScopeProject)
		e.editMatrix(org, access.RoleMember, access.EMemoUse, access.ScopeSelf)

		got, _ := e.meOf(c, org, both)
		require.Len(t, got.Memberships, 2)
		require.Equal(t, "SELF", got.scopes()["ememo.use"], "only the cleaning membership counts")
		require.Equal(t, "PROJECT", got.scopes()["report.view"], "DANRU's PROJECT beats MEMBER's TEAM")
		require.Equal(t, "TEAM", got.scopes()["attendance.absence.view"], "DANRU's TEAM beats MEMBER's SELF")
	})

	t.Run("a membership that has not started, or has ended, grants nothing", func(t *testing.T) {
		future := e.personNIK(org, "N-FUTURE", "Belum Mulai", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S2", "N-FUTURE", "MEMBER", "2026-03-11") // tomorrow
		ended := e.personNIK(org, "N-ENDED", "Sudah Selesai", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S2", "N-ENDED", "MEMBER", "2026-01-01")
		e.inTenant(org, func(tx pgx.Tx) {
			_, err := tx.Exec(t.Context(), "UPDATE team_members SET end_date = '2026-03-09' WHERE personnel_id = $1", ended)
			require.NoError(t, err)
		})

		for name, who := range map[string]int64{"not started": future, "ended": ended} {
			got, raw := e.meOf(c, org, who)
			require.Empty(t, got.Memberships, name)
			require.Empty(t, got.Permissions, name)
			require.Contains(t, string(raw), `"permissions":[]`, name)
		}
	})

	t.Run("an admin who serves in no regu holds no field permission", func(t *testing.T) {
		got, raw := e.meOf(c, org, e.personNIK(org, "N-ADMIN", "Admin", "ACTIVE"))
		require.Empty(t, got.Permissions)
		require.Contains(t, string(raw), `"permissions":[]`)
	})
}

func TestMeReadsOnlyItsOwnOrganizationsMatrix(t *testing.T) {
	e := newMemberEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	for _, org := range []int64{orgA, orgB} {
		admin := e.person(org, "ORG_ADMIN", "")
		e.fixture(admin)
		e.personNIK(org, "N-MEMBER", "Anggota", "ACTIVE")
		e.mustAdd(admin, "1K-IBMI-S1", "N-MEMBER", "MEMBER", "2026-01-01")
	}
	c := e.meClient(clock.Fixed{T: now})
	inA := scalarID(t, e, orgA, "N-MEMBER")
	inB := scalarID(t, e, orgB, "N-MEMBER")

	gotA, _ := e.meOf(c, orgA, inA)
	gotB, _ := e.meOf(c, orgB, inB)
	require.Equal(t, gotA.scopes(), gotB.scopes())

	// A takes everything away from MEMBER; B is as it was.
	e.inTenant(orgA, func(tx pgx.Tx) {
		_, err := tx.Exec(t.Context(), "UPDATE role_permissions SET scope = 'NONE' WHERE team_role = 'MEMBER'")
		require.NoError(t, err)
	})
	gotA, _ = e.meOf(c, orgA, inA)
	gotB, _ = e.meOf(c, orgB, inB)
	require.Empty(t, gotA.Permissions)
	require.Equal(t, wantFromCatalog(access.Held{Role: access.RoleMember, Line: access.LineSecurity}), gotB.scopes())
}

func scalarID(t *testing.T, e *env, org int64, nik string) (id int64) {
	t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(t.Context(), "SELECT id FROM personnels WHERE nik = $1", nik).Scan(&id))
	})
	return id
}

func TestMeDoesNotSeedForAPersonWhoHoldsNothing(t *testing.T) {
	e := newMemberEnv(t)
	org := testkit.NewOrg(t)
	e.person(org, "ORG_ADMIN", "")
	nobody := e.personNIK(org, "N-NOBODY", "Tanpa Regu", "ACTIVE")
	c := e.meClient(clock.Fixed{T: now})

	got, raw := e.meOf(c, org, nobody)
	require.Empty(t, got.Permissions)
	require.Contains(t, string(raw), `"permissions":[]`)
	require.Equal(t, 0, e.matrixRows(org), "reading the profile of a person with no regu writes nothing")
}
