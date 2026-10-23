package access

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Grant is one permission of a role and the scope it is given.
type Grant struct {
	Permission Permission
	Scope      Scope
}

// RoleGrants is every grant of one team role, one per permission of the catalog, in the
// catalog's order.
type RoleGrants struct {
	TeamRole TeamRole
	Grants   []Grant
}

// Matrix is what the admin screen shows: the catalog, and the grants of both roles.
type Matrix struct {
	Catalog []Entry
	Roles   []RoleGrants
}

// FieldPermissions lets an ORG_ADMIN read and edit the permission matrix of the field roles.
// It is the web admin's side of Phase 03; the features read the matrix through
// EffectivePermissions and (Phase 03.2) Resolve.
type FieldPermissions struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewFieldPermissions returns the FieldPermissions usecase.
func NewFieldPermissions(pool *pgxpool.Pool, c clock.Clock) *FieldPermissions {
	return &FieldPermissions{pool: pool, clock: c}
}

// List returns the catalog and the matrix of both roles. Only an ORG_ADMIN may: the matrix
// decides what every person in the organization may do, so a REGION_ADMIN, who is limited to
// one region, may not see or change it.
func (f *FieldPermissions) List(ctx context.Context, actor auth.Principal, scope AdminScope) (Matrix, error) {
	if !scope.All {
		return Matrix{}, ErrOrgAdminOnly
	}
	var out Matrix
	err := database.WithTenant(ctx, f.pool, actor.OrganizationID, func(q *db.Queries) error {
		if _, err := EnsureDefaultsTx(ctx, q, f.clock.Now()); err != nil {
			return err
		}
		rows, err := q.ListRolePermissions(ctx)
		if err != nil {
			return fmt.Errorf("list role permissions: %w", err)
		}
		stored := map[TeamRole]map[Permission]Scope{}
		for _, r := range rows {
			role := TeamRole(r.TeamRole)
			if stored[role] == nil {
				stored[role] = map[Permission]Scope{}
			}
			stored[role][Permission(r.Permission)] = Scope(r.Scope)
		}
		out = Matrix{Catalog: Catalog()}
		for _, role := range teamRoles {
			out.Roles = append(out.Roles, RoleGrants{TeamRole: role, Grants: grantsInCatalogOrder(stored[role])})
		}
		return nil
	})
	return out, err
}

// Replace sets every grant of one role, in one transaction, and returns the grants as they
// are now. grants must name every permission of the catalog exactly once: this is a
// replacement, so a body that leaves a permission out is refused instead of being read as
// "take it away", and a short payload can never strip a role of what it was not told about.
//
// An unknown permission is PERMISSION_UNKNOWN, any other fault of the list is a
// VALIDATION_ERROR; both say which grant in errors[]. Nothing is written for a refused list.
//
// Only an ORG_ADMIN may. The grants that change are audited with their old and new scope; a
// replacement that changes nothing writes nothing, not even an audit row.
func (f *FieldPermissions) Replace(ctx context.Context, actor auth.Principal, scope AdminScope, role TeamRole, grants []Grant) (RoleGrants, error) {
	if !scope.All {
		return RoleGrants{}, ErrOrgAdminOnly
	}
	if !IsTeamRole(role) {
		return RoleGrants{}, apperr.ErrNotFound
	}
	if err := checkGrants(grants); err != nil {
		return RoleGrants{}, err
	}

	var out RoleGrants
	err := database.WithTenant(ctx, f.pool, actor.OrganizationID, func(q *db.Queries) error {
		// RequireAdmin read the role in an earlier transaction; another ORG_ADMIN may have
		// removed it since.
		if err := lockAndRecheckActor(ctx, q, actor.PersonnelID, actor.PersonnelID); err != nil {
			return err
		}
		now := f.clock.Now()
		if _, err := EnsureDefaultsTx(ctx, q, now); err != nil {
			return err
		}
		locked, err := q.LockRolePermissionsOfRole(ctx, db.TeamRole(role))
		if err != nil {
			return fmt.Errorf("lock role permissions: %w", err)
		}
		current := make(map[Permission]Scope, len(locked))
		for _, r := range locked {
			current[Permission(r.Permission)] = Scope(r.Scope)
		}

		var changed []Grant
		var changes []map[string]any
		for _, g := range grants {
			before, present := current[g.Permission]
			if present && before == g.Scope {
				continue
			}
			if !present {
				before = ScopeNone
			}
			changed = append(changed, g)
			changes = append(changes, map[string]any{"permission": string(g.Permission), "before": string(before), "after": string(g.Scope)})
			current[g.Permission] = g.Scope
		}
		if len(changed) > 0 {
			permissions, scopes := make([]string, len(changed)), make([]string, len(changed))
			for i, g := range changed {
				permissions[i], scopes[i] = string(g.Permission), string(g.Scope)
			}
			if _, err := q.UpsertRolePermissions(ctx, db.UpsertRolePermissionsParams{
				TeamRole: db.TeamRole(role), Now: pgtype.Timestamptz{Time: now, Valid: true}, Permissions: permissions, Scopes: scopes,
			}); err != nil {
				return fmt.Errorf("upsert role permissions: %w", err)
			}
			actorID := actor.PersonnelID
			if err := audit.Record(ctx, q, audit.Entry{
				ActorPersonnelID: &actorID, Action: "field_permissions.replace", EntityType: "field_permissions", EntityCode: string(role),
				Metadata: map[string]any{"team_role": string(role), "changes": changes},
			}); err != nil {
				return err
			}
		}
		out = RoleGrants{TeamRole: role, Grants: grantsInCatalogOrder(current)}
		return nil
	})
	return out, err
}

// grantsInCatalogOrder lists one grant per permission of the catalog. A permission with no
// stored scope is NONE: it is not seeded yet, and nothing is granted without a row.
func grantsInCatalogOrder(stored map[Permission]Scope) []Grant {
	out := make([]Grant, 0, len(catalog))
	for _, e := range catalog {
		scope, ok := stored[e.Key]
		if !ok {
			scope = ScopeNone
		}
		out = append(out, Grant{Permission: e.Key, Scope: scope})
	}
	return out
}

// checkGrants checks a replacement list before anything is written: every permission is
// known, appears once, has a scope of the matrix, and no permission of the catalog is left out.
func checkGrants(grants []Grant) error {
	var fields []apperr.FieldError
	unknown := false
	seen := make(map[Permission]bool, len(grants))
	for i, g := range grants {
		field := fmt.Sprintf("grants[%d]", i)
		switch {
		case !IsKnown(g.Permission):
			unknown = true
			fields = append(fields, apperr.FieldError{Field: field + ".permission", Code: "UNKNOWN", Message: "Hak akses tidak dikenal."})
		case seen[g.Permission]:
			fields = append(fields, apperr.FieldError{Field: field + ".permission", Code: "DUPLICATE", Message: "Hak akses ini sudah ada di daftar."})
		default:
			seen[g.Permission] = true
		}
		if !IsScope(g.Scope) {
			fields = append(fields, apperr.FieldError{Field: field + ".scope", Code: "INVALID", Message: "Harus NONE, SELF, TEAM, atau PROJECT."})
		}
	}
	for _, e := range catalog {
		if !seen[e.Key] {
			fields = append(fields, apperr.FieldError{Field: "grants", Code: "REQUIRED", Message: "Hak akses " + string(e.Key) + " wajib ada di daftar."})
		}
	}
	switch {
	case len(fields) == 0:
		return nil
	case unknown:
		return ErrPermissionUnknown.WithFields(fields...)
	default:
		return apperr.ErrValidation.WithFields(fields...)
	}
}
