package organization

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler serves the organization endpoints under /api/v1/admin.
type Handler struct {
	regions *Regions
}

// NewHandler returns the organization Handler.
func NewHandler(regions *Regions) *Handler { return &Handler{regions: regions} }

// MountAdmin adds the routes to the /api/v1/admin group, behind access.RequireAdmin (cmd/api
// mounts it once on the group). The paths are relative to the group.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/regions", h.listRegions)
	r.Post("/regions", h.createRegion)
	r.Get("/regions/{code}", h.getRegion)
	r.Patch("/regions/{code}", h.updateRegion)
	r.Delete("/regions/{code}", h.deleteRegion)
}

// caller returns who is asking and what they may reach. Both are set by auth.Require and
// access.RequireAdmin before a handler runs.
func caller(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, bool) {
	p, okP := auth.From(r.Context())
	s, okS := access.ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, access.AdminScope{}, false
	}
	return p, s, true
}

func (h *Handler) listRegions(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	filter := RegionFilter{Q: query.Q, Limit: query.Limit, Offset: query.Offset()}
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "is_active", Code: "INVALID", Message: "Harus true atau false.",
			}))
			return
		}
		filter.Active = &active
	}
	rows, total, err := h.regions.List(r.Context(), p, scope, filter)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.Region, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAPI(row))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) getRegion(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	row, err := h.regions.Get(r.Context(), p, scope, chi.URLParam(r, "code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, toAPI(row))
}

func (h *Handler) createRegion(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in RegionInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.regions.Create(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, toAPI(row))
}

func (h *Handler) updateRegion(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in RegionUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.regions.Update(r.Context(), p, scope, chi.URLParam(r, "code"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, toAPI(row))
}

func (h *Handler) deleteRegion(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	if err := h.regions.Delete(r.Context(), p, scope, chi.URLParam(r, "code")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func toAPI(r Region) apigen.Region {
	return apigen.Region{
		Code: &r.Code, Description: &r.Description, IsActive: &r.Active,
		CreatedAt: &r.CreatedAt, UpdatedAt: &r.UpdatedAt,
	}
}
