package access

import (
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
)

// manualTable is Tabel 8 as docs/plan/03-access-control.md prints it, typed in a second time
// on purpose: a typo in catalog.go must not pass because the test copied the same typo.
var manualTable = []struct {
	key           string
	member, danru Scope
	cleaning      bool
}{
	{"attendance.checkin", ScopeSelf, ScopeSelf, false},
	{"attendance.qr.display", ScopeNone, ScopeTeam, false},
	{"attendance.leave.request", ScopeSelf, ScopeSelf, false},
	{"attendance.leave.approve", ScopeNone, ScopeTeam, false},
	{"attendance.absence.create", ScopeNone, ScopeTeam, false},
	{"attendance.absence.view", ScopeSelf, ScopeTeam, false},
	{"attendance.group.view", ScopeNone, ScopeProject, false},
	{"attendance.swap.create", ScopeNone, ScopeProject, false},
	{"report.create", ScopeTeam, ScopeTeam, false},
	{"report.create.danru_only", ScopeTeam, ScopeNone, false},
	{"report.view", ScopeTeam, ScopeProject, false},
	{"instruction.create", ScopeNone, ScopeProject, false},
	{"team.positions.view", ScopeNone, ScopeProject, false},
	{"patrol.scan", ScopeTeam, ScopeTeam, false},
	{"patrol.approve", ScopeNone, ScopeTeam, false},
	{"guard.scan", ScopeTeam, ScopeTeam, false},
	{"guard.approve", ScopeNone, ScopeTeam, false},
	{"cctv.view", ScopeProject, ScopeProject, false},
	{"panic.create", ScopeTeam, ScopeTeam, false},
	{"panic.view", ScopeTeam, ScopeProject, false},
	{"panic.finish", ScopeSelf, ScopeTeam, false},
	{"asset.report", ScopeTeam, ScopeTeam, false},
	{"ememo.use", ScopeTeam, ScopeTeam, true},
}

func TestTheCatalogIsTheManualTable(t *testing.T) {
	got := Catalog()
	require.Len(t, got, len(manualTable))
	for i, want := range manualTable {
		e := got[i]
		require.Equal(t, want.key, string(e.Key), "row %d: the catalog keeps the manual's order", i)
		require.Equal(t, want.member, e.Defaults[RoleMember], "%s for MEMBER", want.key)
		require.Equal(t, want.danru, e.Defaults[RoleDanru], "%s for DANRU", want.key)
		if want.cleaning {
			require.Equal(t, []BusinessLine{LineCleaning}, e.BusinessLines, "%s is cleaning only", want.key)
		} else {
			require.Empty(t, e.BusinessLines, "%s applies to every business line", want.key)
		}
	}
}

func TestEveryCatalogEntryIsWellFormed(t *testing.T) {
	// The same shape as the role_permissions_key_check constraint: an entry the table would
	// refuse must fail here, not at the first login.
	shape := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	seen := map[Permission]bool{}
	for _, e := range Catalog() {
		require.Regexp(t, shape, string(e.Key))
		require.LessOrEqual(t, len(e.Key), 64, "permission is VARCHAR(64)")
		require.False(t, seen[e.Key], "%s is listed twice", e.Key)
		seen[e.Key] = true

		require.NotEmpty(t, e.Module, e.Key)
		require.NotEmpty(t, e.Description, e.Key)
		require.Len(t, e.Defaults, len(TeamRoles()), "%s has a default for every role and no other", e.Key)
		for _, role := range TeamRoles() {
			scope, ok := e.Defaults[role]
			require.True(t, ok, "%s has no default for %s", e.Key, role)
			require.True(t, IsScope(scope), "%s for %s: %q is not a scope", e.Key, role, scope)
		}
		for _, line := range e.BusinessLines {
			require.Contains(t, []BusinessLine{LineSecurity, LineCleaning}, line, e.Key)
		}
		require.True(t, IsKnown(e.Key))
	}
	require.False(t, IsKnown("no.such.permission"))
	require.False(t, IsKnown(""))
	require.False(t, IsKnown("ATTENDANCE.CHECKIN"), "keys are case sensitive")
}

func TestCatalogHandsOutCopies(t *testing.T) {
	first := Catalog()
	first[0].Key = "tampered.key"
	first[0].Defaults[RoleMember] = ScopeProject
	first[len(first)-1].BusinessLines[0] = LineSecurity

	again := Catalog()
	require.Equal(t, AttendanceCheckin, again[0].Key)
	require.Equal(t, ScopeSelf, again[0].Defaults[RoleMember])
	require.Equal(t, []BusinessLine{LineCleaning}, again[len(again)-1].BusinessLines)
	require.True(t, IsKnown(AttendanceCheckin))
	require.False(t, IsKnown("tampered.key"))

	roles := TeamRoles()
	roles[0] = "TAMPERED"
	require.Equal(t, []TeamRole{RoleDanru, RoleMember}, TeamRoles())
}

