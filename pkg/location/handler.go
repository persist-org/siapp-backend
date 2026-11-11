package location

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of location: decode, call the service, respond.
type Handler struct {
	service *Service
	limiter *httpx.RateLimiter
}

// uploadLimit is how many uploads one person may make per minute. A phone sends a batch every few
// seconds at most; the limit leaves room for a queue of offline batches being flushed.
var uploadLimit = httpx.PerPeriod(30, time.Minute)

// NewHandler returns the handler. limiter keeps the upload route from being hammered; nil turns
// the limit off (tests of other things).
func NewHandler(service *Service, limiter *httpx.RateLimiter) *Handler {
	return &Handler{service: service, limiter: limiter}
}

// MountField adds the routes of the field app. The upload needs a signed-in person only: whether
// they are on a shift that tracks is the usecase's decision. The three map routes need
// team.positions.view (the DANRU), and the usecase narrows what they return to the regu that
// permission reaches today.
func (h *Handler) MountField(r chi.Router, perms *access.Service) {
	view := perms.Require(access.TeamPositionsView)
	r.Post("/api/v1/location-logs", h.upload)
	r.With(view).Get("/api/v1/teams/{code}/map", h.teamMap)
	r.With(view).Get("/api/v1/personnel-tracking", h.tracking)
	r.With(view).Get("/api/v1/personnels/{id}/position", h.position)
}

// MountAdmin adds the web monitor. access.RequireAdmin is mounted once on the whole group.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/personnel-tracking", h.adminTracking)
}

// pointJSON and batchJSON are the request of POST /location-logs. Pointers make "0" a valid
// coordinate while a missing one still fails `required`.
type pointJSON struct {
	LoggedAt       *time.Time `json:"logged_at" validate:"required"`
	Latitude       *float64   `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64   `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64   `json:"accuracy_meters" validate:"required,gte=0"`
	BatteryPct     *int       `json:"battery_pct" validate:"omitempty,gte=0,lte=100"`
}

type batchJSON struct {
	Points []pointJSON `json:"points" validate:"required,min=1,dive"`
}

func (p pointJSON) point() Point {
	return Point{LoggedAt: *p.LoggedAt, Latitude: *p.Latitude, Longitude: *p.Longitude, AccuracyMeters: *p.AccuracyMeters, BatteryPct: p.BatteryPct}
}

// upload is POST /location-logs.
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	// Per person, not per address: phones of one site share a NAT address.
	if h.limiter != nil {
		key := strconv.FormatInt(p.OrganizationID, 10) + ":" + strconv.FormatInt(p.PersonnelID, 10)
		if allowed, retryAfter := h.limiter.Allow(r.Context(), "location-upload", key, uploadLimit); !allowed {
			httpx.TooManyRequests(w, r, retryAfter)
			return
		}
	}
	var in batchJSON
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if len(in.Points) > MaxBatch {
		httpx.Error(w, r, ErrBatchTooLarge)
		return
	}
	points := make([]Point, len(in.Points))
	for i, pt := range in.Points {
		points[i] = pt.point()
	}
	res, err := h.service.Ingest(r.Context(), p, points)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, apigen.LocationBatchResult{Accepted: res.Accepted, Rejected: res.Rejected})
}

// reach returns what team.positions.view covers for the caller today.
func reach(w http.ResponseWriter, r *http.Request) (auth.Principal, access.Resolution, bool) {
	p, okP := auth.From(r.Context())
	res, okR := access.From(r.Context())
	if !okP || !okR {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, access.Resolution{}, false
	}
	return p, res, true
}

// teamMap is GET /teams/{code}/map.
func (h *Handler) teamMap(w http.ResponseWriter, r *http.Request) {
	p, res, ok := reach(w, r)
	if !ok {
		return
	}
	code := chi.URLParam(r, "code")
	// ⚠ A regu outside the caller's reach, one that does not exist and another organization's
	// all answer 403: the answer must not tell them apart.
	if !res.HasTeam(code) {
		httpx.Error(w, r, access.ErrPermissionDenied)
		return
	}
	h.respondList(w, r, p.OrganizationID, Filter{TeamCodes: []string{code}})
}

// tracking is GET /personnel-tracking, the DANRU's view of everyone in their reach, or of one
// project or regu inside it.
func (h *Handler) tracking(w http.ResponseWriter, r *http.Request) {
	p, res, ok := reach(w, r)
	if !ok {
		return
	}
	f := Filter{TeamCodes: res.TeamCodes}
	if project := r.URL.Query().Get("project_code"); project != "" {
		if !res.HasProject(project) {
			httpx.Error(w, r, access.ErrPermissionDenied)
			return
		}
		f.ProjectCodes = []string{project}
	}
	if team := r.URL.Query().Get("team_code"); team != "" {
		if !res.HasTeam(team) {
			httpx.Error(w, r, access.ErrPermissionDenied)
			return
		}
		f.TeamCodes = []string{team}
	}
	h.respondList(w, r, p.OrganizationID, f)
}

