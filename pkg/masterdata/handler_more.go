package masterdata

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// adminCRUD is the five operations of one admin resource and how its rows look in the API. The
// stakeholder types, letterheads and app links behave alike, so their handlers are one piece of
// code: Row is the domain type, API its JSON, In and Upd the create and patch bodies.
type adminCRUD[Row, API, In, Upd any] struct {
	list     func(ctx context.Context, p auth.Principal, scope access.AdminScope, f ListFilter) ([]Row, int, error)
	get      func(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64) (Row, error)
	create   func(ctx context.Context, p auth.Principal, scope access.AdminScope, in In) (Row, error)
	update   func(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64, in Upd) (Row, error)
	remove   func(ctx context.Context, p auth.Principal, scope access.AdminScope, id int64) error
	toAPI    func(Row) API
	notFound error
}

// mountAdminCRUD adds GET/POST on path and GET/PATCH/DELETE on path/{id}.
func mountAdminCRUD[Row, API, In, Upd any](r chi.Router, path string, c adminCRUD[Row, API, In, Upd]) {
	r.Get(path, func(w http.ResponseWriter, r *http.Request) {
		p, scope, f, ok := adminList(w, r)
		if !ok {
			return
		}
		rows, total, err := c.list(r.Context(), p, scope, f.filter)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		out := make([]API, 0, len(rows))
		for _, row := range rows {
			out = append(out, c.toAPI(row))
		}
		httpx.List(w, r, out, f.query.WithTotal(total))
	})
	r.Post(path, func(w http.ResponseWriter, r *http.Request) {
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
	})
	r.Get(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, c.notFound)
			return
		}
		row, err := c.get(r.Context(), p, scope, id)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, c.toAPI(row))
	})
	r.Patch(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, c.notFound)
			return
		}
		var in Upd
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		row, err := c.update(r.Context(), p, scope, id, in)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, c.toAPI(row))
	})
	r.Delete(path+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, scope, ok := caller(w, r)
		if !ok {
			return
		}
		id, ok := idParam(r)
		if !ok {
			httpx.Error(w, r, c.notFound)
			return
		}
		if err := c.remove(r.Context(), p, scope, id); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusOK, nil)
	})
}

func (h *Handler) mountAdminMore(r chi.Router) {
	mountAdminCRUD(r, "/stakeholder-types", adminCRUD[StakeholderType, stakeholderTypeJSON, StakeholderTypeInput, StakeholderTypeUpdate]{
		list: h.service.ListStakeholderTypes, get: h.service.GetStakeholderType, create: h.service.CreateStakeholderType,
		update: h.service.UpdateStakeholderType, remove: h.service.DeleteStakeholderType,
		toAPI: stakeholderTypeToAPI, notFound: ErrStakeholderTypeNotFound,
	})
	mountAdminCRUD(r, "/letterheads", adminCRUD[Letterhead, letterheadJSON, LetterheadInput, LetterheadUpdate]{
		list: h.service.ListLetterheads, get: h.service.GetLetterhead, create: h.service.CreateLetterhead,
		update: h.service.UpdateLetterhead, remove: h.service.DeleteLetterhead,
		toAPI: letterheadToAPI, notFound: ErrLetterheadNotFound,
	})
	mountAdminCRUD(r, "/app-links", adminCRUD[AppLink, appLinkJSON, AppLinkInput, AppLinkUpdate]{
		list: h.service.ListAppLinks, get: h.service.GetAppLink, create: h.service.CreateAppLink,
		update: h.service.UpdateAppLink, remove: h.service.DeleteAppLink,
		toAPI: appLinkToAPI, notFound: ErrAppLinkNotFound,
	})
}

func (h *Handler) mountFieldMore(r chi.Router) {
	r.Get("/api/v1/app-links", h.fieldAppLinks)
	r.Get("/api/v1/projects/{code}/emergency-contacts", h.projectContacts)
}

func (h *Handler) fieldAppLinks(w http.ResponseWriter, r *http.Request) {
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
		ch, err := h.service.FieldAppLinkChanges(r.Context(), p, since)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		up := make([]appLinkJSON, 0, len(ch.Upserted))
		for _, l := range ch.Upserted {
			up = append(up, appLinkToAPI(l))
		}
		httpx.Delta(w, r, up, tombstonesToAPI(ch.Deleted), httpx.DeltaMeta{ServerTime: ch.ServerTime})
		return
	}
	rows, serverTime, err := h.service.FieldAppLinks(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]appLinkJSON, 0, len(rows))
	for _, l := range rows {
		out = append(out, appLinkToAPI(l))
	}
	httpx.ListAt(w, r, out, fullPage(len(out)), serverTime)
}

func (h *Handler) projectContacts(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	rows, err := h.service.FieldContactsOfProject(r.Context(), p, chi.URLParam(r, "code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]contactJSON, 0, len(rows))
	for _, c := range rows {
		out = append(out, contactJSON{
			StakeholderTypeID: c.StakeholderTypeID, TypeCode: c.TypeCode, TypeName: c.TypeName,
			Name: c.Name, Phone: c.Phone, IsDefault: c.Default,
		})
	}
	httpx.List(w, r, out, fullPage(len(out)))
}

// ---------- JSON ----------

type stakeholderTypeJSON struct {
	ID             int64     `json:"id"`
	OrganizationID int64     `json:"organization_id"`
	Code           string    `json:"code"`
	Name           string    `json:"name"`
	DefaultPhone   *string   `json:"default_phone"`
	SortOrder      int       `json:"sort_order"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func stakeholderTypeToAPI(t StakeholderType) stakeholderTypeJSON {
	return stakeholderTypeJSON{
		ID: t.ID, OrganizationID: t.OrganizationID, Code: t.Code, Name: t.Name, DefaultPhone: t.DefaultPhone,
		SortOrder: t.SortOrder, IsActive: t.Active, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

type letterheadJSON struct {
	ID             int64     `json:"id"`
	OrganizationID int64     `json:"organization_id"`
	Name           string    `json:"name"`
	HeaderText     *string   `json:"header_text"`
	FooterText     *string   `json:"footer_text"`
	LogoMediaID    *int64    `json:"logo_media_id"`
	LogoURL        *string   `json:"logo_url"`
	IsDefault      bool      `json:"is_default"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func letterheadToAPI(l Letterhead) letterheadJSON { return letterheadJSON(l) }

type appLinkJSON struct {
	ID             int64     `json:"id"`
	OrganizationID int64     `json:"organization_id"`
	Name           string    `json:"name"`
	IconKey        string    `json:"icon_key"`
	AndroidPackage *string   `json:"android_package"`
	IOSURLScheme   *string   `json:"ios_url_scheme"`
	URL            *string   `json:"url"`
	SortOrder      int       `json:"sort_order"`
	IsActive       bool      `json:"is_active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func appLinkToAPI(l AppLink) appLinkJSON {
	return appLinkJSON{
		ID: l.ID, OrganizationID: l.OrganizationID, Name: l.Name, IconKey: l.IconKey, AndroidPackage: l.AndroidPackage,
		IOSURLScheme: l.IOSURLScheme, URL: l.URL, SortOrder: l.SortOrder, IsActive: l.Active,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
	}
}

type contactJSON struct {
	StakeholderTypeID int64  `json:"stakeholder_type_id"`
	TypeCode          string `json:"type_code"`
	TypeName          string `json:"type_name"`
	Name              string `json:"name"`
	Phone             string `json:"phone"`
	IsDefault         bool   `json:"is_default"`
}
