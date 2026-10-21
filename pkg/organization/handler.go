package organization

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
)

// Usecases is every organization usecase, built over one pool and clock.
type Usecases struct {
	Regions   *Regions
	Branches  *Branches
	Clients   *Clients
	Projects  *Projects
	Teams     *Teams
	Members   *Members
	Shifts    *ShiftMasters
	Schedules *Schedules
}

// NewUsecases returns the organization usecases. people finds a person by NIK for the team
// members (identity.People); it may be nil where no test or command adds members.
func NewUsecases(pool *pgxpool.Pool, c clock.Clock, people PeopleResolver) Usecases {
	return Usecases{
		Regions: NewRegions(pool, c), Branches: NewBranches(pool, c), Clients: NewClients(pool, c),
		Projects: NewProjects(pool, c), Teams: NewTeams(pool, c), Members: NewMembers(pool, c, people),
		Shifts: NewShiftMasters(pool, c), Schedules: NewSchedules(pool, c),
	}
}

// Handler serves the organization endpoints under /api/v1/admin.
type Handler struct {
	regions   *Regions
	branches  *Branches
	clients   *Clients
	projects  *Projects
	teams     *Teams
	members   *Members
	shifts    *ShiftMasters
	schedules *Schedules
}

// NewHandler returns the organization Handler.
func NewHandler(u Usecases) *Handler {
	return &Handler{regions: u.Regions, branches: u.Branches, clients: u.Clients, projects: u.Projects, teams: u.Teams, members: u.Members,
		shifts: u.Shifts, schedules: u.Schedules}
}

// MountAdmin adds the routes to the /api/v1/admin group, behind access.RequireAdmin (cmd/api
// mounts it once on the group). The paths are relative to the group.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/regions", h.listRegions)
	r.Post("/regions", h.createRegion)
	r.Get("/regions/{code}", h.getRegion)
	r.Patch("/regions/{code}", h.updateRegion)
	r.Delete("/regions/{code}", h.deleteRegion)
	h.mountHierarchy(r)
	h.mountMembers(r)
	h.mountShifts(r)
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
