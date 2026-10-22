package organization

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/pii"
)

// MountField adds the Regu screens of the field app (Phase 02.6). They need a signed-in
// person and no web role: what each person sees is decided by the regu they serve in.
func (h *Handler) MountField(r chi.Router) {
	r.Get("/api/v1/teams/me", h.myTeams)
	r.Get("/api/v1/teams", h.fieldTeams)
	r.Get("/api/v1/teams/{code}/members", h.teamDetail)
	r.Get("/api/v1/personnels/{id}", h.personnelCard)
}

// fieldCaller returns who is asking. auth.Require has already refused a request without one.
func fieldCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
	}
	return p, ok
}

func (h *Handler) myTeams(w http.ResponseWriter, r *http.Request) {
	p, ok := fieldCaller(w, r)
	if !ok {
		return
	}
	views, err := h.roster.MyTeams(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.TeamDetail, 0, len(views))
	for _, v := range views {
		out = append(out, teamDetailToAPI(v))
	}
	httpx.OK(w, r, http.StatusOK, out)
}

func (h *Handler) fieldTeams(w http.ResponseWriter, r *http.Request) {
	p, ok := fieldCaller(w, r)
	if !ok {
		return
	}
	views, err := h.roster.Teams(r.Context(), p, r.URL.Query().Get("project_code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.TeamSummary, 0, len(views))
	for _, v := range views {
		out = append(out, teamSummaryToAPI(v))
	}
	httpx.OK(w, r, http.StatusOK, out)
}

func (h *Handler) teamDetail(w http.ResponseWriter, r *http.Request) {
	p, ok := fieldCaller(w, r)
	if !ok {
		return
	}
	v, err := h.roster.TeamDetail(r.Context(), p, chi.URLParam(r, "code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, teamDetailToAPI(v))
}

func (h *Handler) personnelCard(w http.ResponseWriter, r *http.Request) {
	p, ok := fieldCaller(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, errNoSuchPersonnel)
		return
	}
	c, err := h.roster.Card(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	memberships := make([]apigen.PersonnelMembership, 0, len(c.Memberships))
	for _, m := range c.Memberships {
		role := apigen.TeamRole(m.Role)
		memberships = append(memberships, apigen.PersonnelMembership{
			TeamCode: &m.TeamCode, TeamName: &m.TeamName, ProjectCode: &m.ProjectCode, ProjectName: &m.ProjectName, Role: &role,
		})
	}
	out := apigen.PersonnelCard{
		PersonnelId: &c.PersonnelID, FullName: &c.FullName, Phone: c.Phone, PhotoUrl: c.PhotoURL,
		PositionName: c.PositionName, Memberships: &memberships,
	}
	if c.NIK != "" {
		masked := pii.MaskNIK(c.NIK)
		out.Nik = &masked
	}
	httpx.OK(w, r, http.StatusOK, out)
}

func teamSummaryToAPI(v TeamView) apigen.TeamSummary {
	understaffed, mine := v.Understaffed(), v.MyRole != ""
	out := apigen.TeamSummary{
		Code: &v.Code, Name: &v.Name, Phone: v.Phone, ProjectCode: &v.ProjectCode, ProjectName: &v.ProjectName,
		ClientCode: &v.ClientCode, ClientName: &v.ClientName, BusinessDate: apiDate(v.BusinessDate),
		MemberCount: &v.MemberCount, MinPersonnel: &v.MinPersonnel, Understaffed: &understaffed, IsMyTeam: &mine,
		Danru: personToAPI(v.Danru),
	}
	if mine {
		role := apigen.TeamRole(v.MyRole)
		out.MyRole = &role
	}
	return out
}

func teamDetailToAPI(v TeamView) apigen.TeamDetail {
	s := teamSummaryToAPI(v)
	members := make([]apigen.TeamRosterMember, 0, len(v.Members))
	for _, m := range v.Members {
		role := apigen.TeamRole(m.Role)
		member := apigen.TeamRosterMember{
			PersonnelId: &m.PersonnelID, FullName: &m.FullName, Role: &role, PositionName: m.PositionName, PhotoUrl: m.PhotoURL,
		}
		if m.TodayShift != nil {
			start, end, overnight := formatClock(m.TodayShift.StartMinute), formatClock(m.TodayShift.EndMinute), m.TodayShift.Overnight()
			member.TodayShift = &apigen.ScheduleShift{
				Id: &m.TodayShift.ID, Name: &m.TodayShift.Name, StartTime: &start, EndTime: &end, IsOff: &m.TodayShift.IsOff, Overnight: &overnight,
			}
		}
		members = append(members, member)
	}
	shifts := make([]apigen.TeamShiftToday, 0, len(v.TodayShifts))
	for _, t := range v.TodayShifts {
		start, end, overnight := formatClock(t.Shift.StartMinute), formatClock(t.Shift.EndMinute), t.Shift.Overnight()
		shifts = append(shifts, apigen.TeamShiftToday{
			Id: &t.Shift.ID, Name: &t.Shift.Name, StartTime: &start, EndTime: &end, Overnight: &overnight, PersonnelCount: &t.PersonnelCount,
		})
	}
	return apigen.TeamDetail{
		Code: s.Code, Name: s.Name, Phone: s.Phone, ProjectCode: s.ProjectCode, ProjectName: s.ProjectName,
		ClientCode: s.ClientCode, ClientName: s.ClientName, BusinessDate: s.BusinessDate, MemberCount: s.MemberCount,
		MinPersonnel: s.MinPersonnel, Understaffed: s.Understaffed, IsMyTeam: s.IsMyTeam, MyRole: s.MyRole, Danru: s.Danru,
		RelationshipOfficer: personToAPI(v.RelationshipOfficer), TodayShifts: &shifts, Members: &members,
	}
}

func personToAPI(p *Person) *apigen.DirectoryPerson {
	if p == nil {
		return nil
	}
	return &apigen.DirectoryPerson{PersonnelId: &p.PersonnelID, FullName: &p.FullName, Phone: p.Phone, PhotoUrl: p.PhotoURL}
}
