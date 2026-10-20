package access

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
)

// AdminScopeOf returns the admin scope of a person, or ok=false when they have no web role
// assignment. It runs inside the caller's tenant transaction (q), so the identity module can
// use it in the web login gate without a second transaction.
func AdminScopeOf(ctx context.Context, q *db.Queries, personnelID int64) (AdminScope, bool, error) {
	row, err := q.GetWebRole(ctx, personnelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminScope{}, false, nil
	}
	if err != nil {
		return AdminScope{}, false, fmt.Errorf("get web role: %w", err)
	}
	return scopeOf(row.Role, row.RegionCode.String), true, nil
}

func scopeOf(role db.WebRole, regionCode string) AdminScope {
	if role == db.WebRoleORGADMIN {
		return AdminScope{All: true}
	}
	return AdminScope{RegionCode: regionCode}
}

// RequireAdmin is the middleware of the /api/v1/admin group: it loads the caller's web role
// assignment, refuses everyone without one with 403 NOT_ADMIN, and puts the AdminScope in
// the context for the usecases to filter by.
//
// ⚠ It is mounted ONCE, on the whole group (cmd/api), not route by route: a route someone
// adds under /api/v1/admin is protected without anyone having to remember it. A guard test
// walks every admin route and requires 403 for a person who is not an admin.
//
// It runs after auth.Require, which has already proved who the caller is. The assignment is
// read on every request and never taken from the token, so a revoked role stops working at
// the next call.
func RequireAdmin(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.From(r.Context())
			if !ok {
				// Unreachable behind auth.Require; refuse rather than guess who is asking.
				httpx.Error(w, r, apperr.ErrUnauthenticated)
				return
			}
			var scope AdminScope
			var isAdmin bool
			err := database.WithTenant(r.Context(), pool, p.OrganizationID, func(q *db.Queries) (err error) {
				scope, isAdmin, err = AdminScopeOf(r.Context(), q, p.PersonnelID)
				return err
			})
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !isAdmin {
				httpx.Error(w, r, ErrNotAdmin)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAdminScope(r.Context(), scope)))
		})
	}
}
