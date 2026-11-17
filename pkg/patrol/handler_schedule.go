package patrol

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
)

// mountSchedules adds the admin routes of the patrol schedules, to the group MountAdmin was given
// (access.RequireAdmin is already on it; the service narrows every route to the admin's region).
func (h *Handler) mountSchedules(r chi.Router) {
	r.Get("/patrol-schedules", h.scheduleAdminList)
	r.Post("/patrol-schedules", h.scheduleCreate)
	r.Patch("/patrol-schedules/{id}", h.scheduleUpdate)
	r.Delete("/patrol-schedules/{id}", h.scheduleCancel)
}

// scheduleStatuses are the values of ?status=.
var scheduleStatuses = []string{ScheduleScheduled, ScheduleInProgress, ScheduleCompleted, ScheduleMissed, ScheduleCancelled}

// scheduleFilter reads the filters the two lists share.
func scheduleFilter(r *http.Request) (ScheduleFilter, error) {
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		return ScheduleFilter{}, err
	}
	f := ScheduleFilter{ProjectCode: r.URL.Query().Get("project_code"), Limit: query.Limit, Offset: query.Offset()}
	var problems []apperr.FieldError
	for _, name := range []string{"date_from", "date_to"} {
		d, err := dateQuery(r, name)
		if err != nil {
			problems = append(problems, apperr.FieldError{Field: name, Code: "INVALID", Message: "Harus berformat YYYY-MM-DD."})
			continue
		}
		if name == "date_from" {
			f.DateFrom = d
		} else {
			f.DateTo = d
		}
	}
	for _, name := range []string{"zone_id", "personnel_id"} {
		s := r.URL.Query().Get(name)
		if s == "" {
			continue
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			problems = append(problems, apperr.FieldError{Field: name, Code: "INVALID", Message: "Harus berupa angka positif."})
			continue
		}
		if name == "zone_id" {
			f.ZoneID = n
		} else {
			f.PersonnelID = n
		}
	}
	if s := r.URL.Query().Get("status"); s != "" {
		valid := false
		for _, v := range scheduleStatuses {
			valid = valid || s == v
		}
		if !valid {
			problems = append(problems, apperr.FieldError{Field: "status", Code: "INVALID", Message: "Harus salah satu dari: SCHEDULED, IN_PROGRESS, COMPLETED, MISSED, CANCELLED."})
		}
		f.Status = s
	}
	if len(problems) > 0 {
		return ScheduleFilter{}, apperr.ErrValidation.WithFields(problems...)
	}
	return f, nil
}

func (h *Handler) scheduleAdminList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	f, err := scheduleFilter(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	list, total, err := h.service.ListSchedulesAdmin(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	query, _ := httpx.ParseListQuery(r)
	httpx.List(w, r, schedulesToAPI(list), query.WithTotal(total))
}

func (h *Handler) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in ScheduleInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.CreateSchedules(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, schedulesToAPI(got))
}

func (h *Handler) scheduleUpdate(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	var in ScheduleUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.UpdateSchedule(r.Context(), p, scope, chi.URLParam(r, "id"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, scheduleToAPI(got))
}

func (h *Handler) scheduleCancel(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := admin(w, r)
	if !ok {
		return
	}
	if err := h.service.CancelSchedule(r.Context(), p, scope, chi.URLParam(r, "id")); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

// scheduleList is GET /patrol-schedules, the field read.
func (h *Handler) scheduleList(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	f, err := scheduleFilter(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A project filter is the admin's; the field read narrows by the caller's reach instead.
	f.ProjectCode = ""
	list, total, err := h.service.ListSchedules(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	query, _ := httpx.ParseListQuery(r)
	httpx.List(w, r, schedulesToAPI(list), query.WithTotal(total))
}

// scheduleGet is GET /patrol-schedules/{id}.
func (h *Handler) scheduleGet(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	got, err := h.service.GetSchedule(r.Context(), p, chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, scheduleToAPI(got))
}

func schedulesToAPI(list []Schedule) []apigen.PatrolSchedule {
	out := make([]apigen.PatrolSchedule, 0, len(list))
	for _, s := range list {
		out = append(out, scheduleToAPI(s))
	}
	return out
}

func scheduleToAPI(s Schedule) apigen.PatrolSchedule {
	out := apigen.PatrolSchedule{
		Id: mustUUID(s.ID), ProjectCode: s.ProjectCode, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
		WorkDate: openapi_types.Date{Time: s.WorkDate.Time()}, Notes: s.Notes, Status: apigen.PatrolScheduleStatus(s.Status),
		CancelledAt: s.CancelledAt,
	}
	out.Zone.Id, out.Zone.Name, out.Zone.Type = s.ZoneID, s.ZoneName, apigen.PatrolScheduleZoneType(s.ZoneType)
	out.Personnel.Id, out.Personnel.FullName = s.PersonnelID, s.PersonnelName
	out.ScanCount, out.StartDelayMinutes = s.ScanCount, s.StartDelayMinutes
	if s.Patrol != nil {
		asJSON(map[string]any{"id": s.Patrol.ID, "status": s.Patrol.Status}, &out.Patrol)
	}
	return out
}
