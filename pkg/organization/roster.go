package organization

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// Person is the short card of someone on the Regu screens: a DANRU, a relationship officer.
type Person struct {
	PersonnelID int64
	FullName    string
	Phone       *string
	PhotoURL    *string
}

// RosterMember is one person serving in a regu today, with their shift of today (nil when
// they are not scheduled).
type RosterMember struct {
	Person
	Role         string
	PositionName *string
	TodayShift   *Shift
}

// TeamShiftToday is a working shift some members of a regu are scheduled on today.
type TeamShiftToday struct {
	Shift          Shift
	PersonnelCount int
}

// TeamView is a regu as the field app shows it. Members and TodayShifts are only filled for
// the detail (TeamDetail, MyTeams).
type TeamView struct {
	Code         string
	Name         string
	Phone        *string
	ProjectCode  string
	ProjectName  string
	ClientCode   string
	ClientName   string
	BusinessDate timeutil.Date
	MinPersonnel int
	MemberCount  int
	// MyRole is the caller's role in the regu today, empty when they are not in it.
	MyRole              string
	Danru               *Person
	RelationshipOfficer *Person
	TodayShifts         []TeamShiftToday
	Members             []RosterMember
}

// Understaffed reports whether fewer people serve in the regu today than it needs (DL-02).
func (t TeamView) Understaffed() bool { return t.MemberCount < t.MinPersonnel }

// PersonnelCard is what the Regu screen shows of a person, and nothing more: Personalia owns
// the HR profile (birth date, address, documents), so SiAPP never shows it.
type PersonnelCard struct {
	Person
	NIK          string
	PositionName *string
	Memberships  []Membership
}

// Roster serves the Regu screens of the field app (§2.9, §3.2.5).
//
// ⚠ Field scope: a person sees the projects where they serve in a regu today, each project's
// today in its own timezone. A DANRU sees their project because they are its regu's DANRU,
// and a MEMBER the project of their own regu: there is no separate project assignment.
type Roster struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewRoster returns the Roster usecase.
func NewRoster(pool *pgxpool.Pool, c clock.Clock) *Roster { return &Roster{pool: pool, clock: c} }

// MyTeams returns the detail of every regu the caller serves in today.
func (r *Roster) MyTeams(ctx context.Context, actor auth.Principal) ([]TeamView, error) {
	var out []TeamView
	now := r.clock.Now()
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		mine, err := r.membershipsOf(ctx, q, actor.PersonnelID, now)
		if err != nil {
			return err
		}
		codes := make([]string, 0, len(mine))
		for _, m := range mine {
			codes = append(codes, m.TeamCode)
		}
		if len(codes) == 0 {
			return nil
		}
		out, err = r.teams(ctx, q, now, mine, nil, codes, true)
		return err
	})
	return out, err
}

// Teams returns the regu of a project the caller serves in today, or, without a project, the
// regu of every project the caller serves in today. A project the caller has no regu in today
// (or one that does not exist) is NOT_PROJECT_MEMBER.
func (r *Roster) Teams(ctx context.Context, actor auth.Principal, projectCode string) ([]TeamView, error) {
	var out []TeamView
	now := r.clock.Now()
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		mine, err := r.membershipsOf(ctx, q, actor.PersonnelID, now)
		if err != nil {
			return err
		}
		projects := projectsOf(mine)
		if projectCode != "" {
			if !slices.Contains(projects, projectCode) {
				return ErrNotProjectMember
			}
			projects = []string{projectCode}
		}
		if len(projects) == 0 {
			return nil
		}
		out, err = r.teams(ctx, q, now, mine, projects, nil, false)
		return err
	})
	return out, err
}

// TeamDetail returns one regu with its members and today's shifts, for someone who serves in
// a regu of the same project today.
func (r *Roster) TeamDetail(ctx context.Context, actor auth.Principal, teamCode string) (TeamView, error) {
	if teamCode == "" {
		return TeamView{}, ErrTeamNotFound
	}
	var out TeamView
	now := r.clock.Now()
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		mine, err := r.membershipsOf(ctx, q, actor.PersonnelID, now)
		if err != nil {
			return err
		}
		views, err := r.teams(ctx, q, now, mine, nil, []string{teamCode}, true)
		if err != nil {
			return err
		}
		if len(views) == 0 {
			return ErrTeamNotFound
		}
		if !slices.Contains(projectsOf(mine), views[0].ProjectCode) {
			return ErrNotProjectMember
		}
		out = views[0]
		return nil
	})
	return out, err
}

// Card returns a person's card for someone who shares a project with them today (or for
// themselves). Only the memberships in the shared projects are shown: where else the person
// serves is not the caller's business.
func (r *Roster) Card(ctx context.Context, actor auth.Principal, personnelID int64) (PersonnelCard, error) {
	var out PersonnelCard
	now := r.clock.Now()
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		p, err := q.GetPersonnel(ctx, personnelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoSuchPersonnel
		}
		if err != nil {
			return fmt.Errorf("get personnel: %w", err)
		}
		if p.Status == db.PersonnelStatusINACTIVE && personnelID != actor.PersonnelID {
			return errNoSuchPersonnel // they have left: the Regu screens no longer list them
		}
		theirs, err := r.membershipsOf(ctx, q, personnelID, now)
		if err != nil {
			return err
		}
		shown := theirs
		if personnelID != actor.PersonnelID {
			mine, err := r.membershipsOf(ctx, q, actor.PersonnelID, now)
			if err != nil {
				return err
			}
			shared := projectsOf(mine)
			shown = slices.DeleteFunc(slices.Clone(theirs), func(m Membership) bool { return !slices.Contains(shared, m.ProjectCode) })
			if len(shown) == 0 {
				return ErrNotProjectMember.WithMessage("Anda tidak berada di proyek yang sama dengan personil ini.")
			}
		}
		out = PersonnelCard{
			Person:       Person{PersonnelID: p.ID, FullName: p.FullName, Phone: textValue(p.Phone), PhotoURL: textValue(p.PhotoUrl)},
			PositionName: textValue(p.PositionName), Memberships: shown,
		}
		if p.Nik.Valid {
			out.NIK = p.Nik.String
		}
		return nil
	})
	return out, err
}

