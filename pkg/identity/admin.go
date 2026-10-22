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
	"siapp/pkg/platform/timeutil"
)

// RegionLookup says under which koordinator wilayah a person has an active regu membership.
// Module 02 (team_members) implements it; without it nobody belongs to a regu, so a
// REGION_ADMIN can reach nobody, which fails closed.
type RegionLookup interface {
	RegionCodesOf(ctx context.Context, q *db.Queries, personnelID int64) ([]string, error)
}

// Membership is one regu a person serves in today, as /auth/me lists it.
type Membership struct {
	TeamCode    string
	TeamName    string
	ProjectCode string
	ProjectName string
	// BusinessLine is the line of the project ("security" or "cleaning"). Permissions that
	// belong to one line (E-Memo) are counted only through memberships of that line.
	BusinessLine string
	Role         string
	StartDate    timeutil.Date
}

// MembershipLookup lists the regu a person serves in today (each in its project's own
// timezone). Module 02 implements it.
type MembershipLookup interface {
	MembershipsOf(ctx context.Context, q *db.Queries, personnelID int64) ([]Membership, error)
}

type noMemberships struct{}

func (noMemberships) RegionCodesOf(context.Context, *db.Queries, int64) ([]string, error) {
	return nil, nil
}

func (noMemberships) MembershipsOf(context.Context, *db.Queries, int64) ([]Membership, error) {
	return nil, nil
}

// UseRegionLookup replaces the default (nobody is in a regu) with the real membership data.
func (u *Usecase) UseRegionLookup(l RegionLookup) { u.regions = l }

// UseMembershipLookup replaces the default (nobody is in a regu) with the real membership data.
func (u *Usecase) UseMembershipLookup(l MembershipLookup) { u.memberships = l }

// ResetDeviceBinding frees the phone slot of a person: the admin's answer to a lost or
// replaced phone. An ORG_ADMIN may reset anyone in the organization; a REGION_ADMIN only
// people in a regu under their koordinator wilayah.
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
			// A REGION_ADMIN gets the same answer for a person who does not exist as for one
			// outside their region, so ids cannot be probed.
			if !scope.All {
				return access.ErrOutsideAdminScope
			}
			return apperr.ErrNotFound
		}
		if !scope.All {
			regions, err := u.regions.RegionCodesOf(ctx, q, target)
			if err != nil {
				return fmt.Errorf("regions of personnel: %w", err)
			}
			if !scope.AllowsAnyRegion(regions) {
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
