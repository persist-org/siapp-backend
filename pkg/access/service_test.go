package access_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
	"siapp/pkg/platform/timeutil"
)

// serviceNow is 12:00 on 2026-03-10 in Jakarta.
var serviceNow = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

func day(t *testing.T, s string) timeutil.Date {
	t.Helper()
	d, err := timeutil.ParseDate(s)
	require.NoError(t, err)
	return d
}

// The hierarchy the tests share, in every organization they build one for:
//
//	OS1  1K  1K-IBMI  1K-IBMI-S (security, Jakarta)  S1  S2  S3 (inactive)
//	                  1K-IBMI-C (cleaning, Jakarta)  C1
//	         1K-ABCD  1K-ABCD-S (security, Jakarta)  S1
//	     1I  1I-PUA   1I-PUA-S  (security, Jayapura) S1
//	OS2  2K  2K-QQQQ  2K-QQQQ-S (security, Jakarta)  S1
const worldSQL = `
INSERT INTO regions (code, description) VALUES ('OS1', 'Outsourcing 1'), ('OS2', 'Outsourcing 2');
INSERT INTO branches (code, region_code, company_letter, description) VALUES
  ('1K', 'OS1', 'K', 'Cabang 1K'), ('1I', 'OS1', 'I', 'Cabang 1I'), ('2K', 'OS2', 'K', 'Cabang 2K');
INSERT INTO clients (code, branch_code, description, company_prefix) VALUES
  ('1K-IBMI', '1K', 'IBMI', 'IBMI'), ('1K-ABCD', '1K', 'ABCD', 'ABCD'), ('1I-PUA', '1I', 'Papua', 'PUA'), ('2K-QQQQ', '2K', 'QQQQ', 'QQQQ');
INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES
  ('1K-IBMI-S', '1K-IBMI', 'IBMI keamanan', 'security', 'Asia/Jakarta'),
  ('1K-IBMI-C', '1K-IBMI', 'IBMI kebersihan', 'cleaning', 'Asia/Jakarta'),
  ('1K-ABCD-S', '1K-ABCD', 'ABCD keamanan', 'security', 'Asia/Jakarta'),
  ('1I-PUA-S', '1I-PUA', 'Papua keamanan', 'security', 'Asia/Jayapura'),
  ('2K-QQQQ-S', '2K-QQQQ', 'QQQQ keamanan', 'security', 'Asia/Jakarta');
INSERT INTO teams (code, project_code, description, active) VALUES
  ('1K-IBMI-S1', '1K-IBMI-S', 'IBMI S1', true), ('1K-IBMI-S2', '1K-IBMI-S', 'IBMI S2', true),
  ('1K-IBMI-S3', '1K-IBMI-S', 'IBMI S3 nonaktif', false), ('1K-IBMI-C1', '1K-IBMI-C', 'IBMI C1', true),
  ('1K-ABCD-S1', '1K-ABCD-S', 'ABCD S1', true), ('1I-PUA-S1', '1I-PUA-S', 'Papua S1', true),
  ('2K-QQQQ-S1', '2K-QQQQ-S', 'QQQQ S1', true);`

// world builds the hierarchy in an organization and seeds its permission matrix.
func (e *env) world(org int64) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(e.t.Context(), worldSQL)
		require.NoError(e.t, err)
	})
	_, err := access.EnsureDefaults(e.t.Context(), testkit.Pool(e.t), clock.System{}, org)
	require.NoError(e.t, err)
}

// member puts a person in a regu with a role. end "" means "until further notice".
func (e *env) member(org int64, team string, person int64, role, start, end string) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(e.t.Context(),
			"INSERT INTO team_members (team_code, personnel_id, role, start_date, end_date) VALUES ($1, $2, $3::team_role, $4::date, NULLIF($5, '')::date)",
			team, person, role, start, end)
		require.NoError(e.t, err)
	})
}

