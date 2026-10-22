package organization

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/pii"
	"siapp/pkg/platform/timeutil"
)

// mountMembers adds the team member routes: the members of a regu, and the actions on one
// membership (move, change role).
func (h *Handler) mountMembers(r chi.Router) {
	r.Get("/teams/{code}/members", h.listMembers)
	r.Post("/teams/{code}/members", h.addMember)
	r.Delete("/teams/{code}/members/{personnel_id}", h.endMember)
	r.Post("/team-members/{id}/move", h.moveMember)
	r.Post("/team-members/{id}/change-role", h.changeMemberRole)
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var on *timeutil.Date
	if s := r.URL.Query().Get("on_date"); s != "" {
		d, err := timeutil.ParseDate(s)
		if err != nil {
			httpx.Error(w, r, validationError("on_date", "Harus berformat YYYY-MM-DD."))
			return
		}
		on = &d
	}
	rows, total, err := h.members.List(r.Context(), p, scope, chi.URLParam(r, "code"), on, query.Limit, query.Offset())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.TeamMembership, 0, len(rows))
	for _, row := range rows {
		out = append(out, memberToAPI(row))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in MemberInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.members.Add(r.Context(), p, scope, chi.URLParam(r, "code"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, memberToAPI(row))
}

func (h *Handler) endMember(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	personnelID, err := strconv.ParseInt(chi.URLParam(r, "personnel_id"), 10, 64)
	if err != nil || personnelID < 1 {
		httpx.Error(w, r, ErrMembershipNotFound)
		return
	}
	if err := h.members.End(r.Context(), p, scope, chi.URLParam(r, "code"), personnelID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) moveMember(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := membershipID(w, r)
	if !ok {
		return
	}
	var in MoveInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.members.Move(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, memberToAPI(row))
}

func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := membershipID(w, r)
	if !ok {
		return
	}
	var in ChangeRoleInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.members.ChangeRole(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, memberToAPI(row))
}

// membershipID reads the {id} of a membership. An id that is not a positive number is a
// membership that does not exist.
func membershipID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, ErrMembershipNotFound)
		return 0, false
	}
	return id, true
}

// memberToAPI maps a member to its contract type. The NIK is masked like everywhere else.
func memberToAPI(m Member) apigen.TeamMembership {
	role := apigen.TeamRole(m.Role)
	out := apigen.TeamMembership{
		Id: &m.ID, TeamCode: &m.TeamCode, PersonnelId: &m.PersonnelID, FullName: &m.FullName, Role: &role,
		StartDate: &openapi_types.Date{Time: m.StartDate.Time()}, CreatedAt: &m.CreatedAt, UpdatedAt: &m.UpdatedAt,
	}
	if m.NIK != "" {
		masked := pii.MaskNIK(m.NIK)
		out.Nik = &masked
	}
	if m.EndDate != nil {
		out.EndDate = &openapi_types.Date{Time: m.EndDate.Time()}
	}
	return out
}
