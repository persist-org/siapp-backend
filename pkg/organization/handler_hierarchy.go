package organization

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// listArgs is what a list request carries before an entity turns it into its own filter.
type listArgs struct {
	Q      string
	Active *bool
	Limit  int
	Offset int
	// Params is the whole URL query, for the filters only one entity has (branch_code, ...).
	Params url.Values
}

// crud is the five operations of one level of the hierarchy and how its rows look in the
// API. The four levels below the region behave alike (codes, scope, soft delete, audit), so
// their handlers are one piece of code: Row is the domain type, API its contract type, In
// and Upd the create and patch bodies.
type crud[Row, API, In, Upd any] struct {
	list   func(ctx context.Context, p auth.Principal, scope access.AdminScope, a listArgs) ([]Row, int, error)
	get    func(ctx context.Context, p auth.Principal, scope access.AdminScope, code string) (Row, error)
	create func(ctx context.Context, p auth.Principal, scope access.AdminScope, in In) (Row, error)
	update func(ctx context.Context, p auth.Principal, scope access.AdminScope, code string, in Upd) (Row, error)
	remove func(ctx context.Context, p auth.Principal, scope access.AdminScope, code string) error
	toAPI  func(Row) API
}

// mountCRUD adds GET/POST on path and GET/PATCH/DELETE on path/{code}.
func mountCRUD[Row, API, In, Upd any](r chi.Router, path string, c crud[Row, API, In, Upd]) {
	r.Get(path, c.handleList)
	r.Post(path, c.handleCreate)
	r.Get(path+"/{code}", c.handleGet)
	r.Patch(path+"/{code}", c.handleUpdate)
	r.Delete(path+"/{code}", c.handleDelete)
}

