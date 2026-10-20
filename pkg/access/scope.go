// Package access decides what a signed-in person may do in SiAPP. Phase 01.3 starts it with
// the web admin gate; Phase 03 adds the field permission matrix.
//
// Authentication says who the caller is (package auth, from the token). Authorization says
// what they may do, and it is read fresh from the database on every request, never from the
// token: a role that is revoked takes effect on the next call, not after 15 minutes.
package access

import (
	"context"
	"slices"
)

// Web roles, as stored in web_role_assignments.role.
const (
	RoleOrgAdmin    = "ORG_ADMIN"
	RoleRegionAdmin = "REGION_ADMIN"
)

// AdminScope is what an admin may reach. Admin usecases filter by it: an ORG_ADMIN sees the
// whole organization (All), a REGION_ADMIN only what sits under their koordinator wilayah
// (RegionCode, such as "OS1"), across all three PTs of the group (D18).
//
// Everything below a region (kantor cabang, klien, proyek, regu) is reached through its
// region, so a usecase for one of those resolves the region of the row and asks AllowsRegion.
type AdminScope struct {
	All        bool
	RegionCode string
}

// AllowsRegion reports whether the scope covers a koordinator wilayah. An ORG_ADMIN covers
// every region; a REGION_ADMIN only their own.
func (s AdminScope) AllowsRegion(code string) bool {
	return s.All || (s.RegionCode != "" && s.RegionCode == code)
}

// AllowsAnyRegion reports whether the scope covers at least one of the regions. An ORG_ADMIN
// covers everything, including a person who belongs to no region at all (they lead no regu
// yet); a REGION_ADMIN covers only people in a regu under their region.
func (s AdminScope) AllowsAnyRegion(codes []string) bool {
	if s.All {
		return true
	}
	return slices.ContainsFunc(codes, s.AllowsRegion)
}

type scopeKey struct{}

// WithAdminScope returns ctx carrying the scope. RequireAdmin does this; tests of code that
// only reads the scope may too.
func WithAdminScope(ctx context.Context, s AdminScope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom returns the admin scope RequireAdmin put in ctx. ok is false outside an admin
// route.
func ScopeFrom(ctx context.Context) (s AdminScope, ok bool) {
	s, ok = ctx.Value(scopeKey{}).(AdminScope)
	return s, ok
}
