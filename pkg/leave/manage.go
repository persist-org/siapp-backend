package leave

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Person is who a request is about, for someone deciding it.
type Person struct {
	PersonnelID int64
	Name        string
	Position    string
}

// Overlap is another member of the regu who is away on a day of the request.
type Overlap struct {
	Name string
	From timeutil.Date
	To   timeutil.Date
}

// Coverage is how many of the regu are there on a day: Present counts the members who are not on an
// approved leave, the person asking not included (they would be away too).
type Coverage struct {
	Date    timeutil.Date
	Present int
	Size    int
}

// AnnualBalance is the annual leave the person has, and what is left after this request.
type AnnualBalance struct {
	RemainingDays     int
	AfterApprovalDays int
}

// ManagedFilter narrows a list of requests for someone who decides them.
type ManagedFilter struct {
	TeamCode    string
	ProjectCode string
	RegionCode  string
	Types       []Type
	Statuses    []Status
	Q           string
	From, To    *timeutil.Date
	Limit       int
	Offset      int
}

// wib is the zone "today" is read in where no single project is meant (a list over several).
var wib = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Jakarta"); err == nil {
		return loc
	}
	return time.FixedZone("WIB", 7*60*60)
}()

// ListForDanru is GET /permissions: the requests of the regu the caller leads today, newest first.
//
// ⚠ By live team scope (attendance.leave.approve, TEAM), NOT by the approver_personnel_id frozen at
// submit. KPI's badge counts by the frozen approver while its list uses the live roster, and the two
// disagree after a reassignment; here there is one answer: whoever leads the regu now sees it.
func (s *Service) ListForDanru(ctx context.Context, p auth.Principal, f ManagedFilter) (Page, error) {
	var out Page
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if s.perms == nil {
			return access.ErrPermissionDenied
		}
		res, err := s.perms.Resolve(ctx, q, p.PersonnelID, access.AttendanceLeaveApprove, timeutil.BusinessDate(s.clock.Now(), wib))
		if err != nil {
			return err
		}
		if !res.Allowed {
			return access.ErrPermissionDenied
		}
		teams := res.TeamCodes
		if teams == nil {
			teams = []string{}
		}
		if f.TeamCode != "" && !res.HasTeam(f.TeamCode) {
			return access.ErrPermissionDenied
		}
		out, err = s.listManaged(ctx, q, teams, f)
		return err
	})
	return out, err
}

// ListForAdmin is GET /admin/permissions: every request within the admin's reach.
func (s *Service) ListForAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, f ManagedFilter) (Page, error) {
	if !scope.All {
		f.RegionCode = scope.RegionCode // a REGION_ADMIN sees their region only, whatever they ask
	}
	var out Page
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		var err error
		out, err = s.listManaged(ctx, q, nil, f)
		return err
	})
	return out, err
}

// listManaged reads a page. teams nil means every regu.
func (s *Service) listManaged(ctx context.Context, q *db.Queries, teams []string, f ManagedFilter) (Page, error) {
	types, statuses := make([]string, 0, len(f.Types)), make([]string, 0, len(f.Statuses))
	for _, t := range f.Types {
		types = append(types, string(t))
	}
	for _, st := range f.Statuses {
		statuses = append(statuses, string(st))
	}
	rows, err := q.ListLeaveRequestViews(ctx, db.ListLeaveRequestViewsParams{
		TeamCodes: teams, TeamCode: textPtrOrNull(strPtr(f.TeamCode)), ProjectCode: textPtrOrNull(strPtr(f.ProjectCode)),
		RegionCode: textPtrOrNull(strPtr(f.RegionCode)), Types: types, Statuses: statuses,
		FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To), Q: f.Q,
		PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
	})
	if err != nil {
		return Page{}, fmt.Errorf("list leave requests: %w", err)
	}
	out := Page{Requests: make([]Request, 0, len(rows))}
	for _, row := range rows {
		r, err := s.requestOf(ctx, q, row.LeaveRequest, row.CategoryCode, row.CategoryName, row.DecidedByName)
		if err != nil {
			return Page{}, err
		}
		if err := s.enrich(ctx, q, &r, row.PersonName, row.PersonPosition, false); err != nil {
			return Page{}, err
		}
		out.Requests = append(out.Requests, r)
	}
	total, err := q.CountLeaveRequestViews(ctx, db.CountLeaveRequestViewsParams{
		TeamCodes: teams, TeamCode: textPtrOrNull(strPtr(f.TeamCode)), ProjectCode: textPtrOrNull(strPtr(f.ProjectCode)),
		RegionCode: textPtrOrNull(strPtr(f.RegionCode)), Types: types, Statuses: statuses,
		FromDate: dateParamPtr(f.From), ToDate: dateParamPtr(f.To), Q: f.Q,
	})
	out.Total = int(total)
	return out, err
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// enrich adds what someone deciding needs: who the person is, who else of the regu is away on the
// same days, how many are left, and for annual leave the balance before and after.
//
// ⚠ The balance after is computed the way KPI does: balance minus the request's days, never below 0.
// Annual days are taken from the balance at submit already, so for a waiting request the balance
// shown is what is left and the "after" is the most the person could ever fall to.
func (s *Service) enrich(ctx context.Context, q *db.Queries, r *Request, name, position string, withBalance bool) error {
	r.Person = &Person{PersonnelID: r.PersonnelID, Name: name, Position: position}

	overlaps, err := q.ListApprovedLeaveOverlapping(ctx, db.ListApprovedLeaveOverlappingParams{
		TeamCode: r.TeamCode, PersonnelID: r.PersonnelID, StartDate: dateParam(r.StartDate), EndDate: dateParam(r.EndDate),
	})
	if err != nil {
		return fmt.Errorf("list overlapping leave: %w", err)
	}
	r.OverlapsWith = make([]Overlap, 0, len(overlaps))
	day := r.StartDate
	for i, o := range overlaps {
		from, to := dateValue(o.StartDate), dateValue(o.EndDate)
		r.OverlapsWith = append(r.OverlapsWith, Overlap{Name: o.FullName, From: from, To: to})
		if i == 0 && from.After(day) {
			day = from // the first day somebody else is away
		}
	}
	size, err := q.CountTeamMembersOn(ctx, db.CountTeamMembersOnParams{TeamCode: r.TeamCode, OnDate: dateParam(day)})
	if err != nil {
		return fmt.Errorf("count team members: %w", err)
	}
	away, err := q.CountTeamMembersOnLeave(ctx, db.CountTeamMembersOnLeaveParams{TeamCode: r.TeamCode, PersonnelID: r.PersonnelID, OnDate: dateParam(day)})
	if err != nil {
		return fmt.Errorf("count members on leave: %w", err)
	}
	r.Coverage = &Coverage{Date: day, Present: max(0, int(size)-int(away)-1), Size: int(size)}

	if withBalance {
		cat, err := categoryByCode(ctx, q, r.CategoryCode)
		if err != nil {
			return err
		}
		if cat.IsAnnual {
			balance, err := Balance(ctx, q, r.PersonnelID)
			if err != nil {
				return err
			}
			r.Balance = &AnnualBalance{RemainingDays: balance, AfterApprovalDays: max(0, balance-r.WorkingDays)}
		}
	}
	return nil
}

