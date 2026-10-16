package identity

import (
	"context"
	"fmt"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// PlacementLookup says at which projects a person is placed. Module 02 (Phase 02.2)
// implements it from the placement data; until then nobody is placed anywhere, so a
// PROJECT_ADMIN can reach nobody, which fails closed.
type PlacementLookup interface {
	ProjectIDsOf(ctx context.Context, q *db.Queries, personnelID int64) ([]int64, error)
}

type noPlacements struct{}

func (noPlacements) ProjectIDsOf(context.Context, *db.Queries, int64) ([]int64, error) {
	return nil, nil
}

// UsePlacements replaces the default (nobody is placed) with the real placement data.
func (u *Usecase) UsePlacements(p PlacementLookup) { u.placements = p }

// ResetDeviceBinding frees the phone slot of a person: the admin's answer to a lost or
// replaced phone. An ORG_ADMIN may reset anyone in the organization; a PROJECT_ADMIN only
// people placed at one of their projects.
//
// ⚠ This is SiAPP's own reset, separate from the one in the Personalia admin panel on purpose:
// HR's reset only frees the Personalia app (the integration login never touches Personalia's
// device table, Phase 00.5), so it does not reach SiAPP's binding.
//
// In ONE transaction it deletes the binding, revokes the person's MOBILE sessions and deletes
// their push-token devices; the revoked sessions are then put on the revocation list, so the
// old phone is out at once and not after 15 minutes. The person signs in again on the phone
// they now use, which binds it. A person without a binding is a no-op, not an error.
func (u *Usecase) ResetDeviceBinding(ctx context.Context, actor auth.Principal, scope access.AdminScope, target int64) error {
	now := u.clock.Now()
	var cutOff []int64
	err := database.WithTenant(ctx, u.pool, actor.OrganizationID, func(q *db.Queries) error {
		// ⚠ The per-person lock first (see the LockPersonnel query): a refresh or login of the
		// same person in flight commits before the reset reads the sessions, so the session it
		// creates is revoked too, and the two cannot deadlock on the binding.
		found, err := u.repo.lockPersonnel(ctx, q, target)
		if err != nil {
			return err
		}
		if !found {
			return apperr.ErrNotFound
		}
		if !scope.All {
			projects, err := u.placements.ProjectIDsOf(ctx, q, target)
			if err != nil {
				return fmt.Errorf("placements of personnel: %w", err)
			}
			if !scope.AllowsAnyProject(projects) {
				return access.ErrOutsideAdminScope
			}
		}

		deleted, ids, err := u.repo.resetBinding(ctx, q, target, now)
		if err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}
		cutOff = ids
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "device_binding.reset", EntityType: "device_binding", EntityID: target,
		})
	})
	if err != nil {
		return err
	}
	u.revocations.Revoke(ctx, cutOff...)
	return nil
}
