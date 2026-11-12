package main

import (
	"context"

	"siapp/internal/db"
	"siapp/pkg/attendance"
	"siapp/pkg/leave"
	"siapp/pkg/notifications"
	"siapp/pkg/organization"
	"siapp/pkg/patrol"
	"siapp/pkg/platform/auth"
)

// notifier adapts notifications.Service to the small interfaces the modules that notify depend
// on (organization.Notifier, identity.PositionNotifier), so none of them imports notifications.
type notifier struct {
	service *notifications.Service
}

var (
	_ organization.Notifier = notifier{}
	_ leave.Notifier        = notifier{}
	_ attendance.Notifier   = notifier{}
	_ patrol.Notifier       = notifier{}
)

// NotifyLeave sends the notice of a leave request: LEAVE_REQUESTED and LEAVE_CANCELLED to who
// decides it, LEAVE_DECIDED to its owner (leave.Notifier).
func (n notifier) NotifyLeave(ctx context.Context, nt leave.Notice) {
	typ := notifications.TypeLeaveRequested
	switch nt.Kind {
	case leave.NoticeDecided:
		typ = notifications.TypeLeaveDecided
	case leave.NoticeCancelled:
		typ = notifications.TypeLeaveCancelled
	case leave.NoticeRequested:
	}
	n.service.Notify(ctx, notifications.Input{
		Type:         typ,
		RecipientIDs: nt.RecipientIDs,
		Vars: notifications.Vars{
			BusinessLine: notifications.BusinessLine(nt.BusinessLine), ActorName: nt.PersonName, TeamName: nt.TeamCode,
			Date: nt.Date, Approved: nt.Approved,
		},
		Entity: &notifications.Entity{Type: "LEAVE_REQUEST", ID: nt.RequestID},
		Data:   map[string]any{"leave_request_id": nt.RequestID, "team_code": nt.TeamCode},
	})
}

// NotifyAttendance sends ABSENCE_MARKED to the person a DANRU marked absent, and SWAP_ASSIGNED to
// each of the two people of a shift swap (attendance.Notifier).
func (n notifier) NotifyAttendance(ctx context.Context, nt attendance.Notice) {
	typ := notifications.TypeAbsenceMarked
	if nt.Kind == attendance.NoticeSwapAssigned {
		typ = notifications.TypeSwapAssigned
	}
	n.service.Notify(ctx, notifications.Input{
		Type:         typ,
		RecipientIDs: nt.RecipientIDs,
		Vars: notifications.Vars{
			BusinessLine: notifications.BusinessLine(nt.BusinessLine), ActorName: nt.ActorName, TeamName: nt.TeamCode, Date: nt.Date,
		},
		Entity: &notifications.Entity{Type: nt.EntityType, ID: nt.EntityID},
		Data:   map[string]any{"team_code": nt.TeamCode, "work_date": nt.Date.String()},
	})
}

// NotifyPatrol sends PATROL_SUBMITTED or GUARD_SUBMITTED (by the zone's type) to who decides a scan,
// and OFFLINE_SUBMISSION_SENT to the guard whose offline scan has arrived (patrol.Notifier).
func (n notifier) NotifyPatrol(ctx context.Context, nt patrol.Notice) {
	typ := notifications.TypePatrolSubmitted
	switch {
	case nt.Kind == patrol.NoticeOfflineSent:
		typ = notifications.TypeOfflineSubmissionSent
	case nt.ZoneType == patrol.TypeGuard:
		typ = notifications.TypeGuardSubmitted
	}
	n.service.Notify(ctx, notifications.Input{
		Type:         typ,
		RecipientIDs: nt.RecipientIDs,
		Vars: notifications.Vars{
			BusinessLine: notifications.BusinessLine(nt.BusinessLine), ActorName: nt.ActorName, TeamName: nt.TeamCode,
			Subject: nt.Subject, Date: nt.Date,
		},
		Entity: &notifications.Entity{Type: patrol.EntityScan, ID: nt.ScanID},
		Data:   map[string]any{"scan_id": nt.ScanID, "zone_id": nt.ZoneID, "team_code": nt.TeamCode, "work_date": nt.Date.String()},
	})
}

// MemberMoved sends TEAM_CHANGED to the person who was moved.
func (n notifier) MemberMoved(ctx context.Context, c organization.MemberChange) {
	n.service.Notify(ctx, notifications.Input{
		Type:         notifications.TypeTeamChanged,
		RecipientIDs: []int64{c.PersonnelID},
		Vars: notifications.Vars{
			BusinessLine: notifications.BusinessLine(c.BusinessLine), TeamName: c.TeamCode, Date: c.EffectiveDate,
		},
		Data: map[string]any{"team_code": c.TeamCode},
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
			Date: c.EffectiveDate,
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