// adminTracking is GET /admin/personnel-tracking, the web monitor.
func (h *Handler) adminTracking(w http.ResponseWriter, r *http.Request) {
	p, okP := auth.From(r.Context())
	scope, okS := access.ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	project, team := r.URL.Query().Get("project_code"), r.URL.Query().Get("team_code")
	if project == "" {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "project_code", Code: "required", Message: "Wajib diisi."}))
		return
	}
	// A project outside the admin's region is answered like one that has nobody online: the
	// region is decided by the code alone, so nothing is read for a project they may not see.
	if !scope.AllowsRegion(access.RegionOfCode(project)) {
		httpx.Error(w, r, access.ErrPermissionDenied)
		return
	}
	f := Filter{ProjectCodes: []string{project}}
	if team != "" {
		f.TeamCodes = []string{team}
	}
	h.respondList(w, r, p.OrganizationID, f)
}

// position is GET /personnels/{id}/position.
func (h *Handler) position(w http.ResponseWriter, r *http.Request) {
	p, res, ok := reach(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, access.ErrPermissionDenied)
		return
	}
	pos, err := h.service.Of(r.Context(), p.OrganizationID, res.TeamCodes, id)
	switch {
	case errors.Is(err, ErrNotInScope):
		httpx.Error(w, r, access.ErrPermissionDenied)
	case err != nil:
		httpx.Error(w, r, err)
	default:
		httpx.OK(w, r, http.StatusOK, toAPI(pos))
	}
}

func (h *Handler) respondList(w http.ResponseWriter, r *http.Request, orgID int64, f Filter) {
	positions, err := h.service.OnMap(r.Context(), orgID, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.PersonnelPosition, 0, len(positions))
	for _, pos := range positions {
		out = append(out, toAPI(pos))
	}
	httpx.OK(w, r, http.StatusOK, out)
}

func toAPI(p Position) apigen.PersonnelPosition {
	return apigen.PersonnelPosition{
		PersonnelId: p.PersonnelID, FullName: p.FullName, PhotoUrl: p.PhotoURL, Role: apigen.PersonnelPositionRole(p.Role),
		PositionName: p.PositionName, TeamCode: p.TeamCode, ProjectCode: p.ProjectCode,
		Online: p.Online, LastSeenAt: p.LastSeenAt,
		Latitude: p.Latitude, Longitude: p.Longitude, AccuracyMeters: p.AccuracyMeters, LoggedAt: p.LoggedAt,
	}
}

// frameJSON is the data of a location.update frame. logged_at is optional on the socket: a live
// frame is "now" unless the phone says otherwise.
type frameJSON struct {
	LoggedAt       *time.Time `json:"logged_at"`
	Latitude       *float64   `json:"latitude" validate:"required,gte=-90,lte=90"`
	Longitude      *float64   `json:"longitude" validate:"required,gte=-180,lte=180"`
	AccuracyMeters *float64   `json:"accuracy_meters" validate:"required,gte=0"`
	BatteryPct     *int       `json:"battery_pct" validate:"omitempty,gte=0,lte=100"`
}

// IngestFrame handles the data of a WebSocket location.update frame: one point, through the same
// Ingest as the REST upload. realtime.Handler calls it; it returns an *apperr.Error for anything
// the client did wrong, whose reason the socket sends back in an error frame.
func (s *Service) IngestFrame(ctx context.Context, p auth.Principal, data json.RawMessage) error {
	var in frameJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if len(data) == 0 || dec.Decode(&in) != nil {
		return apperr.ErrBadRequest.WithMessage("location.update harus berisi latitude, longitude dan accuracy_meters.")
	}
	if err := httpx.Validate(&in); err != nil {
		return err
	}
	at := s.clock.Now()
	if in.LoggedAt != nil {
		at = *in.LoggedAt
	}
	res, err := s.Ingest(ctx, p, []Point{{LoggedAt: at, Latitude: *in.Latitude, Longitude: *in.Longitude, AccuracyMeters: *in.AccuracyMeters, BatteryPct: in.BatteryPct}})
	if err != nil {
		return err
	}
	if res.Rejected > 0 {
		// A batch reports this in its result; a single live frame has no result, so say it.
		return apperr.ErrValidation.WithFields(apperr.FieldError{Field: "logged_at", Code: "out_of_window", Message: "Titik terlalu lama, terlalu jauh ke depan, atau direkam di luar jam dinas."})
	}
	return nil
}
