package access

import "slices"

// TeamRole is a person's role in a regu (DL-04). It is the key of the field permission
// matrix: grants belong to a role, not to a position.
type TeamRole string

// The two team roles, as stored in team_members.role and role_permissions.team_role.
const (
	RoleDanru  TeamRole = "DANRU"
	RoleMember TeamRole = "MEMBER"
)

// teamRoles is every role a grant can name, in the order the matrix lists them.
var teamRoles = []TeamRole{RoleDanru, RoleMember}

// TeamRoles returns every role a grant can name.
func TeamRoles() []TeamRole { return slices.Clone(teamRoles) }

// IsTeamRole reports whether r is a role of the matrix.
func IsTeamRole(r TeamRole) bool { return slices.Contains(teamRoles, r) }

// Scope says whose records a grant covers.
type Scope string

// The scopes of a grant, from nothing to the widest. ScopeNone is a stored value, not the
// absence of a row (see the role_permissions migration).
const (
	ScopeNone    Scope = "NONE"
	ScopeSelf    Scope = "SELF"
	ScopeTeam    Scope = "TEAM"
	ScopeProject Scope = "PROJECT"
)

// scopes is every scope in ascending width.
var scopes = []Scope{ScopeNone, ScopeSelf, ScopeTeam, ScopeProject}

// IsScope reports whether s is a scope of the matrix.
func IsScope(s Scope) bool { return slices.Contains(scopes, s) }

// width orders the scopes: NONE < SELF < TEAM < PROJECT. An unknown scope is the narrowest,
// so a value that should never be there can only ever grant less.
func (s Scope) width() int { return slices.Index(scopes, s) + 1 }

// Widest returns the wider of two scopes: PROJECT beats TEAM, TEAM beats SELF, SELF beats NONE.
func Widest(a, b Scope) Scope {
	if b.width() > a.width() {
		return b
	}
	return a
}

// BusinessLine is the line of business of a project. A permission may apply to one line only.
type BusinessLine string

// The business lines, as stored in projects.business_line.
const (
	LineSecurity BusinessLine = "security"
	LineCleaning BusinessLine = "cleaning"
)

// Permission is the key of one thing a field role may do.
type Permission string

// The permission keys of the catalog (manual Tabel 8, "List Hak Akses User").
const (
	AttendanceCheckin      Permission = "attendance.checkin"
	AttendanceQRDisplay    Permission = "attendance.qr.display"
	AttendanceLeaveRequest Permission = "attendance.leave.request"
	AttendanceLeaveApprove Permission = "attendance.leave.approve"
	AttendanceAbsenceNew   Permission = "attendance.absence.create"
	AttendanceAbsenceView  Permission = "attendance.absence.view"
	AttendanceGroupView    Permission = "attendance.group.view"
	AttendanceSwapCreate   Permission = "attendance.swap.create"
	ReportCreate           Permission = "report.create"
	ReportCreateDanruOnly  Permission = "report.create.danru_only"
	ReportView             Permission = "report.view"
	InstructionCreate      Permission = "instruction.create"
	TeamPositionsView      Permission = "team.positions.view"
	PatrolScan             Permission = "patrol.scan"
	PatrolApprove          Permission = "patrol.approve"
	GuardScan              Permission = "guard.scan"
	GuardApprove           Permission = "guard.approve"
	CCTVView               Permission = "cctv.view"
	PanicCreate            Permission = "panic.create"
	PanicView              Permission = "panic.view"
	PanicFinish            Permission = "panic.finish"
	AssetReport            Permission = "asset.report"
	EMemoUse               Permission = "ememo.use"
)

// Entry is one permission of the catalog.
type Entry struct {
	Key    Permission
	Module string
	// Description is the manual's row, in Indonesian: what an admin reads in the matrix.
	Description string
	// Defaults is the scope each role gets when an organization is first seen. Every role is
	// listed; ScopeNone means "not allowed".
	Defaults map[TeamRole]Scope
	// BusinessLines limits the permission to projects of these lines. Empty means every line:
	// the permission does not depend on the project's business.
	BusinessLines []BusinessLine
}

// defaults is the shorthand of the catalog below: the MEMBER scope, then the DANRU scope.
func defaults(member, danru Scope) map[TeamRole]Scope {
	return map[TeamRole]Scope{RoleMember: member, RoleDanru: danru}
}

