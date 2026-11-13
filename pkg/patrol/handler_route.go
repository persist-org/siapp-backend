package patrol

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
)

// MountRoutes adds the routes of the patrol route to the field app. As with the zone routes, whether
// the caller may start a patrol in a project, or read a patrol, is the service's decision per request.
func (h *Handler) MountRoutes(r chi.Router) {
	r.With(h.idempotent).Post("/api/v1/patrols/start", h.startPatrol)
	// chi matches a fixed segment before a parameter, so /patrols/active is not patrol "active".
	r.Get("/api/v1/patrols/active", h.activePatrol)
	r.Get("/api/v1/patrols", h.listPatrols)
	r.Get("/api/v1/patrols/{id}", h.getPatrol)
	r.With(h.idempotent).Post("/api/v1/patrols/{id}/points", h.patrolPoints)
	r.With(h.idempotent).Post("/api/v1/patrols/{id}/end", h.endPatrol)
}

// patrolIDParam reads the patrol id of the path. An id that is not a positive number cannot name a
// patrol, so it is answered like one that does not exist.
func patrolIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		return 0, ErrPatrolNotFound
	}
	return id, nil
}

func (h *Handler) startPatrol(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in StartInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// local_id must be the Idempotency-Key, and started_at inside the window an offline app may claim.
	if err := in.Offline().Check(r, h.service.Now()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.service.Start(r.Context(), p, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.OK(w, r, status, routeToAPI(res.Route))
}

func (h *Handler) patrolPoints(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := patrolIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in PointsInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.service.AddPoints(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, apigen.PatrolRoutePointsResult{Accepted: res.Accepted, Duplicates: res.Duplicates})
}

func (h *Handler) endPatrol(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := patrolIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in EndInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.End(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, routeDetailToAPI(got))
}

func (h *Handler) activePatrol(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	got, err := h.service.Active(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A list of zero or one, so the app reads "none" as an empty array and not as an error.
	out := []apigen.PatrolRouteDetail{}
	if got != nil {
		out = append(out, routeDetailToAPI(*got))
	}
	httpx.OK(w, r, http.StatusOK, out)
}

func (h *Handler) getPatrol(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := patrolIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.GetRoute(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, routeDetailToAPI(got))
}

func (h *Handler) listPatrols(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	day, err := dateQuery(r, "work_date")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	q := r.URL.Query()
	f := RouteFilter{
		ProjectCode: q.Get("project_code"), TeamCode: q.Get("team_code"), WorkDate: day,
		Limit: query.Limit, Offset: query.Offset(),
	}
	var problems []apperr.FieldError
	if s := q.Get("status"); s != "" {
		if s != RouteActive && s != RouteEnded && s != RouteAutoEnded {
			problems = append(problems, apperr.FieldError{Field: "status", Code: "INVALID", Message: "Harus salah satu dari: ACTIVE, ENDED, AUTO_ENDED."})
		}
		f.Status = s
	}
	if s := q.Get("personnel_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			problems = append(problems, apperr.FieldError{Field: "personnel_id", Code: "INVALID", Message: "Harus berupa angka positif."})
		}
		f.PersonnelID = n
	}
	if len(problems) > 0 {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(problems...))
		return
	}
	got, total, err := h.service.ListRoutes(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.PatrolRoute, 0, len(got))
	for _, g := range got {
		out = append(out, routeToAPI(g))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func routeToAPI(r Route) apigen.PatrolRoute {
	out := apigen.PatrolRoute{
		Id: r.ID, LocalId: openapi_types.UUID(uuid.MustParse(r.LocalID)), PersonnelId: r.PersonnelID,
		TeamCode: r.TeamCode, ProjectCode: r.ProjectCode, Status: apigen.PatrolRouteStatus(r.Status),
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, WorkDate: openapi_types.Date{Time: r.WorkDate.Time()},
		DistanceMeters: r.DistanceMeters, PointCount: r.PointCount, ScanCount: r.ScanCount,
		SubmittedOffline: r.SubmittedOffline, LastPointAt: r.LastPointAt,
	}
	if r.PersonnelName != "" {
		name := r.PersonnelName
		out.PersonnelName = &name
	}
	return out
}

func routeDetailToAPI(d RouteDetail) apigen.PatrolRouteDetail {
	base := routeToAPI(d.Route)
	path := make([]apigen.PatrolRoutePathPoint, 0, len(d.Path))
	for _, p := range d.Path {
		path = append(path, apigen.PatrolRoutePathPoint{Latitude: p.Lat, Longitude: p.Lng, At: p.At, AccuracyMeters: p.Accuracy})
	}
	scans := make([]apigen.PatrolRouteScan, 0, len(d.Scans))
	for _, s := range d.Scans {
		scans = append(scans, apigen.PatrolRouteScan{
			Id: s.ID, ZoneId: s.ZoneID, ZoneName: s.ZoneName, OccurredAt: s.OccurredAt, Latitude: s.Latitude,
			Longitude: s.Longitude, Status: apigen.PatrolRouteScanStatus(s.Status), Subject: s.Subject,
		})
	}
	return apigen.PatrolRouteDetail{
		Id: base.Id, LocalId: base.LocalId, PersonnelId: base.PersonnelId, PersonnelName: base.PersonnelName,
		TeamCode: base.TeamCode, ProjectCode: base.ProjectCode, Status: apigen.PatrolRouteDetailStatus(base.Status),
		StartedAt: base.StartedAt, EndedAt: base.EndedAt, WorkDate: base.WorkDate, DistanceMeters: base.DistanceMeters,
		PointCount: base.PointCount, ScanCount: base.ScanCount, SubmittedOffline: base.SubmittedOffline,
		LastPointAt: base.LastPointAt, Route: lineString(d.Path), Path: path, Scans: scans,
	}
}

// lineString is the path as a GeoJSON LineString, which a map library draws as a line directly.
//
// ⚠ GeoJSON positions are [longitude, latitude], longitude FIRST (RFC 7946 §3.1.1), the reverse of the
// usual "lat, lng" and of every other pair in this API. Swapping them draws the line in the wrong
// hemisphere without any error. A LineString needs two positions or more: one point is written twice,
// and no point at all is an empty array (RFC 7946 allows an empty geometry).
func lineString(path []RoutePoint) apigen.GeoJsonLineString {
	coords := make([][]float64, 0, len(path)+1)
	for _, p := range path {
		coords = append(coords, []float64{p.Lng, p.Lat})
	}
	if len(coords) == 1 {
		coords = append(coords, coords[0])
	}
	return apigen.GeoJsonLineString{Type: apigen.LineString, Coordinates: coords}
}