func (c crud[Row, API, In, Upd]) handleList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	args := listArgs{Q: query.Q, Limit: query.Limit, Offset: query.Offset(), Params: r.URL.Query()}
	if s := args.Params.Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, validationError("is_active", "Harus true atau false."))
			return
		}
		args.Active = &active
	}
	rows, total, err := c.list(r.Context(), p, scope, args)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]API, 0, len(rows))
	for _, row := range rows {
		out = append(out, c.toAPI(row))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (c crud[Row, API, In, Upd]) handleGet(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	row, err := c.get(r.Context(), p, scope, chi.URLParam(r, "code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, c.toAPI(row))
}

func (c crud[Row, API, In, Upd]) handleCreate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in In
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := c.create(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, c.toAPI(row))
}

func (c crud[Row, API, In, Upd]) handleUpdate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in Upd
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := c.update(r.Context(), p, scope, chi.URLParam(r, "code"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, c.toAPI(row))
}

func (c crud[Row, API, In, Upd]) handleDelete(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	if err := c.remove(r.Context(), p, scope, chi.URLParam(r, "code")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

// mountHierarchy adds the kantor cabang, klien, proyek and regu routes.
func (h *Handler) mountHierarchy(r chi.Router) {
	mountCRUD(r, "/branches", crud[Branch, apigen.Branch, BranchInput, BranchUpdate]{
		list: func(ctx context.Context, p auth.Principal, scope access.AdminScope, a listArgs) ([]Branch, int, error) {
			return h.branches.List(ctx, p, scope, BranchFilter{
				Q: a.Q, RegionCode: a.Params.Get("region_code"), Active: a.Active, Limit: a.Limit, Offset: a.Offset,
			})
		},
		get: h.branches.Get, create: h.branches.Create, update: h.branches.Update, remove: h.branches.Delete,
		toAPI: branchToAPI,
	})
	mountCRUD(r, "/clients", crud[Client, apigen.Client, ClientInput, ClientUpdate]{
		list: func(ctx context.Context, p auth.Principal, scope access.AdminScope, a listArgs) ([]Client, int, error) {
			return h.clients.List(ctx, p, scope, ClientFilter{
				Q: a.Q, BranchCode: a.Params.Get("branch_code"), Active: a.Active, Limit: a.Limit, Offset: a.Offset,
			})
		},
		get: h.clients.Get, create: h.clients.Create, update: h.clients.Update, remove: h.clients.Delete,
		toAPI: clientToAPI,
	})
	mountCRUD(r, "/projects", crud[Project, apigen.Project, ProjectInput, ProjectUpdate]{
		list: func(ctx context.Context, p auth.Principal, scope access.AdminScope, a listArgs) ([]Project, int, error) {
			line := a.Params.Get("business_line")
			if line != "" && line != BusinessLineSecurity && line != BusinessLineCleaning {
				return nil, 0, validationError("business_line", "Harus security atau cleaning.")
			}
			return h.projects.List(ctx, p, scope, ProjectFilter{
				Q: a.Q, ClientCode: a.Params.Get("client_code"), BusinessLine: line, Active: a.Active, Limit: a.Limit, Offset: a.Offset,
			})
		},
		get: h.projects.Get, create: h.projects.Create, update: h.projects.Update, remove: h.projects.Delete,
		toAPI: projectToAPI,
	})
	mountCRUD(r, "/teams", crud[Team, apigen.Team, TeamInput, TeamUpdate]{
		list: func(ctx context.Context, p auth.Principal, scope access.AdminScope, a listArgs) ([]Team, int, error) {
			return h.teams.List(ctx, p, scope, TeamFilter{
				Q: a.Q, ProjectCode: a.Params.Get("project_code"), Active: a.Active, Limit: a.Limit, Offset: a.Offset,
			})
		},
		get: h.teams.Get, create: h.teams.Create, update: h.teams.Update, remove: h.teams.Delete,
		toAPI: teamToAPI,
	})
}

func branchToAPI(b Branch) apigen.Branch {
	letter := apigen.CompanyLetter(b.CompanyLetter)
	return apigen.Branch{
		Code: &b.Code, RegionCode: &b.RegionCode, CompanyLetter: &letter, ManagerName: b.ManagerName,
		City: b.City, Phone: b.Phone, Fax: b.Fax, Email: b.Email, Address: b.Address, ZipCode: b.ZipCode,
		Description: &b.Description, IsActive: &b.Active, CreatedAt: &b.CreatedAt, UpdatedAt: &b.UpdatedAt,
	}
}

func clientToAPI(c Client) apigen.Client {
	return apigen.Client{
		Code: &c.Code, BranchCode: &c.BranchCode, Description: &c.Description, CompanyCode: c.CompanyCode,
		CompanyPrefix: &c.CompanyPrefix, CompanyBidangUsaha: c.CompanyBidangUsaha, DirectorName: c.DirectorName,
		City: c.City, Phone: c.Phone, Fax: c.Fax, Address: c.Address, ZipCode: c.ZipCode, Email: c.Email,
		IsActive: &c.Active, CreatedAt: &c.CreatedAt, UpdatedAt: &c.UpdatedAt,
	}
}

func projectToAPI(p Project) apigen.Project {
	line := apigen.BusinessLine(p.BusinessLine)
	tz := apigen.ProjectTimezone(p.Timezone)
	configured := p.Configured()
	contacts := make([]apigen.EmergencyContact, 0, len(p.EmergencyContacts))
	for _, c := range p.EmergencyContacts {
		contacts = append(contacts, apigen.EmergencyContact{Type: c.Type, Name: c.Name, Phone: c.Phone})
	}
	return apigen.Project{
		Code: &p.Code, ClientCode: &p.ClientCode, Description: &p.Description, CompanyCode: p.CompanyCode,
		CompanyPrefix: p.CompanyPrefix, CompanyBidangUsaha: p.CompanyBidangUsaha, BusinessLine: &line,
		City: p.City, Address: p.Address, Phone: p.Phone, Email: p.Email, Timezone: &tz,
		Latitude: p.Latitude, Longitude: p.Longitude, Configured: &configured,
		AttendanceRadiusMeters: &p.AttendanceRadiusMeters, StrictAttendanceGeofence: &p.StrictAttendanceGeofence,
		GpsTrackingEnabled: &p.GPSTrackingEnabled, PatrolIntervalMinutes: p.PatrolIntervalMinutes,
		EmergencyContacts: &contacts, IsActive: &p.Active, CreatedAt: &p.CreatedAt, UpdatedAt: &p.UpdatedAt,
	}
}

func teamToAPI(t Team) apigen.Team {
	return apigen.Team{
		Code: &t.Code, ProjectCode: &t.ProjectCode, Description: &t.Description, Phone: t.Phone,
		MinPersonnel: &t.MinPersonnel, Notes: t.Notes, ValidateShift: &t.ValidateShift,
		RejectAttendanceAfterTolerance: &t.RejectAttendanceAfterTolerance,
		RelationshipOfficerPersonnelId: t.RelationshipOfficerPersonnelID,
		IsActive:                       &t.Active, CreatedAt: &t.CreatedAt, UpdatedAt: &t.UpdatedAt,
	}
}
