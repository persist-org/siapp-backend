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
	// Memberships are the regu the person serves in today, empty for an admin who is not in one.
	Memberships []Membership
	// Permissions are the field permissions the person holds today, for the app's menus. Empty
	// (never nil) when they serve in no regu.
	Permissions []access.Effective
}

// Me returns the signed-in person, their organization and their web role.
//
// It never returns the device token or its hash: the binding is an admin's record about the
// phone, not something a client needs to read back. The NIK is masked like everywhere else.
//
// The memberships are the regu the person serves in today (module 02). The permissions are
// what the matrix of their roles gives them through those memberships (access.EffectivePermissions):
// the app shows or hides menus with them, and every feature still checks its own permission.
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
		memberships, err := u.memberships.MembershipsOf(ctx, q, p.PersonnelID)
		if err != nil {
			return fmt.Errorf("memberships of personnel: %w", err)
		}
		out = Me{Person: personFrom(person), OrganizationName: org.Name, Memberships: memberships, Permissions: []access.Effective{}}
		if len(memberships) > 0 {
			// An organization whose people signed in before the matrix existed has no rows yet:
			// give it its defaults now, or the first /auth/me after the upgrade would hide every
			// menu. A call that finds the rows there writes nothing.
			if _, err := access.EnsureDefaultsTx(ctx, q, u.clock.Now()); err != nil {
				return err
			}
			held := make([]access.Held, 0, len(memberships))
			for _, m := range memberships {
				held = append(held, access.Held{Role: access.TeamRole(m.Role), Line: access.BusinessLine(m.BusinessLine)})
			}
			if out.Permissions, err = access.EffectivePermissions(ctx, q, held); err != nil {
				return err
			}
		}
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