func (e *env) exec(org int64, query string, args ...any) {
	e.t.Helper()
	e.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(e.t.Context(), query, args...)
		require.NoError(e.t, err)
	})
}

func (e *env) setScope(org int64, role access.TeamRole, perm access.Permission, scope access.Scope) {
	e.t.Helper()
	e.exec(org, "UPDATE role_permissions SET scope = $3::permission_scope WHERE team_role = $1::team_role AND permission = $2", role, perm, scope)
}

// newWorld is a new organization with the hierarchy and the seeded matrix.
func (e *env) newWorld() int64 {
	e.t.Helper()
	org := testkit.NewOrg(e.t)
	e.world(org)
	return org
}

func (e *env) field(org int64) int64 {
	e.t.Helper()
	_, id := e.personWithID(org, "")
	return id
}

// resolve runs Resolve in the organization's tenant, at the fixed clock.
func (e *env) resolve(org, person int64, perm access.Permission, on string) (access.Resolution, error) {
	e.t.Helper()
	return e.resolveAt(clock.Fixed{T: serviceNow}, org, person, perm, on)
}

// resolveAt is resolve with a chosen clock; on "" asks for "today" (ResolveToday).
func (e *env) resolveAt(c clock.Clock, org, person int64, perm access.Permission, on string) (res access.Resolution, err error) {
	e.t.Helper()
	svc := access.NewService(testkit.Pool(e.t), c)
	err = database.WithTenant(e.t.Context(), testkit.Pool(e.t), org, func(q *db.Queries) error {
		var rerr error
		if on == "" {
			res, rerr = svc.ResolveToday(e.t.Context(), q, person, perm)
		} else {
			res, rerr = svc.Resolve(e.t.Context(), q, person, perm, day(e.t, on))
		}
		return rerr
	})
	return res, err
}

func (e *env) mustResolve(org, person int64, perm access.Permission, on string) access.Resolution {
	e.t.Helper()
	res, err := e.resolve(org, person, perm, on)
	require.NoError(e.t, err)
	return res
}

