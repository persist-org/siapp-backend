package leave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// ReasonAlreadyDecided is returned for a request that cannot take the decision any more.
const ReasonAlreadyDecided = "LEAVE_ALREADY_DECIDED"

// ErrAlreadyDecided is returned when the request is not in a state the decider may change: a DANRU
// only decides a waiting request, and REJECTED and CANCELLED are final.
var ErrAlreadyDecided = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonAlreadyDecided, "Pengajuan ini sudah diputuskan.")

// DecideInput is the body of an approval or a rejection.
type DecideInput struct {
	// Note is required to reject (non-blank, 1000 characters at most) and optional to approve, where
	// it is kept.
	Note string `json:"note" validate:"omitempty,max=1000"`
}

// SkippedDay is a leave day that was not written because the person had already checked in.
type SkippedDay struct {
	WorkDate timeutil.Date
	TeamCode string
}

// level is who is deciding.
type level = db.LeaveDecidedLevel

const (
	levelDanru = db.LeaveDecidedLevelDANRU
	levelAdmin = db.LeaveDecidedLevelADMIN
)

// decision is one decision, whoever takes it.
type decision struct {
	level   level
	approve bool
	note    string
}

// ApproveAsDanru, DeclineAsDanru: the DANRU of the request's regu, through attendance.leave.approve
// (TEAM scope), checked live for today in the project's timezone.

// ApproveByDanru approves a waiting request of a regu the caller leads.
func (s *Service) ApproveByDanru(ctx context.Context, p auth.Principal, id int64, in DecideInput) (Request, error) {
	return s.decideByDanru(ctx, p, id, decision{level: levelDanru, approve: true, note: strings.TrimSpace(in.Note)})
}

// DeclineByDanru rejects a waiting request of a regu the caller leads. The note is required.
func (s *Service) DeclineByDanru(ctx context.Context, p auth.Principal, id int64, in DecideInput) (Request, error) {
	return s.decideByDanru(ctx, p, id, decision{level: levelDanru, note: strings.TrimSpace(in.Note)})
}

// ApproveByAdmin approves a waiting request within the admin's reach.
func (s *Service) ApproveByAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, in DecideInput) (Request, error) {
	return s.decideByAdmin(ctx, p, scope, id, decision{level: levelAdmin, approve: true, note: strings.TrimSpace(in.Note)})
}

// DeclineByAdmin rejects a request within the admin's reach, one the DANRU approved included.
func (s *Service) DeclineByAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, in DecideInput) (Request, error) {
	return s.decideByAdmin(ctx, p, scope, id, decision{level: levelAdmin, note: strings.TrimSpace(in.Note)})
}

func (s *Service) decideByDanru(ctx context.Context, p auth.Principal, id int64, d decision) (Request, error) {
	return s.decide(ctx, p, id, d, func(ctx context.Context, q *db.Queries, lr db.LeaveRequest) error {
		if lr.PersonnelID == p.PersonnelID {
			return access.ErrPermissionDenied // nobody decides their own request
		}
		if s.perms == nil {
			return access.ErrPermissionDenied
		}
		_, loc, err := clientAndZone(ctx, q, lr.TeamCode)
		if err != nil {
			return err
		}
		res, err := s.perms.Resolve(ctx, q, p.PersonnelID, access.AttendanceLeaveApprove, timeutil.BusinessDate(s.clock.Now(), loc))
		if err != nil {
			return err
		}
		if !res.HasTeam(lr.TeamCode) {
			return access.ErrPermissionDenied
		}
		return nil
	})
}

func (s *Service) decideByAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, d decision) (Request, error) {
	return s.decide(ctx, p, id, d, func(_ context.Context, _ *db.Queries, lr db.LeaveRequest) error {
		if !scope.AllowsRegion(access.RegionOfCode(lr.ProjectCode)) {
			return ErrNotFound // outside their region: the same answer as a request that is not there
		}
		if lr.PersonnelID == p.PersonnelID {
			return access.ErrPermissionDenied
		}
		return nil
	})
}

