package access_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

// newRouteEnv serves test routes guarded the way a feature module will guard its own. They are
// not in the contract, so the requests are plain HTTP; an error still has to be a contract
// ErrorEnvelope (raw checks that).
func newRouteEnv(t *testing.T) *env {
	t.Helper()
	testkit.Reset(t)
	svc := access.NewService(testkit.Pool(t), clock.System{})

	// What a usecase does with the Resolution the middleware left: it never resolves again.
	scopeOf := func(w http.ResponseWriter, r *http.Request) {
		res, ok := access.From(r.Context())
		if !ok {
			httpx.Error(w, r, apperr.ErrInternal)
			return
		}
		httpx.OK(w, r, http.StatusOK, map[string]any{
			"allowed": res.Allowed, "self_only": res.SelfOnly, "teams": res.TeamCodes, "projects": res.ProjectCodes,
		})
	}
	// A route about one regu: the usecase checks that regu against the Resolution.
	teamRoute := func(w http.ResponseWriter, r *http.Request) {
		res, _ := access.From(r.Context())
		if !res.HasTeam(chi.URLParam(r, "team_code")) {
			httpx.Error(w, r, access.ErrPermissionDenied)
			return
		}
		httpx.OK(w, r, http.StatusOK, nil)
	}
	ok := func(w http.ResponseWriter, r *http.Request) { httpx.OK(w, r, http.StatusOK, nil) }

	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.With(svc.Require(access.AttendanceQRDisplay)).Get("/api/v1/test/qr", scopeOf)
		r.With(svc.Require(access.AttendanceCheckin)).Get("/api/v1/test/checkin", scopeOf)
		r.With(svc.Require(access.AttendanceQRDisplay)).Get("/api/v1/test/teams/{team_code}/qr", teamRoute)
		r.With(svc.Require(access.EMemoUse), svc.RequireBusinessLine(access.LineCleaning, access.ProjectOfTeamParam("team_code"))).
			Get("/api/v1/test/teams/{team_code}/ememo", ok)
		r.With(svc.RequireBusinessLine(access.LineCleaning, access.ProjectParam("project_code"))).
			Get("/api/v1/test/projects/{project_code}/cleaning", ok)
		r.With(svc.RequireBusinessLine(access.LineSecurity, access.ProjectParam("project_code"))).
			Get("/api/v1/test/projects/{project_code}/security", ok)
	}})
	return &env{t: t, srv: srv.URL}
}

type resolutionBody struct {
	Allowed  bool     `json:"allowed"`
	SelfOnly bool     `json:"self_only"`
	Teams    []string `json:"teams"`
	Projects []string `json:"projects"`
}