func TestResolve(t *testing.T) {
	e := newEnv(t)
	const on = "2026-03-10"

	t.Run("a member of one team", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "")
		res := e.mustResolve(org, p, access.ReportView, on) // MEMBER: TEAM
		require.True(t, res.Allowed)
		require.False(t, res.SelfOnly)
		require.Equal(t, []string{"1K-IBMI-S1"}, res.TeamCodes)
		require.Empty(t, res.ProjectCodes)
		require.True(t, res.HasTeam("1K-IBMI-S1"))
		require.False(t, res.HasTeam("1K-IBMI-S2"), "TEAM reaches the team of the membership only")
	})

	t.Run("a DANRU of two teams in two projects: PROJECT reaches every active team of both", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "DANRU", "2026-01-01", "")
		e.member(org, "1K-ABCD-S1", p, "DANRU", "2026-01-01", "")
		res := e.mustResolve(org, p, access.ReportView, on) // DANRU: PROJECT
		require.True(t, res.Allowed)
		require.False(t, res.SelfOnly)
		require.Equal(t, []string{"1K-ABCD-S", "1K-IBMI-S"}, res.ProjectCodes)
		require.Equal(t, []string{"1K-ABCD-S1", "1K-IBMI-S1", "1K-IBMI-S2"}, res.TeamCodes,
			"S1 and S2 of one project and S1 of the other; the inactive S3 and the cleaning project's C1 are not there")
		require.True(t, res.HasProject("1K-IBMI-S"))
		require.False(t, res.HasProject("1K-IBMI-C"), "another business line of the same client is another project")
		require.False(t, res.HasTeam("1K-IBMI-S3"), "an inactive team is not added by PROJECT")
		require.False(t, res.HasTeam("1K-IBMI-C1"))
		require.False(t, res.HasTeam("2K-QQQQ-S1"))
	})

	t.Run("a team that is deleted drops out of the expansion", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "DANRU", "2026-01-01", "")
		e.exec(org, "UPDATE teams SET deleted_at = now() WHERE code = '1K-IBMI-S2'")
		require.Equal(t, []string{"1K-IBMI-S1"}, e.mustResolve(org, p, access.ReportView, on).TeamCodes)
	})

	t.Run("a person's own team is in scope even when it is inactive", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S3", p, "DANRU", "2026-01-01", "")
		res := e.mustResolve(org, p, access.ReportView, on)
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S2", "1K-IBMI-S3"}, res.TeamCodes, "they still serve there; PROJECT adds the active ones")
	})

	t.Run("a SELF-only grant", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "")
		res := e.mustResolve(org, p, access.AttendanceCheckin, on) // MEMBER: SELF
		require.True(t, res.Allowed)
		require.True(t, res.SelfOnly)
		require.Empty(t, res.TeamCodes, "SELF reaches no team")
		require.Empty(t, res.ProjectCodes)
		require.NotNil(t, res.TeamCodes)
		require.NotNil(t, res.ProjectCodes)
	})

	t.Run("SelfOnly is false as soon as one grant is wider than SELF", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "") // panic.finish: MEMBER SELF
		e.member(org, "1K-ABCD-S1", p, "DANRU", "2026-01-01", "")  // panic.finish: DANRU TEAM
		res := e.mustResolve(org, p, access.PanicFinish, on)
		require.True(t, res.Allowed)
		require.False(t, res.SelfOnly)
		require.Equal(t, []string{"1K-ABCD-S1"}, res.TeamCodes, "the SELF membership adds no team")
	})

	t.Run("no grant means not allowed", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "")
		res := e.mustResolve(org, p, access.AttendanceLeaveApprove, on) // MEMBER: NONE
		require.False(t, res.Allowed)
		require.False(t, res.SelfOnly)
		require.Empty(t, res.TeamCodes)
		require.NotNil(t, res.TeamCodes)

		require.False(t, e.mustResolve(org, e.field(org), access.AttendanceCheckin, on).Allowed, "a person in no team holds nothing")
	})

	t.Run("an ended membership is ignored, and the end date is inclusive", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "2026-03-09")
		require.False(t, e.mustResolve(org, p, access.ReportView, on).Allowed, "ended yesterday")
		require.True(t, e.mustResolve(org, p, access.ReportView, "2026-03-09").Allowed, "it still counts on its last day")
		require.False(t, e.mustResolve(org, p, access.ReportView, "2026-03-10").Allowed, "and not on the day after")
	})

	t.Run("a membership that has not started is ignored, and the start date is inclusive", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-03-11", "")
		require.False(t, e.mustResolve(org, p, access.ReportView, on).Allowed)
		require.False(t, e.mustResolve(org, p, access.ReportView, "2026-03-10").Allowed)
		require.True(t, e.mustResolve(org, p, access.ReportView, "2026-03-11").Allowed, "it counts from its first day")
	})

	t.Run("the date asked about decides, so a record of another day is judged by that day", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S1", p, "MEMBER", "2026-01-01", "2026-02-28")
		e.member(org, "1K-ABCD-S1", p, "MEMBER", "2026-03-01", "")
		require.Equal(t, []string{"1K-IBMI-S1"}, e.mustResolve(org, p, access.ReportView, "2026-02-15").TeamCodes)
		require.Equal(t, []string{"1K-ABCD-S1"}, e.mustResolve(org, p, access.ReportView, "2026-03-15").TeamCodes)
	})

	t.Run("a team or project that is deleted has no members", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-ABCD-S1", p, "MEMBER", "2026-01-01", "")
		e.exec(org, "UPDATE teams SET deleted_at = now() WHERE code = '1K-ABCD-S1'")
		require.False(t, e.mustResolve(org, p, access.ReportView, on).Allowed)
	})

	t.Run("what an admin changed in the matrix is what is read", func(t *testing.T) {
		org := e.newWorld()
		p := e.field(org)
		e.member(org, "1K-IBMI-S2", p, "MEMBER", "2026-01-01", "")
		require.False(t, e.mustResolve(org, p, access.AttendanceLeaveApprove, on).Allowed)
		e.setScope(org, access.RoleMember, access.AttendanceLeaveApprove, access.ScopeProject)
		res := e.mustResolve(org, p, access.AttendanceLeaveApprove, on)
		require.Equal(t, []string{"1K-IBMI-S1", "1K-IBMI-S2"}, res.TeamCodes)
		e.setScope(org, access.RoleMember, access.AttendanceLeaveApprove, access.ScopeNone)
		require.False(t, e.mustResolve(org, p, access.AttendanceLeaveApprove, on).Allowed, "NONE takes it away again")
	})

	t.Run("an organization that is not seeded yet grants nothing", func(t *testing.T) {
		bare := testkit.NewOrg(t)
		e.inTenant(bare, func(tx pgx.Tx) {
			_, err := tx.Exec(t.Context(), worldSQL)
			require.NoError(t, err)
		})
		p := e.field(bare)
		e.member(bare, "1K-IBMI-S1", p, "DANRU", "2026-01-01", "")
		require.False(t, e.mustResolve(bare, p, access.ReportView, on).Allowed, "no rows: fail closed")
	})

	t.Run("one organization never sees another's teams", func(t *testing.T) {
		org := e.newWorld()
		other := e.newWorld()
		mine := e.field(org)
		e.member(org, "1K-IBMI-S1", mine, "DANRU", "2026-01-01", "")
		theirs := e.field(other)
		e.member(other, "1K-IBMI-S1", theirs, "DANRU", "2026-01-01", "")
		e.exec(other, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S4', '1K-IBMI-S', 'only in the other org')")

		require.NotContains(t, e.mustResolve(org, mine, access.ReportView, on).TeamCodes, "1K-IBMI-S4")
		require.Contains(t, e.mustResolve(other, theirs, access.ReportView, on).TeamCodes, "1K-IBMI-S4")
		// A person asked about in the wrong organization is simply nobody there.
		res, err := e.resolve(other, mine, access.ReportView, on)
		require.NoError(t, err)
		require.False(t, res.Allowed)
	})

	t.Run("a permission that is not in the catalog is an error, not a denial", func(t *testing.T) {
		org := e.newWorld()
		_, err := e.resolve(org, e.field(org), "no.such.permission", on)
		require.Error(t, err)
		var appErr *apperr.Error
		require.False(t, errors.As(err, &appErr), "a mistake in the code, not something to tell the client")
	})

	t.Run("a missing date is an error", func(t *testing.T) {
		org := e.newWorld()
		svc := access.NewService(testkit.Pool(t), clock.System{})
		require.Error(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
			_, err := svc.Resolve(t.Context(), q, 1, access.ReportView, timeutil.Date{})
			return err
		}))
	})
}

