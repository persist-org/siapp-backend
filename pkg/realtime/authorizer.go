package realtime

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Authorizer decides which rooms a person may join. It is asked at every subscribe, and reads
// the database each time: a person moved out of a regu stops being let into its rooms at once.
type Authorizer struct {
	pool   *pgxpool.Pool
	access *access.Service
	clock  clock.Clock
}

// NewAuthorizer returns the room authorizer.
func NewAuthorizer(pool *pgxpool.Pool, svc *access.Service, c clock.Clock) *Authorizer {
	return &Authorizer{pool: pool, access: svc, clock: c}
}

// Allow reports whether p may join room. An error means the answer could not be found, which is
// not a "no": the caller says "unavailable" so the client retries.
//
// ⚠ Two rules, both here and nowhere else:
//
//  1. The organization is always p's own, from the token. A room name carries none, and the hub
//     keys every room by organization, so even a name copied from another tenant resolves to
//     this tenant's room (or to nothing) and the checks below run under this tenant's
//     row-level security: another organization's team code is simply not there.
//  2. Who may join what:
//     - team.{code}.panic: a person who is a member of that regu today.
//     - team.{code}.tracking: the regu's DANRU, that is, a person whose role grants
//     team.positions.view for that regu. An ordinary member only receives the regu's panics,
//     not the map of everyone else.
//     - project.{code}.panic and .tracking: web admins whose scope covers the project (an
//     ORG_ADMIN, or the REGION_ADMIN of the region the project sits under).
//     - personnel.{id}.notifications: only that person.
func (a *Authorizer) Allow(ctx context.Context, p auth.Principal, room Room) (bool, error) {
	allowed, err := a.AllowedRooms(ctx, p, []Room{room})
	return allowed[room.String()], err
}

// AllowedRooms asks Allow for several rooms in one transaction and returns the names of the
// rooms that are allowed. The server uses it to re-check the rooms a connection already joined.
func (a *Authorizer) AllowedRooms(ctx context.Context, p auth.Principal, rooms []Room) (map[string]bool, error) {
	out := make(map[string]bool, len(rooms))
	var needDB []Room
	for _, room := range rooms {
		if room.Scope == ScopePersonnel {
			out[room.String()] = room.Subject == strconv.FormatInt(p.PersonnelID, 10)
		} else {
			needDB = append(needDB, room)
		}
	}
	if len(needDB) == 0 {
		return out, nil
	}
	err := database.WithTenant(ctx, a.pool, p.OrganizationID, func(q *db.Queries) error {
		var tracking *access.Resolution // resolved once, for every tracking room
		for _, room := range needDB {
			var allowed bool
			var err error
			switch {
			case room.Scope == ScopeTeam && room.Channel == ChannelPanic:
				allowed, err = q.IsCurrentTeamMember(ctx, db.IsCurrentTeamMemberParams{
					TeamCode: room.Subject, PersonnelID: p.PersonnelID,
					Now: pgtype.Timestamptz{Time: a.clock.Now(), Valid: true},
				})
			case room.Scope == ScopeTeam && room.Channel == ChannelTracking:
				if tracking == nil {
					res, err := a.access.ResolveToday(ctx, q, p.PersonnelID, access.TeamPositionsView)
					if err != nil {
						return err
					}
					tracking = &res
				}
				allowed = tracking.HasTeam(room.Subject)
			case room.Scope == ScopeProject:
				allowed, err = a.adminCoversProject(ctx, q, p.PersonnelID, room.Subject)
			}
			if err != nil {
				return err
			}
			out[room.String()] = allowed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("realtime: authorize rooms: %w", err)
	}
	return out, nil
}

func (a *Authorizer) adminCoversProject(ctx context.Context, q *db.Queries, personnelID int64, projectCode string) (bool, error) {
	scope, isAdmin, err := access.AdminScopeOf(ctx, q, personnelID)
	if err != nil || !isAdmin {
		return false, err
	}
	if !scope.AllowsRegion(access.RegionOfCode(projectCode)) {
		return false, nil
	}
	return q.ProjectExists(ctx, projectCode)
}