func TestRequireGuardsAFieldRouteWithAPermission(t *testing.T) {
	e := newRouteEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)

	danru, member := e.field(org), e.field(org)
	e.member(org, "1K-IBMI-S1", danru, "DANRU", "2000-01-01", "")
	e.member(org, "1K-IBMI-S1", member, "MEMBER", "2000-01-01", "")
	token := func(id int64) string { return testkit.Token(t, org, id) }

	t.Run("a member is refused with 403 PERMISSION_DENIED", func(t *testing.T) {
		res := e.raw(token(member), http.MethodGet, "/test/qr", nil)
		require.Equal(t, http.StatusForbidden, res.Status, "body: %s", res.Raw)
		code, reason, _ := res.failure(t)
		require.Equal(t, "FORBIDDEN", code)
		require.Equal(t, access.ReasonPermissionDenied, reason)
	})

	t.Run("a DANRU gets through, and the usecase reads the Resolution from the context", func(t *testing.T) {
		res := e.raw(token(danru), http.MethodGet, "/test/qr", nil)
		require.Equal(t, http.StatusOK, res.Status, "body: %s", res.Raw)
		got := data[resolutionBody](t, res)
		require.True(t, got.Allowed)
		require.False(t, got.SelfOnly)
		require.Equal(t, []string{"1K-IBMI-S1"}, got.Teams)
		require.Contains(t, string(res.Raw), `"projects":[]`)
	})

	t.Run("a SELF permission lets a member in, flagged self-only", func(t *testing.T) {
		got := data[resolutionBody](t, e.raw(token(member), http.MethodGet, "/test/checkin", nil))
		require.True(t, got.Allowed)
		require.True(t, got.SelfOnly)
		require.Empty(t, got.Teams)
	})

	t.Run("a person in no team is refused", func(t *testing.T) {
		res := e.raw(token(e.field(org)), http.MethodGet, "/test/qr", nil)
		require.Equal(t, http.StatusForbidden, res.Status)
		_, reason, _ := res.failure(t)
		require.Equal(t, access.ReasonPermissionDenied, reason)
	})

	t.Run("a web admin who serves in no team is refused: admins use /admin routes", func(t *testing.T) {
		admin := e.person(org, access.RoleOrgAdmin)
		res := e.raw(admin, http.MethodGet, "/test/qr", nil)
		require.Equal(t, http.StatusForbidden, res.Status)
		_, reason, _ := res.failure(t)
		require.Equal(t, access.ReasonPermissionDenied, reason)
	})

	t.Run("no token is 401, before any permission is looked at", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/test/qr", nil).Status)
	})

	t.Run("the next call sees a change to the matrix: nothing is cached in the token", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.raw(token(danru), http.MethodGet, "/test/qr", nil).Status)
		e.setScope(org, access.RoleDanru, access.AttendanceQRDisplay, access.ScopeNone)
		require.Equal(t, http.StatusForbidden, e.raw(token(danru), http.MethodGet, "/test/qr", nil).Status)
		e.setScope(org, access.RoleMember, access.AttendanceQRDisplay, access.ScopeTeam)
		require.Equal(t, http.StatusOK, e.raw(token(member), http.MethodGet, "/test/qr", nil).Status)
	})

	t.Run("an ended membership no longer opens the route", func(t *testing.T) {
		e.setScope(org, access.RoleDanru, access.AttendanceQRDisplay, access.ScopeTeam)
		leaver := e.field(org)
		e.member(org, "1K-IBMI-S2", leaver, "MEMBER", "2000-01-01", "2000-12-31")
		e.setScope(org, access.RoleMember, access.AttendanceQRDisplay, access.ScopeTeam)
		require.Equal(t, http.StatusForbidden, e.raw(token(leaver), http.MethodGet, "/test/qr", nil).Status)
	})
}

