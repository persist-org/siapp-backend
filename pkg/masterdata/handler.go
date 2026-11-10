package masterdata

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of the master data: decode, call the service, respond.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// MountAdmin adds the admin routes. The caller mounts it inside the admin group, which holds
// access.RequireAdmin once for every admin route; the service then lets only an ORG_ADMIN
// through, as masters are the organization's, not a region's.
func (h *Handler) MountAdmin(r chi.Router) {
	h.mountCategoryAdmin(r, "/report-categories", KindReport)
	h.mountCategoryAdmin(r, "/instruction-categories", KindInstruction)

	r.Get("/panic-types", h.listPanicTypes)
	r.Post("/panic-types", h.createPanicType)
	r.Get("/panic-types/{id}", h.getPanicType)
	r.Patch("/panic-types/{id}", h.updatePanicType)
	r.Delete("/panic-types/{id}", h.deletePanicType)

	h.mountAdminMore(r)
}

// MountField adds the routes of the field app. They need a signed-in person and nothing more: the
// data is the organization's own, and the categories are narrowed by the caller's business lines.
func (h *Handler) MountField(r chi.Router) {
	r.Get("/api/v1/report-categories", h.fieldCategories(KindReport))
	r.Get("/api/v1/instruction-categories", h.fieldCategories(KindInstruction))
	r.Get("/api/v1/panic-types", h.fieldPanicTypes)
	h.mountFieldMore(r)
}

// ---------- admin: categories ----------

func (h *Handler) mountCategoryAdmin(r chi.Router, path string, kind Kind) {
	r.Get(path, func(w http.ResponseWriter, r *http.Request) {
		p, scope, f, ok := adminList(w, r)
		if !ok {
			return
		}
		rows, total, err := h.service.ListCategories(r.Context(), p, scope, kind, f.filter)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out := make([]categoryJSON, 0, len(rows))
		for _, row := range rows {
			out = append(out, categoryToAPI(row))
		}
		httpx.List(w, r, out, f.query.WithTotal(total))
	})
	r.Post(path, func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		var in CategoryInput
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		row, err := h.service.CreateCategory(r.Context(), p, scope, kind, in)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusCreated, categoryToAPI(row))
	})
	r.Get(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, ErrCategoryNotFound)
			return
		}
		row, err := h.service.GetCategory(r.Context(), p, scope, kind, id)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, categoryToAPI(row))
	})
	r.Patch(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, ErrCategoryNotFound)
			return
		}
		var in CategoryUpdate
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		row, err := h.service.UpdateCategory(r.Context(), p, scope, kind, id, in)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, categoryToAPI(row))
	})
	r.Delete(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, ErrCategoryNotFound)
			return
		}
		if err := h.service.DeleteCategory(r.Context(), p, scope, kind, id); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, nil)
	})
}

// ---------- admin: panic types ----------

func (h *Handler) listPanicTypes(w http.ResponseWriter, r *http.Request) {
	p, scope, f, ok := adminList(w, r)
	if !ok {
		return
	}
	rows, total, err := h.service.ListPanicTypes(r.Context(), p, scope, f.filter)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]panicTypeJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, panicTypeToAPI(row))
	}
	httpx.List(w, r, out, f.query.WithTotal(total))
}

func (h *Handler) createPanicType(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in PanicTypeInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.service.CreatePanicType(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, panicTypeToAPI(row))
}

func (h *Handler) getPanicType(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrPanicTypeNotFound)
		return
	}
	row, err := h.service.GetPanicType(r.Context(), p, scope, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, panicTypeToAPI(row))
}

func (h *Handler) updatePanicType(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrPanicTypeNotFound)
		return
	}
	var in PanicTypeUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	row, err := h.service.UpdatePanicType(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, panicTypeToAPI(row))
}

func (h *Handler) deletePanicType(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrPanicTypeNotFound)
		return
	}
	if err := h.service.DeletePanicType(r.Context(), p, scope, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

// ---------- field reads ----------

// sinceParam reads `since`. ok is false when the request has none (a full pull); a bad value is
// answered here.
func sinceParam(w http.ResponseWriter, r *http.Request) (since time.Time, delta, ok bool) {
	s := r.URL.Query().Get("since")
	if s == "" {
		return time.Time{}, false, true
	}
	// A "+07:00" offset sent without URL encoding arrives as " 07:00": put the plus back.
	t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "+", 1))
	if err != nil {
		httpx.Error(w, r, validationError("since", "Harus berformat tanggal-waktu RFC 3339, mis. 2026-06-01T03:00:00Z."))
		return time.Time{}, false, false
	}
	return t, true, true
}

// fullPage is the pagination block of a full pull, which is never paged: a master is a few dozen
// rows and the offline cache wants all of them.
func fullPage(n int) httpx.Page { return httpx.Page{Page: 1, Limit: max(n, 1), Total: n} }

