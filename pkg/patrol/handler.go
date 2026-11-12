package patrol

import (
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of the patrol module: decode, call the service, respond.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// MountField adds the routes of the field app. Reading zones needs a signed-in person who serves
// in the zone's project: the service decides that per request, from today's memberships.
func (h *Handler) MountField(r chi.Router) {
	r.Get("/api/v1/zones", h.fieldList)
	r.Get("/api/v1/zones/{id}", h.fieldGet)
}

// MountAdmin adds the web admin routes. access.RequireAdmin is mounted once on the whole group,
// and the service narrows every route to the admin's region.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/zones", h.adminList)
	r.Post("/zones", h.adminCreate)
	r.Get("/zones/{id}", h.adminGet)
	r.Patch("/zones/{id}", h.adminUpdate)
	r.Delete("/zones/{id}", h.adminDelete)
	r.Put("/zones/{id}/checklist", h.adminChecklist)
	r.Post("/zones/{id}/rotate-qr", h.adminRotateQR)
	r.Get("/zones/{id}/qr", h.adminQR)
}

func admin(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, bool) {
	p, okP := auth.From(r.Context())
	scope, okS := access.ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, access.AdminScope{}, false
	}
	return p, scope, true
}

// idParam reads the zone id of the path. An id that is not a positive number cannot name a zone,
// so it is answered like one that does not exist.
func idParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		return 0, ErrZoneNotFound
	}
	return id, nil
}

// typeQuery reads the optional ?type= filter.
func typeQuery(r *http.Request) (string, error) {
	t := r.URL.Query().Get("type")
	if t != "" && t != TypePatrol && t != TypeGuard {
		return "", apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "type", Code: "INVALID", Message: "Harus salah satu dari: PATROL, GUARD.",
		})
	}
	return t, nil
}

// ---------- admin ----------

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	typ, err := typeQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := ListFilter{
		ProjectCode: r.URL.Query().Get("project_code"), TeamCode: r.URL.Query().Get("team_code"), Type: typ,
		Q: query.Q, Limit: query.Limit, Offset: query.Offset(),
	}
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "is_active", Code: "INVALID", Message: "Harus true atau false.",
			}))
			return
		}
		f.Active = &active
	}
	zones, total, err := h.service.ListAdmin(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.List(w, r, zonesToAPI(zones), query.WithTotal(total))
}

func (h *Handler) adminCreate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in ZoneInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.Create(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, zoneToAPI(zone))
}

func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.GetAdmin(r.Context(), p, scope, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
}

func (h *Handler) adminUpdate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in ZoneUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.Update(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
}

func (h *Handler) adminDelete(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.service.Delete(r.Context(), p, scope, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) adminChecklist(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in ChecklistReplace
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.ReplaceChecklist(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
}

func (h *Handler) adminRotateQR(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.RotateQR(r.Context(), p, scope, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
}

// adminQR is GET /admin/zones/{id}/qr: the image itself, not an envelope.
func (h *Handler) adminQR(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	switch format {
	case "":
		format = FormatPNG
	case FormatPNG, FormatSVG:
	default:
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "format", Code: "INVALID", Message: "Harus salah satu dari: png, svg.",
		}))
		return
	}
	qr, err := h.service.QRImage(r.Context(), p, scope, id, format)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", qr.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": qr.FileName}))
	// The image holds the zone's secret: nothing between the server and the admin's screen should
	// keep a copy of it.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(qr.Body)
}

// ---------- field ----------

func (h *Handler) fieldList(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	typ, err := typeQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	zones, err := h.service.ListForField(r.Context(), p, FieldFilter{
		ProjectCode: r.URL.Query().Get("project_code"), TeamCode: r.URL.Query().Get("team_code"), Type: typ,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// The whole list is one page: a person serves in a handful of projects, each with a handful
	// of zones, and the app caches the lot.
	httpx.List(w, r, zonesToAPI(zones), httpx.Page{Page: 1, Limit: max(len(zones), 1), Total: len(zones)})
}

func (h *Handler) fieldGet(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.GetForField(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
}

// ---------- responses ----------

func zonesToAPI(zones []Zone) []apigen.PatrolZone {
	out := make([]apigen.PatrolZone, 0, len(zones))
	for _, z := range zones {
		out = append(out, zoneToAPI(z))
	}
	return out
}

func zoneToAPI(z Zone) apigen.PatrolZone {
	items := make([]apigen.PatrolChecklistItem, 0, len(z.Checklist))
	for _, it := range z.Checklist {
		items = append(items, itemToAPI(it))
	}
	out := apigen.PatrolZone{
		Id: z.ID, ProjectCode: z.ProjectCode, TeamCode: z.TeamCode, Type: apigen.PatrolZoneType(z.Type), Name: z.Name,
		Alias: z.Alias, Description: z.Description, PopupText: z.PopupText, Latitude: z.Latitude, Longitude: z.Longitude,
		RadiusMeters: z.RadiusMeters, MinVisit: z.MinVisit, PhotoRequired: z.PhotoRequired, IsActive: z.Active,
		Checklist: items, CreatedAt: z.CreatedAt, UpdatedAt: z.UpdatedAt,
	}
	if z.StartDate != nil {
		out.StartDate = &openapi_types.Date{Time: z.StartDate.Time()}
	}
	return out
}

func itemToAPI(it ChecklistItem) apigen.PatrolChecklistItem {
	out := apigen.PatrolChecklistItem{
		Id: it.ID, SortOrder: it.SortOrder, Label: it.Label, HelpText: it.HelpText,
		AnswerType: apigen.PatrolChecklistItemAnswerType(it.AnswerType), Required: it.Required,
		PhotoMode: apigen.PatrolChecklistItemPhotoMode(it.PhotoMode), PhotoMax: it.PhotoMax,
		NoteMode: apigen.PatrolChecklistItemNoteMode(it.NoteMode), AttachWhen: apigen.PatrolChecklistItemAttachWhen(it.AttachWhen),
	}
	if len(it.Options) > 0 {
		opts := make([]apigen.PatrolChecklistOption, 0, len(it.Options))
		for _, o := range it.Options {
			opts = append(opts, apigen.PatrolChecklistOption{Value: o.Value, Label: o.Label})
		}
		out.Options = &opts
	}
	return out
}
