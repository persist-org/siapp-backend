package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/timeutil"
)

// Resolution is the answer to "may this person do X, and on which regu?". Regu and projects
// are named by their codes (D19: 1K-IBMI-S1, 1K-IBMI-S).
type Resolution struct {
	// Allowed is true when at least one active membership gives the permission.
	Allowed bool
	// SelfOnly is true when the person may use the permission on their own records only: they
	// hold it, and no grant is wider than SELF. A SelfOnly person has no TeamCodes.
	SelfOnly bool
	// TeamCodes are the regu the permission reaches, sorted, each once. A PROJECT grant adds
	// every active regu of its project.
	TeamCodes []string
	// ProjectCodes are the projects of the PROJECT grants, sorted, each once.
	ProjectCodes []string
}

// HasTeam reports whether the permission reaches the regu.
func (r Resolution) HasTeam(code string) bool { return slices.Contains(r.TeamCodes, code) }

// HasProject reports whether a PROJECT grant covers the project.
func (r Resolution) HasProject(code string) bool { return slices.Contains(r.ProjectCodes, code) }

// Service answers what a field person may do. It is for the people of the mobile app: web admins
// are gated by RequireAdmin and use /admin routes only.
//
// The methods that take a *db.Queries run in the caller's tenant transaction, like every
// other module's lookups (identity's MembershipsOf, AdminScopeOf): a usecase that already
// holds a transaction must not open a second one to ask, so the answer and the writes it
// guards see the same data.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewService returns the access Service.
func NewService(pool *pgxpool.Pool, c clock.Clock) *Service { return &Service{pool: pool, clock: c} }

// Resolve says whether personnelID holds perm on the business date on, and which regu and
// projects it reaches. A usecase calls it with the date of the record it works on, in the
// timezone of the project that record belongs to.
//
// ⚠ The rules. Only memberships that are active on that date count (start_date <= on <=
// end_date, end inclusive), in a regu and project that are not deleted. For each one, the grant
// of the person's role in that regu decides:
//   - NONE, or no row (an organization not seeded yet): nothing, fail closed;
//   - SELF: the person may use it on their own records. Sets Allowed, and SelfOnly if nothing
//     wider turns up;
//   - TEAM: adds that regu;
//   - PROJECT: adds every active regu of that project, and the project itself. The person's own
//     regu is added even if it is inactive: they still serve there.
//
// A permission that belongs to a business line (E-Memo: cleaning) counts only through a
// membership in a project of that line. SelfOnly is true only when no grant is wider than SELF.
// An unknown permission is a programming error, not a denial, so it is an error.
func (s *Service) Resolve(ctx context.Context, q *db.Queries, personnelID int64, perm Permission, on timeutil.Date) (Resolution, error) {
	if on.IsZero() {
		return Resolution{}, errors.New("access: Resolve needs a date")
	}
	return s.resolve(ctx, q, personnelID, perm, pgtype.Date{Time: on.Time(), Valid: true})
}

// ResolveToday is Resolve for "today", where today is each membership's own business date: the
// date in the timezone of its project (WIB, WITA or WIT), the same one /auth/me lists
// memberships by. It is what the Require middleware uses.
func (s *Service) ResolveToday(ctx context.Context, q *db.Queries, personnelID int64, perm Permission) (Resolution, error) {
	return s.resolve(ctx, q, personnelID, perm, pgtype.Date{})
}

func (s *Service) resolve(ctx context.Context, q *db.Queries, personnelID int64, perm Permission, on pgtype.Date) (Resolution, error) {
	entry, ok := entryOf(perm)
	if !ok {
		return Resolution{}, fmt.Errorf("access: unknown permission %q", perm)
	}
	rows, err := q.ListGrantingMemberships(ctx, db.ListGrantingMembershipsParams{
		Permission: string(perm), PersonnelID: personnelID, OnDate: on,
		Now: pgtype.Timestamptz{Time: s.clock.Now(), Valid: true},
	})
	if err != nil {
		return Resolution{}, fmt.Errorf("list granting memberships: %w", err)
	}

	res := Resolution{TeamCodes: []string{}, ProjectCodes: []string{}}
	wide := false
	for _, r := range rows {
		if !entry.appliesTo(BusinessLine(r.BusinessLine)) {
			continue
		}
		switch Scope(r.Scope) {
		case ScopeSelf:
			res.Allowed = true
		case ScopeTeam:
			res.Allowed, wide = true, true
			res.TeamCodes = append(res.TeamCodes, r.TeamCode)
		case ScopeProject:
			res.Allowed, wide = true, true
			res.TeamCodes = append(res.TeamCodes, r.TeamCode)
			res.ProjectCodes = append(res.ProjectCodes, r.ProjectCode)
		case ScopeNone:
			// The query leaves NONE out; kept here so a change to it cannot grant by accident.
		}
	}
	res.SelfOnly = res.Allowed && !wide

	if len(res.ProjectCodes) > 0 {
		teams, err := q.ListActiveTeamsOfProjects(ctx, res.ProjectCodes)
		if err != nil {
			return Resolution{}, fmt.Errorf("list teams of projects: %w", err)
		}
		for _, t := range teams {
			res.TeamCodes = append(res.TeamCodes, t.Code)
		}
	}
	slices.Sort(res.TeamCodes)
	res.TeamCodes = slices.Compact(res.TeamCodes)
	slices.Sort(res.ProjectCodes)
	res.ProjectCodes = slices.Compact(res.ProjectCodes)
	return res, nil
}