func TestWidestScope(t *testing.T) {
	tests := []struct{ a, b, want Scope }{
		{ScopeNone, ScopeSelf, ScopeSelf},
		{ScopeSelf, ScopeTeam, ScopeTeam},
		{ScopeTeam, ScopeProject, ScopeProject},
		{ScopeProject, ScopeTeam, ScopeProject},
		{ScopeTeam, ScopeSelf, ScopeTeam},
		{ScopeSelf, ScopeNone, ScopeSelf},
		{ScopeTeam, ScopeTeam, ScopeTeam},
		{ScopeNone, ScopeNone, ScopeNone},
		// A value that is not a scope grants nothing, whichever side it is on.
		{ScopeNone, "", ScopeNone},
		{"", ScopeSelf, ScopeSelf},
		{"BOGUS", ScopeNone, ScopeNone},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, Widest(tc.a, tc.b), "%q and %q", tc.a, tc.b)
	}
}

// matrix is the default matrix of the catalog, as mergeGrants reads it.
func matrix() map[TeamRole]map[Permission]Scope {
	out := map[TeamRole]map[Permission]Scope{}
	for _, e := range Catalog() {
		for role, scope := range e.Defaults {
			if out[role] == nil {
				out[role] = map[Permission]Scope{}
			}
			out[role][e.Key] = scope
		}
	}
	return out
}

func heldScope(got []Effective, key Permission) Scope {
	for _, e := range got {
		if e.Permission == key {
			return e.Scope
		}
	}
	return ScopeNone
}

func TestMergeGrants(t *testing.T) {
	t.Run("a member of one security team", func(t *testing.T) {
		got := mergeGrants([]Held{{RoleMember, LineSecurity}}, matrix())
		require.Equal(t, ScopeSelf, heldScope(got, AttendanceCheckin))
		require.Equal(t, ScopeTeam, heldScope(got, ReportView))
		require.Equal(t, ScopeProject, heldScope(got, CCTVView))
		require.Equal(t, ScopeNone, heldScope(got, AttendanceLeaveApprove), "NONE is not listed")
		require.Equal(t, ScopeNone, heldScope(got, EMemoUse), "E-Memo is cleaning only")
		for _, e := range got {
			require.NotEqual(t, ScopeNone, e.Scope, "%s: a permission without a scope is left out", e.Permission)
		}
	})

	t.Run("PROJECT beats TEAM beats SELF across memberships", func(t *testing.T) {
		got := mergeGrants([]Held{{RoleMember, LineSecurity}, {RoleDanru, LineSecurity}}, matrix())
		require.Equal(t, ScopeProject, heldScope(got, ReportView), "DANRU PROJECT over MEMBER TEAM")
		require.Equal(t, ScopeTeam, heldScope(got, AttendanceAbsenceView), "DANRU TEAM over MEMBER SELF")
		require.Equal(t, ScopeTeam, heldScope(got, ReportCreateDanruOnly), "the DANRU has none, the MEMBER has TEAM: the MEMBER's grant counts")
		require.Equal(t, ScopeSelf, heldScope(got, AttendanceCheckin))
	})

	t.Run("a business line permission counts only through a membership of that line", func(t *testing.T) {
		both := mergeGrants([]Held{{RoleMember, LineSecurity}, {RoleMember, LineCleaning}}, matrix())
		require.Equal(t, ScopeTeam, heldScope(both, EMemoUse))

		// ⚠ The wide grant of the security membership must not leak E-Memo into the cleaning one.
		grants := matrix()
		grants[RoleDanru][EMemoUse] = ScopeProject
		grants[RoleMember][EMemoUse] = ScopeSelf
		got := mergeGrants([]Held{{RoleDanru, LineSecurity}, {RoleMember, LineCleaning}}, grants)
		require.Equal(t, ScopeSelf, heldScope(got, EMemoUse), "only the cleaning membership counts")

		only := mergeGrants([]Held{{RoleDanru, LineSecurity}}, grants)
		require.Equal(t, ScopeNone, heldScope(only, EMemoUse))
	})

	t.Run("an admin's edit is what is read, not the default", func(t *testing.T) {
		grants := matrix()
		grants[RoleDanru][AttendanceLeaveApprove] = ScopeNone
		grants[RoleMember][AttendanceLeaveApprove] = ScopeProject
		require.Equal(t, ScopeNone, heldScope(mergeGrants([]Held{{RoleDanru, LineSecurity}}, grants), AttendanceLeaveApprove))
		require.Equal(t, ScopeProject, heldScope(mergeGrants([]Held{{RoleMember, LineSecurity}}, grants), AttendanceLeaveApprove))
	})

	t.Run("no row, no grant: an organization not seeded yet grants nothing", func(t *testing.T) {
		require.Empty(t, mergeGrants([]Held{{RoleDanru, LineSecurity}}, map[TeamRole]map[Permission]Scope{}))
	})

	t.Run("a role the matrix does not know grants nothing", func(t *testing.T) {
		require.Empty(t, mergeGrants([]Held{{"SUPERVISOR", LineSecurity}}, matrix()))
	})

	t.Run("the result follows the catalog's order, and is never nil", func(t *testing.T) {
		got := mergeGrants([]Held{{RoleDanru, LineCleaning}}, matrix())
		var order []Permission
		for _, e := range Catalog() {
			if heldScope(got, e.Key) != ScopeNone {
				order = append(order, e.Key)
			}
		}
		var listed []Permission
		for _, e := range got {
			listed = append(listed, e.Permission)
		}
		require.Equal(t, order, listed)
		require.NotNil(t, mergeGrants(nil, matrix()))
	})
}