func TestResolveHonorsTheBusinessLineOfAPermission(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)
	const on = "2026-03-10"

	security := e.field(org)
	e.member(org, "1K-IBMI-S1", security, "DANRU", "2026-01-01", "")
	cleaner := e.field(org)
	e.member(org, "1K-IBMI-C1", cleaner, "MEMBER", "2026-01-01", "")

	require.False(t, e.mustResolve(org, security, access.EMemoUse, on).Allowed, "E-Memo is for cleaning projects only")
	res := e.mustResolve(org, cleaner, access.EMemoUse, on)
	require.True(t, res.Allowed)
	require.Equal(t, []string{"1K-IBMI-C1"}, res.TeamCodes)

	t.Run("a wide grant through a security membership does not widen the cleaning one", func(t *testing.T) {
		both := e.field(org)
		e.member(org, "1K-IBMI-S2", both, "DANRU", "2026-01-01", "")
		e.member(org, "1K-IBMI-C1", both, "MEMBER", "2026-01-01", "")
		e.setScope(org, access.RoleDanru, access.EMemoUse, access.ScopeProject)
		e.setScope(org, access.RoleMember, access.EMemoUse, access.ScopeSelf)

		got := e.mustResolve(org, both, access.EMemoUse, on)
		require.True(t, got.Allowed)
		require.True(t, got.SelfOnly, "only the cleaning membership counts, and it is SELF")
		require.Empty(t, got.TeamCodes)

		onlyCleaning := e.mustResolve(org, cleaner, access.EMemoUse, on)
		require.True(t, onlyCleaning.Allowed)
		require.True(t, onlyCleaning.SelfOnly, "the edit made MEMBER's E-Memo SELF for everyone")
		require.Empty(t, onlyCleaning.TeamCodes)
	})
}

