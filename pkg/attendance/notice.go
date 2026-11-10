package attendance

import (
	"context"
	"fmt"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// Notifications of module 07 (05's types ABSENCE_MARKED and SWAP_ASSIGNED). Same shape as
// pkg/leave's: the notice is built inside the transaction of the action (names and business line
// are read there), and sent after it committed, so a notification exists only for an action that
// happened and can never fail it.

// NoticeKind is what a notice is about.
type NoticeKind string

const (
	// NoticeAbsenceMarked goes to the person a DANRU marked absent. ActorName is the DANRU.
	NoticeAbsenceMarked NoticeKind = "ABSENCE_MARKED"
	// NoticeSwapAssigned goes to both people of a swap, each told the other's name in ActorName.
	NoticeSwapAssigned NoticeKind = "SWAP_ASSIGNED"
)

// Notice is a notification to send.
type Notice struct {
	Kind         NoticeKind
	RecipientIDs []int64
	// ActorName is the person the text names: the DANRU of an absence, the counterpart of a swap.
	ActorName    string
	TeamCode     string
	BusinessLine string
	// Date is the work date the notice is about.
	Date timeutil.Date
	// EntityType and EntityID are what opening the notification leads to.
	EntityType string
	EntityID   int64
}

// Notifier sends notices. cmd/api adapts notifications.Service to it.
type Notifier interface {
	NotifyAttendance(ctx context.Context, n Notice)
}

// UseNotifier plugs in the sender of notices. Called once at start-up; without one, nothing is sent.
func (s *Service) UseNotifier(n Notifier) { s.notifier = n }

// notify sends the notices, after the transaction that caused them has committed.
func (s *Service) notify(ctx context.Context, notices ...Notice) {
	if s.notifier == nil {
		return
	}
	for _, n := range notices {
		if len(n.RecipientIDs) > 0 {
			s.notifier.NotifyAttendance(ctx, n)
		}
	}
}

// noticeBase fills what every notice has: the business line of the project (the texts' labels follow
// it) and the date and regu. Zero value when no notifier is plugged in, so a deployment without one
// pays no queries.
func (s *Service) noticeBase(ctx context.Context, q *db.Queries, kind NoticeKind, teamCode, projectCode string, date timeutil.Date) (Notice, error) {
	n := Notice{Kind: kind, TeamCode: teamCode, Date: date}
	if s.notifier == nil {
		return n, nil
	}
	if line, err := q.GetBusinessLineOfProject(ctx, projectCode); err == nil {
		n.BusinessLine = string(line)
	}
	return n, nil
}

// personName is a person's display name for a notice text, "" when they are gone.
func (s *Service) personName(ctx context.Context, q *db.Queries, id int64) (string, error) {
	name, err := q.GetLeavePersonName(ctx, id)
	if err != nil {
		return "", fmt.Errorf("get person name: %w", err)
	}
	return name, nil
}