func (h *Handler) fieldCategories(kind Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		if !ok {
			httpx.Error(w, r, apperr.ErrUnauthenticated)
			return
		}
		since, delta, ok := sinceParam(w, r)
		if !ok {
			return
		}
		if delta {
			ch, err := h.service.FieldCategoryChanges(r.Context(), p, kind, since)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			up := make([]categoryJSON, 0, len(ch.Upserted))
			for _, c := range ch.Upserted {
				up = append(up, categoryToAPI(c))
			}
			httpx.Delta(w, r, up, tombstonesToAPI(ch.Deleted), httpx.DeltaMeta{ServerTime: ch.ServerTime})
			return
		}
		rows, serverTime, err := h.service.FieldCategories(r.Context(), p, kind)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out := make([]categoryJSON, 0, len(rows))
		for _, c := range rows {
			out = append(out, categoryToAPI(c))
		}
		httpx.ListAt(w, r, out, fullPage(len(out)), serverTime)
	}
}

func (h *Handler) fieldPanicTypes(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	since, delta, ok := sinceParam(w, r)
	if !ok {
		return
	}
	if delta {
		ch, err := h.service.FieldPanicTypeChanges(r.Context(), p, since)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		up := make([]panicTypeJSON, 0, len(ch.Upserted))
		for _, t := range ch.Upserted {
			up = append(up, panicTypeToAPI(t))
		}
		httpx.Delta(w, r, up, tombstonesToAPI(ch.Deleted), httpx.DeltaMeta{ServerTime: ch.ServerTime})
		return
	}
	rows, serverTime, err := h.service.FieldPanicTypes(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]panicTypeJSON, 0, len(rows))
	for _, t := range rows {
		out = append(out, panicTypeToAPI(t))
	}
	httpx.ListAt(w, r, out, fullPage(len(out)), serverTime)
}

// ---------- shared ----------

// caller is the admin behind an admin request. Every master is the organization's, so only an
// ORG_ADMIN gets past here: a REGION_ADMIN is told so before the body is read, whatever is in it.
// The service checks it again.
func caller(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, bool) {
	p, okP := auth.From(r.Context())
	s, okS := access.ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, access.AdminScope{}, false
	}
	if !s.All {
		httpx.Error(w, r, access.ErrOrgAdminOnly)
		return auth.Principal{}, access.AdminScope{}, false
	}
	return p, s, true
}

type adminListArgs struct {
	query  httpx.ListQuery
	filter ListFilter
}

func adminList(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, adminListArgs, bool) {
	p, scope, ok := caller(w, r)
	if !ok {
		return p, scope, adminListArgs{}, false
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return p, scope, adminListArgs{}, false
	}
	args := adminListArgs{query: query, filter: ListFilter{Q: query.Q, Limit: query.Limit, Offset: query.Offset()}}
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, validationError("is_active", "Harus true atau false."))
			return p, scope, adminListArgs{}, false
		}
		args.filter.Active = &active
	}
	return p, scope, args, true
}

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

// categoryJSON is a category as the API shows it: the contract's CodedMaster (id, name, is_active,
// timestamps) plus the fields a category has and a coded master does not.
type categoryJSON struct {
	ID             int64     `json:"id"`
	OrganizationID int64     `json:"organization_id"`
	Name           string    `json:"name"`
	Urgency        *string   `json:"urgency,omitempty"`
	BusinessLines  []string  `json:"business_lines"`
	SortOrder      int       `json:"sort_order"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func categoryToAPI(c Category) categoryJSON {
	out := categoryJSON{
		ID: c.ID, OrganizationID: c.OrganizationID, Name: c.Name, BusinessLines: c.BusinessLines,
		SortOrder: c.SortOrder, IsActive: c.Active, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if c.Urgency != "" {
		out.Urgency = &c.Urgency
	}
	return out
}

// panicTypeJSON is a panic type as the API shows it: CodedMaster plus its sort order.
type panicTypeJSON struct {
	ID             int64  `json:"id"`
	OrganizationID int64  `json:"organization_id"`
	Code           string `json:"code"`
	Name           string `json:"name"`
	// StakeholderTypeID is who to call for this kind of emergency; null when nobody in particular.
	StakeholderTypeID *int64    `json:"stakeholder_type_id"`
	SortOrder         int       `json:"sort_order"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func panicTypeToAPI(t PanicType) panicTypeJSON {
	return panicTypeJSON{
		ID: t.ID, OrganizationID: t.OrganizationID, Code: t.Code, Name: t.Name, StakeholderTypeID: t.StakeholderTypeID,
		SortOrder: t.SortOrder, IsActive: t.Active, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func tombstonesToAPI(in []Tombstone) []apigen.Tombstone {
	out := make([]apigen.Tombstone, 0, len(in))
	for _, t := range in {
		out = append(out, apigen.Tombstone{Id: t.ID, DeletedAt: t.DeletedAt})
	}
	return out
}