// ⚠ "Today" is each membership's own business date. 16:30 UTC is 23:30 in Jakarta on the 10th
// but already 01:30 in Jayapura on the 11th.
func TestResolveTodayIsTheDateOfEachProjectsOwnTimezone(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)
	at := clock.Fixed{T: time.Date(2026, 3, 10, 16, 30, 0, 0, time.UTC)}

	inJayapura := e.field(org)
	e.member(org, "1I-PUA-S1", inJayapura, "MEMBER", "2026-03-11", "")
	inJakarta := e.field(org)
	e.member(org, "1K-IBMI-S1", inJakarta, "MEMBER", "2026-03-11", "")

	got, err := e.resolveAt(at, org, inJayapura, access.ReportView, "")
	require.NoError(t, err)
	require.True(t, got.Allowed, "the 11th has begun in Jayapura")
	got, err = e.resolveAt(at, org, inJakarta, access.ReportView, "")
	require.NoError(t, err)
	require.False(t, got.Allowed, "it is still the 10th in Jakarta")

	t.Run("an end date is judged in the same timezone", func(t *testing.T) {
		ended := e.field(org)
		e.member(org, "1I-PUA-S1", ended, "MEMBER", "2026-01-01", "2026-03-10")
		stillIn := e.field(org)
		e.member(org, "1K-IBMI-S1", stillIn, "MEMBER", "2026-01-01", "2026-03-10")
		got, err := e.resolveAt(at, org, ended, access.ReportView, "")
		require.NoError(t, err)
		require.False(t, got.Allowed, "the 10th is over in Jayapura")
		got, err = e.resolveAt(at, org, stillIn, access.ReportView, "")
		require.NoError(t, err)
		require.True(t, got.Allowed, "the 10th is not over in Jakarta")
	})

	t.Run("an explicit date wins over the clock", func(t *testing.T) {
		got, err := e.resolveAt(at, org, inJakarta, access.ReportView, "2026-03-11")
		require.NoError(t, err)
		require.True(t, got.Allowed)
	})
}

