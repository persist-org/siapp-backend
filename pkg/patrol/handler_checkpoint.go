package patrol

import (
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
)

// mountCheckpoints adds the admin routes of the checkpoints, to the group MountAdmin was given
// (access.RequireAdmin is already on it; the service narrows every route to the admin's region).
func (h *Handler) mountCheckpoints(r chi.Router) {
	r.Get("/checkpoints", h.checkpointList)
	r.Post("/checkpoints", h.checkpointCreate)
	r.Get("/checkpoints/{id}", h.checkpointGet)
	r.Patch("/checkpoints/{id}", h.checkpointUpdate)
	r.Delete("/checkpoints/{id}", h.checkpointDelete)
	r.Put("/checkpoints/{id}/actions", h.checkpointActions)
	r.Post("/checkpoints/{id}/rotate-qr", h.checkpointRotateQR)
	r.Get("/checkpoints/{id}/qr", h.checkpointQR)
}

// checkpointID is the id of the path, as text: the service turns it into a uuid, and answers one
// that is not a uuid like a checkpoint that does not exist.
func checkpointID(r *http.Request) string { return chi.URLParam(r, "id") }

func (h *Handler) checkpointList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := CheckpointFilter{ProjectCode: r.URL.Query().Get("project_code"), Q: query.Q, Limit: query.Limit, Offset: query.Offset()}
	var problems []apperr.FieldError
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			problems = append(problems, apperr.FieldError{Field: "is_active", Code: "INVALID", Message: "Harus true atau false."})
		}
		f.Active = &active
	}
	if s := r.URL.Query().Get("zone_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			problems = append(problems, apperr.FieldError{Field: "zone_id", Code: "INVALID", Message: "Harus berupa angka positif."})
		}
		f.ZoneID = n
	}
	if len(problems) > 0 {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(problems...))
		return
	}
	list, total, err := h.service.ListCheckpoints(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.List(w, r, checkpointsToAPI(list), query.WithTotal(total))
}

func (h *Handler) checkpointCreate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in CheckpointInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.CreateCheckpoint(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, checkpointToAPI(got))
}

func (h *Handler) checkpointGet(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	got, err := h.service.GetCheckpoint(r.Context(), p, scope, checkpointID(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, checkpointToAPI(got))
}

func (h *Handler) checkpointUpdate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in CheckpointUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.UpdateCheckpoint(r.Context(), p, scope, checkpointID(r), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, checkpointToAPI(got))
}

func (h *Handler) checkpointDelete(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteCheckpoint(r.Context(), p, scope, checkpointID(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) checkpointActions(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in ActionsReplace
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.ReplaceActions(r.Context(), p, scope, checkpointID(r), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, checkpointToAPI(got))
}

func (h *Handler) checkpointRotateQR(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	got, err := h.service.RotateCheckpointQR(r.Context(), p, scope, checkpointID(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, checkpointToAPI(got))
}

// checkpointQR is GET /admin/checkpoints/{id}/qr: the image itself, not an envelope.
func (h *Handler) checkpointQR(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
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
	qr, err := h.service.CheckpointQRImage(r.Context(), p, scope, checkpointID(r), format)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", qr.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": qr.FileName}))
	// The image holds the checkpoint's secret: nothing between the server and the admin's screen
	// should keep a copy of it.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(qr.Body)
}

// ---------- responses ----------

func checkpointsToAPI(list []Checkpoint) []apigen.PatrolCheckpoint {
	out := make([]apigen.PatrolCheckpoint, 0, len(list))
	for _, c := range list {
		out = append(out, checkpointToAPI(c))
	}
	return out
}

func checkpointToAPI(c Checkpoint) apigen.PatrolCheckpoint {
	actions := actionsToAPI(c.Actions)
	zones := make([]apigen.PatrolCheckpointZone, 0, len(c.Zones))
	for _, z := range c.Zones {
		zones = append(zones, apigen.PatrolCheckpointZone{
			Id: z.ID, Name: z.Name, Type: apigen.PatrolCheckpointZoneType(z.Type), SortOrder: z.SortOrder,
		})
	}
	return apigen.PatrolCheckpoint{
		Id: mustUUID(c.ID), ProjectCode: c.ProjectCode, Name: c.Name, Description: c.Description,
		Latitude: c.Latitude, Longitude: c.Longitude, RadiusMeters: c.RadiusMeters, IsActive: c.Active,
		QrRotatedAt: c.QRRotatedAt, Actions: actions, Zones: zones, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

// mustUUID reads an id the service made from a uuid column, so it always parses; the zero uuid is
// what a corrupt value would show, never a panic in a handler.
func mustUUID(s string) uuid.UUID {
	u, _ := uuid.Parse(s)
	return u
}

// actionsToAPI maps a checklist; an empty one is an empty array, never null.
func actionsToAPI(list []CheckpointAction) []apigen.PatrolCheckpointAction {
	out := make([]apigen.PatrolCheckpointAction, 0, len(list))
	for _, a := range list {
		out = append(out, apigen.PatrolCheckpointAction{
			Id: mustUUID(a.ID), SortOrder: a.SortOrder, Action: a.Action, Instruction: a.Instruction,
			MinPhotos: a.MinPhotos, MaxPhotos: a.MaxPhotos, PhotoRequired: a.PhotoRequired(), IsNoteRequired: a.IsNoteRequired,
		})
	}
	return out
}
