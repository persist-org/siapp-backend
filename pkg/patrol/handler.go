package patrol

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/timeutil"
)

// Handler is the HTTP side of the patrol module: decode, call the service, respond.
type Handler struct {
	service    *Service
	idempotent func(http.Handler) http.Handler
}

// NewHandler returns the handler. idempotent is the Idempotency-Key middleware (required) that the
// routes that write for the guard sit behind: a scan or a check-in that is retried after a lost
// answer must give the same answer, not a second record.
func NewHandler(service *Service, idempotent func(http.Handler) http.Handler) *Handler {
	return &Handler{service: service, idempotent: idempotent}
}

// MountField adds the routes of the field app. They need a signed-in person; whether they serve in
// the zone's project, and may scan it, is the service's decision per request, from the memberships
// of the day the request is about.
func (h *Handler) MountField(r chi.Router) {
	r.Get("/api/v1/zones", h.fieldList)
	// chi matches a fixed segment before a parameter, so /zones/progress is not read as zone "progress".
	r.Get("/api/v1/zones/progress", h.progress)
	r.Get("/api/v1/zones/{id}", h.fieldGet)
	r.Get("/api/v1/zones/{id}/scanners", h.scanners)
	r.Get("/api/v1/zones/{id}/scans", h.zoneScans)
	r.Get("/api/v1/scans/{id}", h.getScan)
	r.Post("/api/v1/scans/{id}/approve", h.approve)
	r.Post("/api/v1/scans/{id}/decline", h.decline)
	r.Post("/api/v1/scans/approve", h.approveMany)
	r.Get("/api/v1/patrol/pending-approvals", h.pending)
	r.Get("/api/v1/patrol-schedules", h.scheduleList)
	r.Get("/api/v1/patrol-schedules/{id}", h.scheduleGet)
	r.With(h.idempotent).Post("/api/v1/zones/{id}/scan", h.scan)
	r.With(h.idempotent).Post("/api/v1/zones/{id}/skip", h.skip)
	r.With(h.idempotent).Post("/api/v1/zones/{id}/check-in", h.checkIn)
	r.With(h.idempotent).Post("/api/v1/zones/{id}/check-out", h.checkOut)
	r.Get("/api/v1/zones/{id}/live", h.live)
	r.Get("/api/v1/zones/{id}/tracking", h.tracking)
	h.MountRoutes(r)
}

// MountAdmin adds the web admin routes. access.RequireAdmin is mounted once on the whole group,
// and the service narrows every route to the admin's region.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/zones", h.adminList)
	r.Post("/zones", h.adminCreate)
	r.Get("/zones/{id}", h.adminGet)
	r.Patch("/zones/{id}", h.adminUpdate)
	r.Delete("/zones/{id}", h.adminDelete)
	r.Put("/zones/{id}/checkpoints", h.adminCheckpoints)
	h.mountCheckpoints(r)
	h.mountSchedules(r)
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

