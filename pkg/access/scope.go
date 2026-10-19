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
	RoleOrgAdmin     = "ORG_ADMIN"
	RoleProjectAdmin = "PROJECT_ADMIN"
)

// AdminScope is what an admin may reach. Admin usecases filter by it: an ORG_ADMIN sees the
// whole organization (All), a PROJECT_ADMIN only the listed projects.
type AdminScope struct {
	All        bool
	ProjectIDs []int64
}

// AllowsAnyProject reports whether the scope covers at least one of the projects. An
// ORG_ADMIN covers everything, including a person placed at no project at all; a
// PROJECT_ADMIN covers only people placed at one of their projects.
func (s AdminScope) AllowsAnyProject(projectIDs []int64) bool {
	if s.All {
		return true
	}
	return slices.ContainsFunc(projectIDs, func(id int64) bool { return slices.Contains(s.ProjectIDs, id) })
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
