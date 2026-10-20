package access

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler serves the web role endpoints under /api/v1/admin.
type Handler struct {
	roles *WebRoles
}

// NewHandler returns the access Handler.
func NewHandler(roles *WebRoles) *Handler { return &Handler{roles: roles} }

// MountAdmin adds the routes to the /api/v1/admin group, behind RequireAdmin (cmd/api mounts
// it once on the group). The paths are relative to the group.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/web-roles", h.list)
	r.Put("/web-roles/{personnel_id}", h.put)
	r.Delete("/web-roles/{personnel_id}", h.delete)
}

// caller returns who is asking and what they may reach. Both are set by auth.Require and
// RequireAdmin before a handler runs.
func caller(w http.ResponseWriter, r *http.Request) (auth.Principal, AdminScope, bool) {
	p, okP := auth.From(r.Context())
	s, okS := ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, AdminScope{}, false
	}
	return p, s, true
}

// PersonnelIDParam reads the {personnel_id} path parameter. An id that is not a positive
// number cannot name anyone, so it is NOT_FOUND like any other missing person.
func PersonnelIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "personnel_id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.ErrNotFound
	}
	return id, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, total, err := h.roles.List(r.Context(), p, scope, query.Limit, query.Offset())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.WebRoleAssignment, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAPI(row))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	target, err := PersonnelIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var req apigen.WebRoleInput
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := WebRoleInput{Role: string(req.Role)}
	if req.RegionCode != nil {
		in.RegionCode = *req.RegionCode
	}
	if err := httpx.Validate(&in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.roles.Put(r.Context(), p, scope, target, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, toAPI(row))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	target, err := PersonnelIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.roles.Delete(r.Context(), p, scope, target); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func toAPI(r WebRole) apigen.WebRoleAssignment {
	role := apigen.WebRoleAssignmentRole(r.Role)
	out := apigen.WebRoleAssignment{
		PersonnelId: &r.PersonnelID, FullName: &r.FullName, Role: &role,
		GrantedByPersonnelId: r.GrantedByPersonnelID, CreatedAt: &r.CreatedAt, UpdatedAt: &r.UpdatedAt,
	}
	if r.RegionCode != "" {
		out.RegionCode = &r.RegionCode
	}
	if r.NIK != "" {
		out.Nik = &r.NIK
	}
	return out
}