// membershipsOf returns the regu a person serves in today, each project's today in its own
// timezone.
func (r *Roster) membershipsOf(ctx context.Context, q *db.Queries, personnelID int64, now time.Time) ([]Membership, error) {
	rows, err := q.ListCurrentMembershipsOfPersonnel(ctx, db.ListCurrentMembershipsOfPersonnelParams{PersonnelID: personnelID, Now: ts(now)})
	if err != nil {
		return nil, fmt.Errorf("list current memberships: %w", err)
	}
	out := make([]Membership, 0, len(rows))
	for _, m := range rows {
		out = append(out, toMembership(m.ID, m.TeamCode, m.TeamName, m.ProjectCode, m.ProjectName, string(m.Role), m.StartDate, m.EndDate))
	}
	return out, nil
}

// teams reads the regu of some projects, or some regu by code (exactly one of the two is
// non-nil), with who serves in them today. detail also fills the members and today's shifts.
func (r *Roster) teams(ctx context.Context, q *db.Queries, now time.Time, mine []Membership, projects, codes []string, detail bool) ([]TeamView, error) {
	rows, err := q.ListDirectoryTeams(ctx, db.ListDirectoryTeamsParams{ProjectCodes: projects, TeamCodes: codes})
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	teamCodes := make([]string, 0, len(rows))
	for _, t := range rows {
		teamCodes = append(teamCodes, t.Code)
	}
	members, err := q.ListDirectoryMembersToday(ctx, db.ListDirectoryMembersTodayParams{TeamCodes: teamCodes, Now: ts(now)})
	if err != nil {
		return nil, fmt.Errorf("list members today: %w", err)
	}
	byTeam := map[string][]db.ListDirectoryMembersTodayRow{}
	for _, m := range members {
		byTeam[m.TeamCode] = append(byTeam[m.TeamCode], m)
	}

	out := make([]TeamView, 0, len(rows))
	for _, t := range rows {
		loc, err := time.LoadLocation(t.Timezone)
		if err != nil {
			return nil, fmt.Errorf("project %s: load timezone %q: %w", t.ProjectCode, t.Timezone, err)
		}
		v := TeamView{
			Code: t.Code, Name: t.TeamName, Phone: textValue(t.Phone), ProjectCode: t.ProjectCode, ProjectName: t.ProjectName,
			ClientCode: t.ClientCode, ClientName: t.ClientName, BusinessDate: timeutil.BusinessDate(now, loc),
			MinPersonnel: int(t.MinPersonnel), MemberCount: len(byTeam[t.Code]),
		}
		for _, m := range mine {
			if m.TeamCode == t.Code {
				v.MyRole = m.Role
			}
		}
		if t.RoID.Valid {
			v.RelationshipOfficer = &Person{PersonnelID: t.RoID.Int64, FullName: t.RoFullName.String, Phone: textValue(t.RoPhone), PhotoURL: textValue(t.RoPhotoUrl)}
		}
		for _, m := range byTeam[t.Code] {
			member := RosterMember{
				Person: Person{PersonnelID: m.PersonnelID, FullName: m.FullName, Phone: textValue(m.Phone), PhotoURL: textValue(m.PhotoUrl)},
				Role:   string(m.Role), PositionName: textValue(m.PositionName),
			}
			if m.ShiftID.Valid {
				member.TodayShift = &Shift{ID: m.ShiftID.Int64, Name: m.ShiftName.String, StartMinute: int(m.StartMinute.Int16), EndMinute: int(m.EndMinute.Int16), IsOff: m.IsOff.Bool}
			}
			if member.Role == RoleDanru && v.Danru == nil {
				danru := member.Person
				v.Danru = &danru
			}
			if detail {
				v.Members = append(v.Members, member)
			}
		}
		if detail {
			v.TodayShifts = shiftsToday(v.Members)
		}
		out = append(out, v)
	}
	return out, nil
}

// shiftsToday counts the members of each working shift scheduled today, by start time. An OFF
// day is no shift of the regu.
func shiftsToday(members []RosterMember) []TeamShiftToday {
	var out []TeamShiftToday
	for _, m := range members {
		if m.TodayShift == nil || m.TodayShift.IsOff {
			continue
		}
		i := slices.IndexFunc(out, func(s TeamShiftToday) bool { return s.Shift.ID == m.TodayShift.ID })
		if i < 0 {
			out = append(out, TeamShiftToday{Shift: *m.TodayShift})
			i = len(out) - 1
		}
		out[i].PersonnelCount++
	}
	slices.SortFunc(out, func(a, b TeamShiftToday) int {
		if a.Shift.StartMinute != b.Shift.StartMinute {
			return a.Shift.StartMinute - b.Shift.StartMinute
		}
		return int(a.Shift.ID - b.Shift.ID)
	})
	return out
}

// projectsOf returns the projects of some memberships, each once.
func projectsOf(ms []Membership) []string {
	var out []string
	for _, m := range ms {
		if !slices.Contains(out, m.ProjectCode) {
			out = append(out, m.ProjectCode)
		}
	}
	return out
}