// full is every permission of the catalog at one scope.
func full(scope Scope) []Grant {
	var out []Grant
	for _, e := range Catalog() {
		out = append(out, Grant{Permission: e.Key, Scope: scope})
	}
	return out
}

func fieldsOf(t *testing.T, err error) (reason string, fields map[string]string) {
	t.Helper()
	var appErr *apperr.Error
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, apperr.CodeValidation, appErr.Code)
	fields = map[string]string{}
	for _, f := range appErr.Fields {
		fields[f.Field+" "+f.Code] = f.Message
	}
	return appErr.Reason, fields
}

func TestCheckGrants(t *testing.T) {
	t.Run("every permission once, every scope allowed", func(t *testing.T) {
		for _, scope := range []Scope{ScopeNone, ScopeSelf, ScopeTeam, ScopeProject} {
			require.NoError(t, checkGrants(full(scope)), scope)
		}
	})

	t.Run("an unknown permission is PERMISSION_UNKNOWN, and says which", func(t *testing.T) {
		grants := full(ScopeSelf)
		grants[2].Permission = "attendance.leave.reqest"
		reason, fields := fieldsOf(t, checkGrants(grants))
		require.Equal(t, ReasonPermissionUnknown, reason)
		require.Contains(t, fields, "grants[2].permission UNKNOWN")
		// The misspelled one is also missing, which tells the admin what was meant.
		require.Contains(t, fields, "grants REQUIRED")
	})

	t.Run("a permission left out is refused, not read as taken away", func(t *testing.T) {
		reason, fields := fieldsOf(t, checkGrants(full(ScopeTeam)[1:]))
		require.Empty(t, reason, "a plain validation error")
		require.Len(t, fields, 1)
		require.Contains(t, fields["grants REQUIRED"], string(AttendanceCheckin))
	})

	t.Run("an empty list names every permission as missing", func(t *testing.T) {
		var appErr *apperr.Error
		require.ErrorAs(t, checkGrants([]Grant{}), &appErr)
		require.Len(t, appErr.Fields, len(manualTable))
		for i, e := range Catalog() {
			require.Equal(t, "grants", appErr.Fields[i].Field)
			require.Equal(t, "REQUIRED", appErr.Fields[i].Code)
			require.Contains(t, appErr.Fields[i].Message, string(e.Key))
		}
	})

	t.Run("a duplicate is refused, once per extra copy", func(t *testing.T) {
		grants := append(full(ScopeSelf), Grant{Permission: ReportView, Scope: ScopeProject})
		_, fields := fieldsOf(t, checkGrants(grants))
		require.Contains(t, fields, "grants["+strconv.Itoa(len(grants)-1)+"].permission DUPLICATE")
		require.Len(t, fields, 1)
	})

	t.Run("a scope that is not one is refused", func(t *testing.T) {
		for _, bad := range []Scope{"", "ALL", "project", "TEAMS"} {
			grants := full(ScopeSelf)
			grants[0].Scope = bad
			reason, fields := fieldsOf(t, checkGrants(grants))
			require.Empty(t, reason)
			require.Contains(t, fields, "grants[0].scope INVALID", "%q", bad)
		}
	})

	t.Run("keys are case sensitive", func(t *testing.T) {
		grants := full(ScopeSelf)
		grants[0].Permission = "ATTENDANCE.CHECKIN"
		reason, _ := fieldsOf(t, checkGrants(grants))
		require.Equal(t, ReasonPermissionUnknown, reason)
	})
}