// getManaged reads a request the way someone deciding it sees it, with the person, the overlaps and
// the balance. The caller has already been checked.
func (s *Service) getManaged(ctx context.Context, p auth.Principal, id int64) (Request, error) {
	var out Request
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetLeaveRequestViewWithPerson(ctx, id)
		if err != nil {
			return fmt.Errorf("get leave request: %w", err)
		}
		if out, err = s.requestOf(ctx, q, row.LeaveRequest, row.CategoryCode, row.CategoryName, row.DecidedByName); err != nil {
			return err
		}
		return s.enrich(ctx, q, &out, row.PersonName, row.PersonPosition, true)
	})
	return out, err
}

// GetForAdmin is GET /admin/permissions/{id}: any request within the admin's reach. One outside it,
// and one that is not there, are the same 404.
func (s *Service) GetForAdmin(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64) (Request, error) {
	var out Request
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetLeaveRequestViewWithPerson(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave request: %w", err)
		}
		if !scope.AllowsRegion(access.RegionOfCode(row.LeaveRequest.ProjectCode)) {
			return ErrNotFound
		}
		if out, err = s.requestOf(ctx, q, row.LeaveRequest, row.CategoryCode, row.CategoryName, row.DecidedByName); err != nil {
			return err
		}
		return s.enrich(ctx, q, &out, row.PersonName, row.PersonPosition, true)
	})
	return out, err
}

// ---------- notifications ----------

// NoticeKind is what a notice is about.
type NoticeKind string

// The notices a request sends (05's types LEAVE_REQUESTED, LEAVE_DECIDED, LEAVE_CANCELLED).
const (
	NoticeRequested NoticeKind = "REQUESTED"
	NoticeDecided   NoticeKind = "DECIDED"
	NoticeCancelled NoticeKind = "CANCELLED"
)

// Notice is a notification to send, after the transaction that caused it has committed.
type Notice struct {
	Kind         NoticeKind
	RequestID    int64
	RecipientIDs []int64
	// PersonName is who the request is of: the texts name the person, never the NIK.
	PersonName   string
	TeamCode     string
	BusinessLine string
	// Date is the first day of the request.
	Date     timeutil.Date
	Approved bool
}

// Notifier sends notices. cmd/api adapts notifications.Service to it.
type Notifier interface {
	NotifyLeave(ctx context.Context, n Notice)
}

// UseNotifier plugs in the sender of notices. Call it while wiring.
func (s *Service) UseNotifier(n Notifier) { s.notifier = n }

func (s *Service) notify(ctx context.Context, n *Notice) {
	if n != nil && s.notifier != nil && len(n.RecipientIDs) > 0 {
		s.notifier.NotifyLeave(ctx, *n)
	}
}

// noticeFor builds the notice of a request inside its transaction. A submit and a cancel go to the
// DANRU of the regu, or to the admins when there is none (access.Service.DanruOf, the same rule as
// an absence); a decision goes to the owner.
func (s *Service) noticeFor(ctx context.Context, q *db.Queries, kind NoticeKind, lr db.LeaveRequest, approved bool) (*Notice, error) {
	if s.notifier == nil {
		return nil, nil
	}
	n := &Notice{Kind: kind, RequestID: lr.ID, TeamCode: lr.TeamCode, Date: dateValue(lr.StartDate), Approved: approved}
	name, err := q.GetLeavePersonName(ctx, lr.PersonnelID)
	if err != nil {
		return nil, fmt.Errorf("get person name: %w", err)
	}
	n.PersonName = name
	if line, err := q.GetBusinessLineOfProject(ctx, lr.ProjectCode); err == nil {
		n.BusinessLine = string(line)
	}
	if kind == NoticeDecided {
		n.RecipientIDs = []int64{lr.PersonnelID}
		return n, nil
	}
	if s.perms == nil {
		return nil, nil
	}
	if n.RecipientIDs, err = s.perms.DanruOf(ctx, q, lr.TeamCode, lr.PersonnelID, n.Date); err != nil {
		return nil, fmt.Errorf("recipients: %w", err)
	}
	return n, nil
}