// AssertTeam returns PERMISSION_DENIED unless personnelID holds perm on the business date on
// for the regu. A SELF-only grant does not reach a regu, and a regu of another organization is
// never in scope: row-level security means it is simply not there, so the answer is the same
// as for a regu the person has no right to, and says nothing about whether it exists.
func (s *Service) AssertTeam(ctx context.Context, q *db.Queries, personnelID int64, perm Permission, teamCode string, on timeutil.Date) error {
	res, err := s.Resolve(ctx, q, personnelID, perm, on)
	if err != nil {
		return err
	}
	if !res.HasTeam(teamCode) {
		return ErrPermissionDenied
	}
	return nil
}

// DanruOf returns who receives a report that is for the DANRU only ("Kirim laporan DANRU
// only", Tabel 8): the regu's DANRU on the date on, unless that is the sender.
//
// ⚠ The fallback. When the sender IS the DANRU, or the regu has no active DANRU that day (none
// assigned, or Personalia has deactivated them), there is nobody above to send to in the regu,
// so the report goes to the admins instead: every ORG_ADMIN and the REGION_ADMINs of the
// region the project sits under, active people only and never the sender. (The plan said
// PROJECT_ADMIN; since the group became one organization an admin's reach is a region.)
// The result may be empty: an organization with no such admin has nobody to tell, which is
// not an error.
func (s *Service) DanruOf(ctx context.Context, q *db.Queries, teamCode string, sender int64, on timeutil.Date) ([]int64, error) {
	if on.IsZero() {
		return nil, errors.New("access: DanruOf needs a date")
	}
	projectCode, err := q.GetProjectCodeOfTeam(ctx, teamCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project of team: %w", err)
	}
	day := pgtype.Date{Time: on.Time(), Valid: true}
	danru, err := q.GetDanruOfTeamOn(ctx, db.GetDanruOfTeamOnParams{TeamCode: teamCode, OnDate: day})
	switch {
	case err == nil && danru != sender:
		return []int64{danru}, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("get danru: %w", err)
	}

	region := RegionOfCode(projectCode)
	admins, err := q.ListAdminRecipientsOfRegion(ctx, pgtype.Text{String: region, Valid: region != ""})
	if err != nil {
		return nil, fmt.Errorf("list admin recipients: %w", err)
	}
	out := make([]int64, 0, len(admins))
	for _, id := range admins {
		if id != sender {
			out = append(out, id)
		}
	}
	return out, nil
}

// AssertBusinessLine returns FEATURE_NOT_ENABLED unless the project exists and has the line.
// A project that does not exist answers the same, so a cleaning-only route cannot be used to
// find out which project codes exist.
func (s *Service) AssertBusinessLine(ctx context.Context, q *db.Queries, projectCode string, line BusinessLine) error {
	got, err := q.GetBusinessLineOfProject(ctx, projectCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFeatureNotEnabled
	}
	if err != nil {
		return fmt.Errorf("get business line: %w", err)
	}
	if BusinessLine(got) != line {
		return ErrFeatureNotEnabled
	}
	return nil
}

var regionNumberPattern = regexp.MustCompile(`^[0-9]+`)

// RegionOfCode returns the koordinator wilayah a hierarchy code sits under: OS plus the number
// the code starts with (1K-IBMI-S1 is under OS1), or "" for a code that does not start with a
// number. organization.RegionOfCode is this function.
func RegionOfCode(code string) string {
	n := regionNumberPattern.FindString(code)
	if n == "" {
		return ""
	}
	return "OS" + n
}

// ProjectCodeOfTeam returns the project a regu code belongs to: a regu code is its project's
// code followed by the regu's number (1K-IBMI-S1 is of 1K-IBMI-S), and a project code ends in S
// or C, never a digit. It is "" for a code that does not end in a number.
func ProjectCodeOfTeam(teamCode string) string {
	project := strings.TrimRight(teamCode, "0123456789")
	if project == teamCode {
		return ""
	}
	return project
}