func TestARouteAboutOneTeamNeverReachesAnotherOrganizationsTeam(t *testing.T) {
	e := newRouteEnv(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	e.world(orgA)
	e.world(orgB)
	e.exec(orgB, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S9', '1K-IBMI-S', 'only in B')")

	danruA := e.field(orgA)
	e.member(orgA, "1K-IBMI-S1", danruA, "DANRU", "2000-01-01", "")
	token := testkit.Token(t, orgA, danruA)

	require.Equal(t, http.StatusOK, e.raw(token, http.MethodGet, "/test/teams/1K-IBMI-S1/qr", nil).Status, "their own team")
	for name, team := range map[string]string{
		"a team of another organization":    "1K-IBMI-S9",
		"a sibling team they do not lead":   "1K-IBMI-S2",
		"a team that does not exist":        "9Z-NOPE-S1",
		"a team in another project of A":    "1K-ABCD-S1",
		"a team of a different region of A": "2K-QQQQ-S1",
	} {
		res := e.raw(token, http.MethodGet, "/test/teams/"+team+"/qr", nil)
		require.Equal(t, http.StatusForbidden, res.Status, "%s: %s", name, res.Raw)
		_, reason, _ := res.failure(t)
		require.Equal(t, access.ReasonPermissionDenied, reason, name)
	}
}

func TestRequireBusinessLineKeepsACleaningRouteToCleaningProjects(t *testing.T) {
	e := newRouteEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)
	other := testkit.NewOrg(t)
	e.world(other)
	e.exec(other, "INSERT INTO clients (code, branch_code, description, company_prefix) VALUES ('1K-ONLY', '1K', 'only in the other', 'ONLY')")
	e.exec(other, "INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ('1K-ONLY-C', '1K-ONLY', 'x', 'cleaning', 'Asia/Jakarta')")

	cleaner := e.field(org)
	e.member(org, "1K-IBMI-C1", cleaner, "MEMBER", "2000-01-01", "")
	guard := e.field(org)
	e.member(org, "1K-IBMI-S1", guard, "DANRU", "2000-01-01", "")
	cleanerToken, guardToken := testkit.Token(t, org, cleaner), testkit.Token(t, org, guard)

	refused := func(res reply, why string) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, res.Status, "%s: %s", why, res.Raw)
		code, reason, _ := res.failure(t)
		require.Equal(t, "FORBIDDEN", code, why)
		require.Equal(t, access.ReasonFeatureNotEnabled, reason, why)
	}

	t.Run("a cleaning project passes, a security project does not", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.raw(cleanerToken, http.MethodGet, "/test/projects/1K-IBMI-C/cleaning", nil).Status)
		refused(e.raw(cleanerToken, http.MethodGet, "/test/projects/1K-IBMI-S/cleaning", nil), "a security project")
	})

	t.Run("the gate works the other way for a security route", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.raw(guardToken, http.MethodGet, "/test/projects/1K-IBMI-S/security", nil).Status)
		refused(e.raw(guardToken, http.MethodGet, "/test/projects/1K-IBMI-C/security", nil), "a cleaning project")
	})

	t.Run("a project that does not exist, or is another organization's, answers the same", func(*testing.T) {
		refused(e.raw(cleanerToken, http.MethodGet, "/test/projects/9Z-NOPE-C/cleaning", nil), "unknown project")
		refused(e.raw(cleanerToken, http.MethodGet, "/test/projects/1K-ONLY-C/cleaning", nil), "a project of another organization")
	})

	t.Run("a deleted project is not enabled", func(*testing.T) {
		e.exec(org, "UPDATE projects SET deleted_at = now() WHERE code = '1K-IBMI-C'")
		refused(e.raw(cleanerToken, http.MethodGet, "/test/projects/1K-IBMI-C/cleaning", nil), "a deleted project")
		e.exec(org, "UPDATE projects SET deleted_at = NULL WHERE code = '1K-IBMI-C'")
	})

	t.Run("the project is taken from a team code too", func(t *testing.T) {
		require.Equal(t, http.StatusOK, e.raw(cleanerToken, http.MethodGet, "/test/teams/1K-IBMI-C1/ememo", nil).Status)
		// Not a team code at all: no number at the end, so no project.
		refused(e.raw(cleanerToken, http.MethodGet, "/test/teams/NOT-A-TEAM/ememo", nil), "no project in the code")
	})

	t.Run("a permission and a business line are two gates: the permission answers first", func(t *testing.T) {
		// A DANRU of a security team: E-Memo is not theirs at all.
		res := e.raw(guardToken, http.MethodGet, "/test/teams/1K-IBMI-S1/ememo", nil)
		require.Equal(t, http.StatusForbidden, res.Status)
		_, reason, _ := res.failure(t)
		require.Equal(t, access.ReasonPermissionDenied, reason)
	})

	t.Run("no token is 401", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, e.raw("", http.MethodGet, "/test/projects/1K-IBMI-C/cleaning", nil).Status)
	})
}

func TestTheMiddlewareRefusesAMistakeAtStartUp(t *testing.T) {
	e := newEnv(t)
	_ = e
	svc := access.NewService(testkit.Pool(t), clock.System{})
	require.Panics(t, func() { svc.Require("attendance.checkn") }, "a permission that is not in the catalog")
	require.Panics(t, func() { svc.RequireBusinessLine("janitorial", access.ProjectParam("p")) }, "a business line that does not exist")
	require.NotPanics(t, func() { svc.Require(access.AttendanceCheckin) })
}

func TestFromIsEmptyOutsideAGuardedRoute(t *testing.T) {
	_, ok := access.From(t.Context())
	require.False(t, ok)
	ctx := access.WithResolution(t.Context(), access.Resolution{Allowed: true, TeamCodes: []string{"1K-IBMI-S1"}})
	got, ok := access.From(ctx)
	require.True(t, ok)
	require.True(t, got.HasTeam("1K-IBMI-S1"))
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
}