// decide takes a decision in one transaction. allowed is the caller's authority over the request,
// asked after the request is read and before anything changes.
//
// The owner's row is locked first (the same order submit and cancel take, so none of them can
// deadlock on the ledger), then the request, whose status is read AFTER the lock: two decisions at
// once take turns, and the second finds the first's result (KPI approval.go:125-305).
//
// Transitions. A DANRU decides a waiting request only. An admin decides a waiting request, and
// may reject one the DANRU approved (KPI's HR rejecting a supervisor-approved request); nothing
// else: REJECTED and CANCELLED are final.
func (s *Service) decide(ctx context.Context, p auth.Principal, id int64, d decision, allowed func(context.Context, *db.Queries, db.LeaveRequest) error) (Request, error) {
	if !d.approve && d.note == "" {
		return Request{}, validationError("note", "Alasan penolakan wajib diisi.")
	}
	var (
		notice  *Notice
		skipped []SkippedDay
	)
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		seen, err := q.GetLeaveRequest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave request: %w", err)
		}
		if err := allowed(ctx, q, seen); err != nil {
			return err
		}
		if _, err := q.LockPersonnel(ctx, seen.PersonnelID); err != nil {
			return fmt.Errorf("lock personnel: %w", err)
		}
		lr, err := q.GetLeaveRequestForUpdate(ctx, id)
		if err != nil {
			return fmt.Errorf("lock leave request: %w", err)
		}
		if err := checkTransition(lr, d); err != nil {
			return err
		}
		wasApproved := lr.Status == StatusApproved

		status := StatusRejected
		if d.approve {
			status = StatusApproved
		}
		now := s.clock.Now()
		decided, err := q.DecideLeaveRequest(ctx, db.DecideLeaveRequestParams{
			ID: id, Status: status, DecidedLevel: db.NullLeaveDecidedLevel{LeaveDecidedLevel: d.level, Valid: true}, DecidedByID: pgInt8(p.PersonnelID), Now: ts(now), Note: textOrNull(d.note),
		})
		if err != nil {
			return fmt.Errorf("decide leave request: %w", err)
		}
		catRow, err := q.GetLeaveCategoryByID(ctx, decided.CategoryID)
		if err != nil {
			return fmt.Errorf("get leave category: %w", err)
		}
		cat := toCategory(catRow)
		from, to := dateValue(decided.StartDate), dateValue(decided.EndDate)

		var eventType string
		switch {
		case d.approve:
			if cat.MakesLeaveDays {
				if skipped, err = s.writeLeaveDays(ctx, q, p.OrganizationID, now, decided); err != nil {
					return err
				}
			}
			if err := RecomputeLateness(ctx, q, now, decided.PersonnelID, from, to); err != nil {
				return err
			}
			eventType = eventApproved
		default:
			// ⚠ Rejected: annual days come back (once: Refund is idempotent); and when the request had
			// been approved its LEAVE rows go and the lateness it excused comes back.
			if cat.IsAnnual {
				if _, err := Refund(ctx, q, now, decided.PersonnelID, id, "Penolakan "+decided.Number); err != nil {
					return err
				}
			}
			if wasApproved {
				// A guard back from a revoked leave no longer needs the replacer a DANRU assigned:
				// cancel the unstarted swaps before their records go.
				if err := q.CancelUnstartedSwapsOfLeaveRequest(ctx, db.CancelUnstartedSwapsOfLeaveRequestParams{LeaveRequestID: pgInt8(id), Now: ts(now)}); err != nil {
					return fmt.Errorf("cancel swaps: %w", err)
				}
				if _, err := q.DeleteLeaveAttendancesOfRequest(ctx, pgInt8(id)); err != nil {
					return fmt.Errorf("delete leave attendances: %w", err)
				}
				if err := q.DetachLeaveAttendancesOfRequest(ctx, pgInt8(id)); err != nil {
					return fmt.Errorf("detach leave attendances: %w", err)
				}
				if err := RecomputeLateness(ctx, q, now, decided.PersonnelID, from, to); err != nil {
					return err
				}
				eventType = eventRevoked
			}
		}
		if eventType != "" {
			if err := writeLeaveEvent(ctx, q, decided, cat, eventType, now); err != nil {
				return err
			}
		}

		actorID := p.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.request." + strings.ToLower(string(status)), EntityType: "leave_request", EntityID: id,
			Metadata: map[string]any{"number": decided.Number, "level": string(d.level), "was_approved": wasApproved, "skipped_days": len(skipped)},
		}); err != nil {
			return err
		}
		notice, err = s.noticeFor(ctx, q, NoticeDecided, decided, d.approve)
		return err
	})
	if err != nil {
		return Request{}, err
	}
	s.notify(ctx, notice)

	out, err := s.getManaged(ctx, p, id)
	if err != nil {
		return Request{}, err
	}
	out.Skipped = skipped
	return out, nil
}

// checkTransition is the state machine (see decide).
func checkTransition(lr db.LeaveRequest, d decision) error {
	switch lr.Status {
	case StatusPending:
		return nil
	case StatusApproved:
		// Only an admin overruling a DANRU's approval, and only to reject it.
		if d.level == levelAdmin && !d.approve && lr.DecidedLevel.Valid && lr.DecidedLevel.LeaveDecidedLevel == levelDanru {
			return nil
		}
	case StatusRejected, StatusCancelled:
	}
	return ErrAlreadyDecided
}