func TestAssertTeam(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)
	svc := access.NewService(testkit.Pool(t), clock.Fixed{T: serviceNow})
	assert := func(person int64, perm access.Permission, team string) error {
		return database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
			return svc.AssertTeam(t.Context(), q, person, perm, team, day(t, "2026-03-10"))
		})
	}

	danru := e.field(org)
	e.member(org, "1K-IBMI-S1", danru, "DANRU", "2026-01-01", "")
	member := e.field(org)
	e.member(org, "1K-IBMI-S1", member, "MEMBER", "2026-01-01", "")

	require.NoError(t, assert(danru, access.AttendanceLeaveApprove, "1K-IBMI-S1"), "TEAM reaches their own team")
	require.ErrorIs(t, assert(danru, access.AttendanceLeaveApprove, "1K-IBMI-S2"), access.ErrPermissionDenied, "and not a sibling team")
	require.NoError(t, assert(danru, access.ReportView, "1K-IBMI-S2"), "PROJECT reaches the siblings")
	require.ErrorIs(t, assert(danru, access.ReportView, "1K-ABCD-S1"), access.ErrPermissionDenied, "and not another project")
	require.ErrorIs(t, assert(member, access.AttendanceLeaveApprove, "1K-IBMI-S1"), access.ErrPermissionDenied, "a member has no grant")
	require.ErrorIs(t, assert(member, access.AttendanceCheckin, "1K-IBMI-S1"), access.ErrPermissionDenied, "SELF reaches no team")
	require.ErrorIs(t, assert(danru, access.ReportView, "NO-SUCH-TEAM"), access.ErrPermissionDenied, "a team that does not exist is not in scope either")

	t.Run("a team of another organization is never in scope", func(t *testing.T) {
		other := testkit.NewOrg(t)
		e.world(other)
		e.exec(other, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S9', '1K-IBMI-S', 'theirs')")
		err := assert(danru, access.ReportView, "1K-IBMI-S9")
		require.ErrorIs(t, err, access.ErrPermissionDenied, "the same answer as for a team that does not exist")
	})

	t.Run("the denial is a 403 PERMISSION_DENIED", func(t *testing.T) {
		var appErr *apperr.Error
		require.ErrorAs(t, assert(member, access.AttendanceLeaveApprove, "1K-IBMI-S1"), &appErr)
		require.Equal(t, 403, appErr.Status)
		require.Equal(t, apperr.CodeForbidden, appErr.Code)
		require.Equal(t, access.ReasonPermissionDenied, appErr.Reason)
	})
}

