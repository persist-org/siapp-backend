package main

import (
	"context"

	"siapp/internal/db"
	"siapp/pkg/notifications"
	"siapp/pkg/organization"
	"siapp/pkg/platform/auth"
)

// notifier adapts notifications.Service to the small interfaces the modules that notify depend
// on (organization.Notifier, identity.PositionNotifier), so none of them imports notifications.
type notifier struct {
	service *notifications.Service
}

var _ organization.Notifier = notifier{}

// MemberMoved sends TEAM_CHANGED to the person who was moved.
func (n notifier) MemberMoved(ctx context.Context, c organization.MemberChange) {
	n.service.Notify(ctx, notifications.Input{
		Type:         notifications.TypeTeamChanged,
		RecipientIDs: []int64{c.PersonnelID},
		Vars:         notifications.Vars{BusinessLine: notifications.BusinessLine(c.BusinessLine), TeamName: c.TeamCode},
		Data:         map[string]any{"team_code": c.TeamCode},
		// The admin who moved them knows.
		ExcludePersonnelID: c.ActorID,
	})
}

// MemberRoleChanged sends POSITION_CHANGED for a change of role in a regu: the manual counts a
// DANRU becoming a member (or the reverse) as a change of position, like Personel's own.
func (n notifier) MemberRoleChanged(ctx context.Context, c organization.MemberChange) {
	n.service.Notify(ctx, notifications.Input{
		Type:         notifications.TypePositionChanged,
		RecipientIDs: []int64{c.PersonnelID},
		Vars: notifications.Vars{
			BusinessLine: notifications.BusinessLine(c.BusinessLine), TeamName: c.TeamCode, TeamRole: db.TeamRole(c.Role),
		},
		Data:               map[string]any{"team_code": c.TeamCode},
		ExcludePersonnelID: c.ActorID,
	})
}

// PositionChanged sends POSITION_CHANGED for a position Personel changed (identity.PositionNotifier).
// A webhook has no signed-in caller, and Notify takes the organization from the context, so the
// context gets a principal that holds only the organization.
func (n notifier) PositionChanged(ctx context.Context, organizationID, personnelID int64, positionName string) {
	ctx = auth.WithPrincipal(ctx, auth.Principal{OrganizationID: organizationID})
	n.service.Notify(ctx, notifications.Input{
		Type:         notifications.TypePositionChanged,
		RecipientIDs: []int64{personnelID},
		Vars:         notifications.Vars{PositionName: positionName},
	})
}
