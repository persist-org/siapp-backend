package access

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// EnsureDefaults gives an organization the default grants of the catalog, in a transaction of
// its own, and returns how many rows it added. It runs when an organization is first seen
// (identity.Usecase.Login, after the organization row exists) and from `cmd/seed permissions`.
//
// It is a separate transaction on purpose: login's own transaction holds the person's row
// lock, and the rows this inserts have nothing to do with the person.
func EnsureDefaults(ctx context.Context, pool *pgxpool.Pool, c clock.Clock, organizationID int64) (int64, error) {
	var added int64
	err := database.WithTenant(ctx, pool, organizationID, func(q *db.Queries) error {
		var err error
		added, err = EnsureDefaultsTx(ctx, q, c.Now())
		return err
	})
	return added, err
}

// EnsureDefaultsTx is EnsureDefaults inside the tenant transaction of q. Admin screens and
// /auth/me call it first, so an organization whose people signed in before the matrix existed
// gets its rows the first time anyone reads them, and never shows an empty matrix.
//
// ⚠ Only the (role, permission) pairs without a row are inserted (INSERT … ON CONFLICT DO
// NOTHING). Existing rows are never overwritten, so an admin's edit survives a re-seed, and a
// permission the catalog gains later reaches organizations that were seeded before. "Not
// allowed" is a stored NONE, never a missing row, or the next seed would hand the permission
// back to a role the admin took it from.
//
// A call that finds every row there writes nothing.
func EnsureDefaultsTx(ctx context.Context, q *db.Queries, now time.Time) (int64, error) {
	keys := make([]string, 0, len(catalog))
	for _, e := range catalog {
		keys = append(keys, string(e.Key))
	}
	have, err := q.CountRolePermissionsOf(ctx, keys)
	if err != nil {
		return 0, fmt.Errorf("count role permissions: %w", err)
	}
	want := len(catalog) * len(teamRoles)
	if have >= int64(want) {
		return 0, nil
	}

	// The catalog's order, role by role inside a key: every caller inserts in this order, so
	// two organizations' first logins at once wait on the same row and never on each other's.
	roles, permissions, scopes := make([]string, 0, want), make([]string, 0, want), make([]string, 0, want)
	for _, e := range catalog {
		for _, role := range teamRoles {
			roles = append(roles, string(role))
			permissions = append(permissions, string(e.Key))
			scopes = append(scopes, string(e.Defaults[role]))
		}
	}
	added, err := q.InsertDefaultRolePermissions(ctx, db.InsertDefaultRolePermissionsParams{
		Now: pgtype.Timestamptz{Time: now, Valid: true}, TeamRoles: roles, Permissions: permissions, Scopes: scopes,
	})
	if err != nil {
		return 0, fmt.Errorf("insert default role permissions: %w", err)
	}
	return added, nil
}

// Held is one regu membership of a person, as far as grants are concerned: the role they hold
// in it and the business line of its project.
type Held struct {
	Role TeamRole
	Line BusinessLine
}

// Effective is one permission a person holds today, at its widest scope.
type Effective struct {
	Permission Permission
	Scope      Scope
}

// EffectivePermissions returns what a person may do, from the memberships they hold today
// (the caller reads them in each project's own timezone). It is what /auth/me shows, so the
// app can hide the menus a person may not use. It never grants anything by itself: the
// features check their own permission when they are called (Phase 03.2).
//
// For every permission of the catalog it takes the widest scope any membership gives:
//   - a membership grants what its role's row says (NONE, or a missing row, grants nothing);
//   - ⚠ a permission limited to a business line counts only through memberships in a project
//     of that line, so a DANRU of a security project does not get E-Memo from a cleaning
//     membership's grant, and a cleaning member does not get it from a security one;
//   - PROJECT beats TEAM, TEAM beats SELF.
//
// The result is in the catalog's order, holds only permissions with a scope, and is never nil.
func EffectivePermissions(ctx context.Context, q *db.Queries, held []Held) ([]Effective, error) {
	if len(held) == 0 {
		return []Effective{}, nil
	}
	rows, err := q.ListRolePermissions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list role permissions: %w", err)
	}
	grants := make(map[TeamRole]map[Permission]Scope, len(teamRoles))
	for _, r := range rows {
		role := TeamRole(r.TeamRole)
		if grants[role] == nil {
			grants[role] = map[Permission]Scope{}
		}
		grants[role][Permission(r.Permission)] = Scope(r.Scope)
	}
	return mergeGrants(held, grants), nil
}

// mergeGrants is EffectivePermissions without the database.
func mergeGrants(held []Held, grants map[TeamRole]map[Permission]Scope) []Effective {
	out := make([]Effective, 0, len(catalog))
	for _, e := range catalog {
		best := ScopeNone
		for _, h := range held {
			if !e.appliesTo(h.Line) {
				continue
			}
			best = Widest(best, grants[h.Role][e.Key])
		}
		if best != ScopeNone {
			out = append(out, Effective{Permission: e.Key, Scope: best})
		}
	}
	return out
}