// writeLeaveDays writes the LEAVE rows of an approved request: one for each scheduled working shift
// of the person in the range, in every regu they serve in that day (a person in two regu has two
// instances). A record that is already PRESENT stays and is reported as skipped: the guard came to
// work, and the day cannot be both. Nothing is written for a day with no schedule (nothing was
// expected of the person). The rows are SiAPP's own record: Personel learns of the leave by the
// LEAVE event (writeLeaveEvent), not by an event per row.
func (s *Service) writeLeaveDays(ctx context.Context, q *db.Queries, organizationID int64, now time.Time, lr db.LeaveRequest) ([]SkippedDay, error) {
	if s.schedule == nil {
		return nil, nil
	}
	instances, err := s.schedule.Instances(ctx, organizationID, lr.PersonnelID, dateValue(lr.StartDate), dateValue(lr.EndDate))
	if err != nil {
		return nil, fmt.Errorf("schedule instances: %w", err)
	}
	var skipped []SkippedDay
	for _, inst := range instances {
		if inst.Off {
			continue
		}
		for _, teamCode := range inst.TeamCodes {
			team, err := q.GetTeam(ctx, teamCode)
			if err != nil {
				return nil, fmt.Errorf("get team %s: %w", teamCode, err)
			}
			_, err = q.UpsertLeaveAttendance(ctx, db.UpsertLeaveAttendanceParams{
				PersonnelID: lr.PersonnelID, TeamCode: teamCode, ProjectCode: team.ProjectCode, WorkDate: dateParam(inst.Date),
				ShiftID: inst.ShiftID, ShiftStartsAt: ts(inst.StartsAt), ShiftEndsAt: ts(inst.EndsAt), Timezone: inst.Timezone,
				LeaveRequestID: pgInt8(lr.ID), Now: ts(now),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				skipped = append(skipped, SkippedDay{WorkDate: inst.Date, TeamCode: teamCode})
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("upsert leave attendance: %w", err)
			}
		}
	}
	return skipped, nil
}

// The events Personel receives for a leave.
const (
	eventApproved = "leave.approved"
	eventRevoked  = "leave.revoked"
)

// leaveEvent is what personel_outbox.payload holds for a leave. Like an attendance event it names the
// person by id, not by NIK: the delivery (07.3) adds the NIK from the person as they are then.
type leaveEvent struct {
	Type         string  `json:"type"`
	RequestID    int64   `json:"leave_request_id"`
	Number       string  `json:"number"`
	PersonnelID  int64   `json:"personnel_id"`
	TeamCode     string  `json:"team_code"`
	ProjectCode  string  `json:"project_code"`
	LeaveType    string  `json:"leave_type"`
	CategoryCode string  `json:"category_code"`
	CategoryName string  `json:"category_name"`
	StartDate    string  `json:"start_date"`
	EndDate      string  `json:"end_date"`
	StartTime    *string `json:"start_time"`
	EndTime      *string `json:"end_time"`
	WorkingDays  int     `json:"working_days"`
	OccurredAt   string  `json:"occurred_at"`
}

// writeLeaveEvent writes the Personel outbox row of a leave, in the transaction of the decision.
//
// ⚠ This is the ONLY way Personel learns of a leave now (D21): a request is SiAPP's, and nothing
// is forwarded as a request. Approving writes leave.approved; rejecting a request that had been
// approved writes leave.revoked; no other change writes one (a rejected waiting request, or a
// cancel, was never reported). aggregate_id is the request, so its events stay in order.
func writeLeaveEvent(ctx context.Context, q *db.Queries, lr db.LeaveRequest, cat Category, eventType string, now time.Time) error {
	payload, err := json.Marshal(leaveEvent{
		Type: eventType, RequestID: lr.ID, Number: lr.Number, PersonnelID: lr.PersonnelID, TeamCode: lr.TeamCode,
		ProjectCode: lr.ProjectCode, LeaveType: string(lr.Type), CategoryCode: cat.Code, CategoryName: cat.Name,
		StartDate: dateValue(lr.StartDate).String(), EndDate: dateValue(lr.EndDate).String(),
		StartTime: clockOf(lr.StartTime), EndTime: clockOf(lr.EndTime), WorkingDays: int(lr.WorkingDays),
		OccurredAt: timeutil.FormatTimestamp(now),
	})
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	if _, err := q.InsertPersonelOutbox(ctx, db.InsertPersonelOutboxParams{
		Kind: db.OutboxKindLEAVE, AggregateID: lr.ID, Payload: payload, Now: ts(now),
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return nil
}