func TestDanruOf(t *testing.T) {
	e := newEnv(t)
	org := testkit.NewOrg(t)
	e.world(org)
	svc := access.NewService(testkit.Pool(t), clock.Fixed{T: serviceNow})
	const on = "2026-03-10"
	danruOf := func(team string, sender int64, date string) ([]int64, error) {
		var out []int64
		err := database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
			var err error
			out, err = svc.DanruOf(t.Context(), q, team, sender, day(t, date))
			return err
		})
		return out, err
	}

	_, orgAdmin := e.personWithID(org, access.RoleOrgAdmin)
	_, regionAdmin := e.personWithID(org, access.RoleRegionAdmin) // OS1
	other := e.field(org)
	e.exec(org, "INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS2')", other)
	otherRegionAdmin := other

	danru := e.field(org)
	e.member(org, "1K-IBMI-S1", danru, "DANRU", "2026-01-01", "")
	member := e.field(org)
	e.member(org, "1K-IBMI-S1", member, "MEMBER", "2026-01-01", "")
	e.member(org, "1K-IBMI-S2", e.field(org), "MEMBER", "2026-01-01", "")

	t.Run("a member's report goes to the DANRU of the team", func(t *testing.T) {
		got, err := danruOf("1K-IBMI-S1", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{danru}, got)
	})

	t.Run("the DANRU is the sender: the project's admins instead, never the sender", func(t *testing.T) {
		got, err := danruOf("1K-IBMI-S1", danru, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, regionAdmin}, got, "every ORG_ADMIN and the REGION_ADMIN of OS1, not OS2's")
		require.NotContains(t, got, otherRegionAdmin)
		require.NotContains(t, got, danru)
	})

	t.Run("a team with no DANRU that day: the admins", func(t *testing.T) {
		got, err := danruOf("1K-IBMI-S2", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, regionAdmin}, got)
	})

	t.Run("the region of the project decides which REGION_ADMIN", func(t *testing.T) {
		e.member(org, "2K-QQQQ-S1", e.field(org), "MEMBER", "2026-01-01", "")
		got, err := danruOf("2K-QQQQ-S1", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, otherRegionAdmin}, got, "2K-QQQQ is under OS2")
	})

	t.Run("a DANRU who has not started or has ended is none that day", func(t *testing.T) {
		got, err := danruOf("1K-IBMI-S1", member, "2025-12-31")
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, regionAdmin}, got, "before the DANRU started")

		e.member(org, "1K-ABCD-S1", e.field(org), "MEMBER", "2026-01-01", "")
		later := e.field(org)
		e.member(org, "1K-ABCD-S1", later, "DANRU", "2026-03-11", "")
		got, err = danruOf("1K-ABCD-S1", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, regionAdmin}, got, "the DANRU starts tomorrow")
		got, err = danruOf("1K-ABCD-S1", member, "2026-03-11")
		require.NoError(t, err)
		require.Equal(t, []int64{later}, got)
	})

	t.Run("a DANRU Personalia has deactivated is none", func(t *testing.T) {
		gone := e.field(org)
		e.member(org, "1I-PUA-S1", gone, "DANRU", "2026-01-01", "")
		e.exec(org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = $1", gone)
		got, err := danruOf("1I-PUA-S1", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin, regionAdmin}, got)
	})

	t.Run("an admin who is no longer active is not told", func(t *testing.T) {
		e.exec(org, "UPDATE personnels SET status = 'INACTIVE' WHERE id = $1", regionAdmin)
		defer e.exec(org, "UPDATE personnels SET status = 'ACTIVE' WHERE id = $1", regionAdmin)
		got, err := danruOf("1K-IBMI-S2", member, on)
		require.NoError(t, err)
		require.Equal(t, []int64{orgAdmin}, got)
	})

	t.Run("a sender who is also an admin is left out of the fallback", func(t *testing.T) {
		got, err := danruOf("1K-IBMI-S2", orgAdmin, on)
		require.NoError(t, err)
		require.NotContains(t, got, orgAdmin)
	})

	t.Run("nobody to tell is an empty list, not an error", func(t *testing.T) {
		e.exec(org, "DELETE FROM web_role_assignments")
		got, err := danruOf("1K-IBMI-S2", member, on)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Empty(t, got)
	})

	t.Run("a team that does not exist, or is deleted, is NOT_FOUND", func(t *testing.T) {
		_, err := danruOf("1K-IBMI-S77", member, on)
		require.ErrorIs(t, err, apperr.ErrNotFound)
		e.exec(org, "UPDATE teams SET deleted_at = now() WHERE code = '1K-IBMI-S2'")
		_, err = danruOf("1K-IBMI-S2", member, on)
		require.ErrorIs(t, err, apperr.ErrNotFound)
	})

	t.Run("a team of another organization is NOT_FOUND", func(t *testing.T) {
		foreign := testkit.NewOrg(t)
		e.world(foreign)
		e.exec(foreign, "INSERT INTO teams (code, project_code, description) VALUES ('1K-IBMI-S8', '1K-IBMI-S', 'theirs')")
		_, err := danruOf("1K-IBMI-S8", member, on)
		require.ErrorIs(t, err, apperr.ErrNotFound)
	})

	t.Run("a missing date is an error", func(t *testing.T) {
		require.Error(t, database.WithTenant(t.Context(), testkit.Pool(t), org, func(q *db.Queries) error {
			_, err := svc.DanruOf(t.Context(), q, "1K-IBMI-S1", member, timeutil.Date{})
			return err
		}))
	})
}

func TestCodeHelpers(t *testing.T) {
	for code, want := range map[string]string{"1K-IBMI-S1": "OS1", "12K-IBMI-S1": "OS12", "1K": "OS1", "OS1": "", "": "", "K1": ""} {
		require.Equal(t, want, access.RegionOfCode(code), code)
	}
	for code, want := range map[string]string{"1K-IBMI-S1": "1K-IBMI-S", "1K-IBMI-C12": "1K-IBMI-C", "1K-IBMI-S": "", "": "", "123": ""} {
		require.Equal(t, want, access.ProjectCodeOfTeam(code), code)
	}
}