func (h *Handler) adminCheckpoints(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in CheckpointsReplace
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	zone, err := h.service.AssignCheckpoints(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, zoneToAPI(zone))
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

// scan is POST /zones/{id}/scan.
func (h *Handler) scan(w http.ResponseWriter, r *http.Request) {
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
	var in ScanInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// local_id must be the Idempotency-Key, and occurred_at inside the window an offline app may claim.
	if err := in.Offline().Check(r, h.service.Now()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.service.Scan(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.OK(w, r, status, scanToAPI(res.Scan))
}

// skip is POST /zones/{id}/skip: a checkpoint of the round that could not be scanned (Phase 10.17).
func (h *Handler) skip(w http.ResponseWriter, r *http.Request) {
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
	var in SkipInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := in.Offline().Check(r, h.service.Now()); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.service.Skip(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.OK(w, r, status, scanToAPI(res.Scan))
}

func (h *Handler) checkIn(w http.ResponseWriter, r *http.Request) {
	h.presence(w, r, h.service.CheckIn, http.StatusCreated)
}

func (h *Handler) checkOut(w http.ResponseWriter, r *http.Request) {
	h.presence(w, r, h.service.CheckOut, http.StatusOK)
}

// presence is the shared body of check-in and check-out.
func (h *Handler) presence(w http.ResponseWriter, r *http.Request,
	call func(ctx context.Context, p auth.Principal, zoneID int64, in PresenceInput) (CheckIn, error), status int,
) {
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
	var in PresenceInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := call(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, status, checkInToAPI(got))
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
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
	got, err := h.service.Live(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, checkInsToAPI(got))
}

func (h *Handler) tracking(w http.ResponseWriter, r *http.Request) {
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
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := TrackingFilter{TeamCode: r.URL.Query().Get("team_code"), Limit: query.Limit, Offset: query.Offset()}
	var problems []apperr.FieldError
	for _, name := range []string{"from", "to"} {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			problems = append(problems, apperr.FieldError{Field: name, Code: "REQUIRED", Message: "Wajib diisi."})
			continue
		}
		d, err := timeutil.ParseDate(raw)
		if err != nil {
			problems = append(problems, apperr.FieldError{Field: name, Code: "INVALID", Message: "Harus berformat YYYY-MM-DD."})
			continue
		}
		if name == "from" {
			f.From = d
		} else {
			f.To = d
		}
	}
	if s := r.URL.Query().Get("personnel_id"); s != "" {
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
	got, total, err := h.service.Tracking(r.Context(), p, id, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.List(w, r, checkInsToAPI(got), query.WithTotal(total))
}

// ---------- responses ----------

func scansToAPI(list []Scan) []apigen.PatrolScan {
	out := make([]apigen.PatrolScan, 0, len(list))
	for _, sc := range list {
		out = append(out, scanToAPI(sc))
	}
	return out
}

// photosToAPI maps photo views. A nil list stays nil (the scan was not read, it was just made, so
// there are no URLs to show); an empty one is an empty array.
func photosToAPI(photos []Photo) *[]apigen.PatrolScanPhoto {
	if photos == nil {
		return nil
	}
	out := make([]apigen.PatrolScanPhoto, 0, len(photos))
	for _, p := range photos {
		item := apigen.PatrolScanPhoto{Id: p.ID, Url: p.URL, ContentType: p.ContentType, CapturedAt: p.CapturedAt}
		if p.CaptureSource != "" {
			src := apigen.PatrolScanPhotoCaptureSource(p.CaptureSource)
			item.CaptureSource = &src
		}
		out = append(out, item)
	}
	return &out
}

func scanToAPI(sc Scan) apigen.PatrolScan {
	actions := make([]apigen.PatrolScanAction, 0, len(sc.Actions))
	for _, a := range sc.Actions {
		out := apigen.PatrolScanAction{
			ActionId: mustUUID(a.ActionID), Action: a.Action, Instruction: a.Instruction, Note: a.Note,
			Photos: []apigen.PatrolScanPhoto{},
		}
		if p := photosToAPI(a.Photos); p != nil {
			out.Photos = *p
		}
		if len(a.DeviceCondition) > 0 && string(a.DeviceCondition) != "null" {
			var dc map[string]interface{}
			if err := json.Unmarshal(a.DeviceCondition, &dc); err == nil {
				out.DeviceCondition = &dc
			}
		}
		actions = append(actions, out)
	}
	local, _ := uuid.Parse(sc.LocalID)
	var name *string
	if sc.PersonnelName != "" {
		name = &sc.PersonnelName
	}
	checkpoint := apigen.PatrolScanCheckpoint{Id: mustUUID(sc.Checkpoint.ID), Name: sc.Checkpoint.Name, SortOrder: sc.Checkpoint.SortOrder}
	out := apigen.PatrolScan{
		Id: sc.ID, LocalId: local, ZoneId: sc.ZoneID, ZoneType: apigen.PatrolScanZoneType(sc.ZoneType),
		CheckpointId: checkpoint.Id, Checkpoint: checkpoint,
		PersonnelId: sc.PersonnelID, TeamCode: sc.TeamCode, ProjectCode: sc.ProjectCode, OccurredAt: sc.OccurredAt,
		ReceivedAt: sc.ReceivedAt, WorkDate: openapi_types.Date{Time: sc.WorkDate.Time()}, Latitude: sc.Latitude,
		Longitude: sc.Longitude, AccuracyMeters: sc.AccuracyMeters, DistanceMeters: sc.DistanceMeters, Subject: sc.Subject,
		Notes: sc.Notes, Actions: actions, Status: apigen.PatrolScanStatus(sc.Status), PatrolId: sc.PatrolID,
		SubmittedOffline: sc.SubmittedOffline, CreatedAt: sc.CreatedAt,
		DecidedById: sc.DecidedByID, DecidedAt: sc.DecidedAt, DecisionNote: sc.DecisionNote,
		PersonnelName: name, LocationFlag: apigen.PatrolLocationFlag(sc.LocationFlag), Outcome: apigen.PatrolScanOutcome(sc.Outcome),
		Photos: photosToAPI(sc.Photos),
	}
	if sc.SkipReason != nil {
		reason := apigen.PatrolScanSkipReason(*sc.SkipReason)
		out.SkipReason = &reason
	}
	return out
}

func checkInToAPI(c CheckIn) apigen.ZoneCheckIn {
	out := apigen.ZoneCheckIn{
		Id: c.ID, ZoneId: c.ZoneID, CheckpointId: uuidPtr(c.CheckpointID), PersonnelId: c.PersonnelID, TeamCode: c.TeamCode, ShiftId: c.ShiftID,
		CheckInAt: c.CheckInAt, CheckInLatitude: c.CheckInLat, CheckInLongitude: c.CheckInLng,
		CheckOutAt: c.CheckOutAt, CheckOutLatitude: c.CheckOutLat, CheckOutLongitude: c.CheckOutLng,
	}
	if c.FullName != "" {
		name := c.FullName
		out.FullName = &name
	}
	return out
}

// uuidPtr is the uuid of an optional id kept as text (the service's), for a nullable field of the API.
func uuidPtr(s *string) *uuid.UUID {
	if s == nil {
		return nil
	}
	u := mustUUID(*s)
	return &u
}

func checkInsToAPI(list []CheckIn) []apigen.ZoneCheckIn {
	out := make([]apigen.ZoneCheckIn, 0, len(list))
	for _, c := range list {
		out = append(out, checkInToAPI(c))
	}
	return out
}

func zonesToAPI(zones []Zone) []apigen.PatrolZone {
	out := make([]apigen.PatrolZone, 0, len(zones))
	for _, z := range zones {
		out = append(out, zoneToAPI(z))
	}
	return out
}

func zoneToAPI(z Zone) apigen.PatrolZone {
	out := apigen.PatrolZone{
		Id: z.ID, ProjectCode: z.ProjectCode, TeamCode: z.TeamCode, Type: apigen.PatrolZoneType(z.Type), Name: z.Name,
		Alias: z.Alias, Description: z.Description, PopupText: z.PopupText, Latitude: z.Latitude, Longitude: z.Longitude,
		RadiusMeters: z.RadiusMeters, MinVisit: z.MinVisit, IsActive: z.Active,
		CheckpointCount: z.CheckpointCount, CreatedAt: z.CreatedAt, UpdatedAt: z.UpdatedAt,
	}
	if z.StartDate != nil {
		out.StartDate = &openapi_types.Date{Time: z.StartDate.Time()}
	}
	// A list leaves checkpoints out (nil); a detail has them, possibly none (an empty array).
	if z.Checkpoints != nil {
		cps := make([]apigen.PatrolZoneCheckpoint, 0, len(z.Checkpoints))
		for _, c := range z.Checkpoints {
			cps = append(cps, zoneCheckpointToAPI(c))
		}
		out.Checkpoints = &cps
	}
	if z.NextCheckpointID != nil {
		next := mustUUID(*z.NextCheckpointID)
		out.NextCheckpointId = &next
	}
	return out
}

func zoneCheckpointToAPI(c ZoneCheckpoint) apigen.PatrolZoneCheckpoint {
	out := apigen.PatrolZoneCheckpoint{
		Id: mustUUID(c.ID), Name: c.Name, SortOrder: c.SortOrder, IsActive: c.Active,
		Latitude: c.Latitude, Longitude: c.Longitude, RadiusMeters: c.RadiusMeters, MyScans: c.MyScans, Scannable: c.Scannable,
	}
	if c.Actions != nil {
		actions := actionsToAPI(c.Actions)
		out.Actions = &actions
	}
	return out
}