// catalog is the table of docs/plan/03-access-control.md, in the order the matrix shows it.
// The order also fixes the order rows are inserted in (see EnsureDefaults).
var catalog = []Entry{
	{Key: AttendanceCheckin, Module: "attendance", Description: "Absen QR Code / Absen selfie", Defaults: defaults(ScopeSelf, ScopeSelf)},
	{Key: AttendanceQRDisplay, Module: "attendance", Description: "Tampil QR Code", Defaults: defaults(ScopeNone, ScopeTeam)},
	{Key: AttendanceLeaveRequest, Module: "attendance", Description: "Pengajuan izin", Defaults: defaults(ScopeSelf, ScopeSelf)},
	{Key: AttendanceLeaveApprove, Module: "attendance", Description: "Persetujuan izin", Defaults: defaults(ScopeNone, ScopeTeam)},
	{Key: AttendanceAbsenceNew, Module: "attendance", Description: "Buat mangkir", Defaults: defaults(ScopeNone, ScopeTeam)},
	{Key: AttendanceAbsenceView, Module: "attendance", Description: "Riwayat mangkir", Defaults: defaults(ScopeSelf, ScopeTeam)},
	{Key: AttendanceGroupView, Module: "attendance", Description: "Absensi group", Defaults: defaults(ScopeNone, ScopeProject)},
	{Key: AttendanceSwapCreate, Module: "attendance", Description: "Tukar jaga", Defaults: defaults(ScopeNone, ScopeProject)},
	{Key: ReportCreate, Module: "report", Description: "Buat laporan", Defaults: defaults(ScopeTeam, ScopeTeam)},
	{Key: ReportCreateDanruOnly, Module: "report", Description: "Kirim laporan DANRU only", Defaults: defaults(ScopeTeam, ScopeNone)},
	{Key: ReportView, Module: "report", Description: "Lihat laporan regu / per proyek", Defaults: defaults(ScopeTeam, ScopeProject)},
	{Key: InstructionCreate, Module: "instruction", Description: "Buat instruksi / kirim ke banyak regu/proyek", Defaults: defaults(ScopeNone, ScopeProject)},
	{Key: TeamPositionsView, Module: "team", Description: "Lihat posisi anggota (regu / per anggota / per proyek)", Defaults: defaults(ScopeNone, ScopeProject)},
	{Key: PatrolScan, Module: "patrol", Description: "Membuat laporan patroli", Defaults: defaults(ScopeTeam, ScopeTeam)},
	{Key: PatrolApprove, Module: "patrol", Description: "Approve laporan patroli", Defaults: defaults(ScopeNone, ScopeTeam)},
	{Key: GuardScan, Module: "guard", Description: "Membuat laporan jaga", Defaults: defaults(ScopeTeam, ScopeTeam)},
	{Key: GuardApprove, Module: "guard", Description: "Approve laporan jaga", Defaults: defaults(ScopeNone, ScopeTeam)},
	{Key: CCTVView, Module: "cctv", Description: "Mengakses CCTV", Defaults: defaults(ScopeProject, ScopeProject)},
	{Key: PanicCreate, Module: "panic", Description: "Membuat sinyal panik", Defaults: defaults(ScopeTeam, ScopeTeam)},
	{Key: PanicView, Module: "panic", Description: "Sinyal panik pribadi / grup / per proyek", Defaults: defaults(ScopeTeam, ScopeProject)},
	{Key: PanicFinish, Module: "panic", Description: "Menyelesaikan sinyal panik (DL-31)", Defaults: defaults(ScopeSelf, ScopeTeam)},
	{Key: AssetReport, Module: "asset", Description: "Laporan aset (live app, DL-43)", Defaults: defaults(ScopeTeam, ScopeTeam)},
	{Key: EMemoUse, Module: "ememo", Description: "E-Memo (cleaning only, module 16)", Defaults: defaults(ScopeTeam, ScopeTeam), BusinessLines: []BusinessLine{LineCleaning}},
}

// Catalog returns every permission, in the order the matrix shows them. The result is a copy:
// the catalog is fixed at build time and no caller can change it.
func Catalog() []Entry {
	out := make([]Entry, len(catalog))
	for i, e := range catalog {
		e.Defaults = cloneDefaults(e.Defaults)
		e.BusinessLines = slices.Clone(e.BusinessLines)
		out[i] = e
	}
	return out
}

func cloneDefaults(m map[TeamRole]Scope) map[TeamRole]Scope {
	out := make(map[TeamRole]Scope, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// entryOf finds a catalog entry by key. The result shares memory with the catalog: it is for
// reading inside this package only.
func entryOf(key Permission) (Entry, bool) {
	for _, e := range catalog {
		if e.Key == key {
			return e, true
		}
	}
	return Entry{}, false
}

// IsKnown reports whether key is a permission of the catalog.
func IsKnown(key Permission) bool {
	_, ok := entryOf(key)
	return ok
}

// appliesTo reports whether the permission may be used in a project of the line: an entry with
// no business lines applies to all of them.
func (e Entry) appliesTo(line BusinessLine) bool {
	return len(e.BusinessLines) == 0 || slices.Contains(e.BusinessLines, line)
}
