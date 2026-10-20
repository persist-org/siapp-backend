package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// WebRole is the caller's web role as /auth/me shows it. A person with no role is field
// staff, and Me.WebRole is nil.
type WebRole struct {
	Role string
	// RegionCode is the koordinator wilayah of a REGION_ADMIN; empty for an ORG_ADMIN.
	RegionCode string
}

// Me is what the clients build their menus and profile from (DL-52).
type Me struct {
	Person           Person
	OrganizationName string
	WebRole          *WebRole
}

// Me returns the signed-in person, their organization and their web role.
//
// It never returns the device token or its hash: the binding is an admin's record about the
// phone, not something a client needs to read back. The NIK is masked like everywhere else.
//
// The memberships of the person's teams arrive with Phase 02.2 and the field permissions with
// Phase 03.1; until then the response lists no memberships and no permissions.
func (u *Usecase) Me(ctx context.Context, p auth.Principal) (Me, error) {
	var out Me
	err := database.WithTenant(ctx, u.pool, p.OrganizationID, func(q *db.Queries) error {
		person, err := u.repo.getPersonnel(ctx, q, p.PersonnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			// A token for a person who is not in the cache any more is not a valid sign-in.
			return apperr.ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		org, err := q.GetOrganization(ctx, p.OrganizationID)
		if err != nil {
			return fmt.Errorf("get organization: %w", err)
		}
		scope, isAdmin, err := access.AdminScopeOf(ctx, q, p.PersonnelID)
		if err != nil {
			return err
		}
		out = Me{Person: personFrom(person), OrganizationName: org.Name}
		if isAdmin {
			role := &WebRole{Role: access.RoleOrgAdmin}
			if !scope.All {
				role = &WebRole{Role: access.RoleRegionAdmin, RegionCode: scope.RegionCode}
			}
			out.WebRole = role
		}
		return nil
	})
	return out, err
}
