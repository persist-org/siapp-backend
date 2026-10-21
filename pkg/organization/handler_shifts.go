package organization

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/pii"
	"siapp/pkg/platform/timeutil"
)

// mountShifts adds the shift, shift group, assignment and schedule routes (Phase 02.5).
func (h *Handler) mountShifts(r chi.Router) {
	r.Get("/shifts", h.listShifts)
	r.Post("/shifts", h.createShift)
	r.Get("/shifts/{id}", h.getShift)
	r.Patch("/shifts/{id}", h.updateShift)
	r.Delete("/shifts/{id}", h.deleteShift)

	r.Get("/shift-groups", h.listShiftGroups)
	r.Post("/shift-groups", h.createShiftGroup)
	r.Get("/shift-groups/{id}", h.getShiftGroup)
	r.Patch("/shift-groups/{id}", h.updateShiftGroup)
	r.Delete("/shift-groups/{id}", h.deleteShiftGroup)

	r.Get("/personnel/{personnel_id}/shift-assignment", h.getAssignment)
	r.Put("/personnel/{personnel_id}/shift-assignment", h.setAssignment)
	r.Post("/personnel/{personnel_id}/schedules", h.createSchedules)

	r.Get("/schedules", h.listSchedules)
	r.Post("/schedules/bulk-delete", h.bulkDeleteSchedules)
	r.Get("/schedules/{id}", h.getSchedule)
	r.Patch("/schedules/{id}", h.updateSchedule)
	r.Delete("/schedules/{id}", h.deleteSchedule)
}

// pathID reads a positive numeric path parameter. Anything else is the resource's not-found:
// an id that cannot exist does not exist.
func pathID(w http.ResponseWriter, r *http.Request, name string, notFound *apperr.Error) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, notFound)
		return 0, false
	}
	return id, true
}

func (h *Handler) listShifts(w http.ResponseWriter, r *http.Request) {
	p, _, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := ShiftFilter{Q: query.Q, Limit: query.Limit, Offset: query.Offset()}
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, validationError("is_active", "Harus true atau false."))
			return
		}
		f.Active = &active
	}
	rows, total, err := h.shifts.ListShifts(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.Shift, 0, len(rows))
	for _, s := range rows {
		out = append(out, shiftToAPI(s))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) getShift(w http.ResponseWriter, r *http.Request) {
	p, _, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftNotFound)
	if !ok {
		return
	}
	s, err := h.shifts.GetShift(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, shiftToAPI(s))
}

func (h *Handler) createShift(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in ShiftInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s, err := h.shifts.CreateShift(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, shiftToAPI(s))
}

func (h *Handler) updateShift(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftNotFound)
	if !ok {
		return
	}
	var in ShiftUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s, err := h.shifts.UpdateShift(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, shiftToAPI(s))
}

func (h *Handler) deleteShift(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftNotFound)
	if !ok {
		return
	}
	if err := h.shifts.DeleteShift(r.Context(), p, scope, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) listShiftGroups(w http.ResponseWriter, r *http.Request) {
	p, _, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, total, err := h.shifts.ListShiftGroups(r.Context(), p, query.Q, query.Limit, query.Offset())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.ShiftGroup, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupToAPI(g))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) getShiftGroup(w http.ResponseWriter, r *http.Request) {
	p, _, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftGroupNotFound)
	if !ok {
		return
	}
	g, err := h.shifts.GetShiftGroup(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, groupToAPI(g))
}

func (h *Handler) createShiftGroup(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in ShiftGroupInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	g, err := h.shifts.CreateShiftGroup(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, groupToAPI(g))
}

func (h *Handler) updateShiftGroup(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftGroupNotFound)
	if !ok {
		return
	}
	var in ShiftGroupUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	g, err := h.shifts.UpdateShiftGroup(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, groupToAPI(g))
}

func (h *Handler) deleteShiftGroup(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrShiftGroupNotFound)
	if !ok {
		return
	}
	if err := h.shifts.DeleteShiftGroup(r.Context(), p, scope, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) getAssignment(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	personnelID, ok := pathID(w, r, "personnel_id", ErrPersonnelNotFound)
	if !ok {
		return
	}
	a, err := h.schedules.GetAssignment(r.Context(), p, scope, personnelID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, assignmentToAPI(a))
}

func (h *Handler) setAssignment(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	personnelID, ok := pathID(w, r, "personnel_id", ErrPersonnelNotFound)
	if !ok {
		return
	}
	var in AssignmentInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	res, err := h.schedules.SetAssignment(r.Context(), p, scope, personnelID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	a := assignmentToAPI(res.Assignment)
	httpx.OK(w, r, http.StatusOK, apigen.ShiftAssignmentResult{
		Assignment: &a, From: apiDate(res.From), Deleted: &res.Deleted, Generated: &res.Generated,
	})
}

func (h *Handler) createSchedules(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	personnelID, ok := pathID(w, r, "personnel_id", ErrPersonnelNotFound)
	if !ok {
		return
	}
	var in ScheduleEntries
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	rows, err := h.schedules.Create(r.Context(), p, scope, personnelID, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.Schedule, 0, len(rows))
	for _, s := range rows {
		out = append(out, scheduleToAPI(s))
	}
	httpx.OK(w, r, http.StatusCreated, out)
}

func (h *Handler) listSchedules(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	values := r.URL.Query()
	f := ScheduleFilter{Q: query.Q, TeamCode: values.Get("team_code"), Limit: query.Limit, Offset: query.Offset()}
	for name, dst := range map[string]*timeutil.Date{"date_from": &f.From, "date_to": &f.To} {
		if s := values.Get(name); s != "" {
			d, err := timeutil.ParseDate(s)
			if err != nil {
				httpx.Error(w, r, validationError(name, "Harus berformat YYYY-MM-DD."))
				return
			}
			*dst = d
		}
	}
	if s := values.Get("personnel_id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 1 {
			httpx.Error(w, r, validationError("personnel_id", "Harus angka positif."))
			return
		}
		f.PersonnelID = id
	}
	rows, total, err := h.schedules.List(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.Schedule, 0, len(rows))
	for _, s := range rows {
		out = append(out, scheduleToAPI(s))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) getSchedule(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrScheduleNotFound)
	if !ok {
		return
	}
	s, err := h.schedules.Get(r.Context(), p, scope, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, scheduleToAPI(s))
}

func (h *Handler) updateSchedule(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrScheduleNotFound)
	if !ok {
		return
	}
	var in ScheduleUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s, err := h.schedules.Update(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, scheduleToAPI(s))
}

func (h *Handler) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r, "id", ErrScheduleNotFound)
	if !ok {
		return
	}
	if err := h.schedules.Delete(r.Context(), p, scope, id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, nil)
}

func (h *Handler) bulkDeleteSchedules(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in BulkDelete
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	n, err := h.schedules.DeleteMany(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, apigen.ScheduleBulkDeleteResult{Deleted: &n})
}

func shiftToAPI(s Shift) apigen.Shift {
	start, end, overnight := formatClock(s.StartMinute), formatClock(s.EndMinute), s.Overnight()
	return apigen.Shift{
		Id: &s.ID, Name: &s.Name, StartTime: &start, EndTime: &end, IsOff: &s.IsOff, Overnight: &overnight,
		ToleranceMinutes: &s.ToleranceMinutes, IsActive: &s.Active, CreatedAt: &s.CreatedAt, UpdatedAt: &s.UpdatedAt,
	}
}

func groupToAPI(g ShiftGroup) apigen.ShiftGroup {
	days := make([]apigen.ShiftGroupDay, 0, len(g.Days))
	for _, d := range g.Days {
		weekday := apigen.Weekday(Weekdays[d.Weekday-1])
		shift := shiftToAPI(d.Shift)
		// A day carries the shift's name and times; its other fields belong to /shifts.
		shift.IsActive, shift.ToleranceMinutes, shift.CreatedAt, shift.UpdatedAt = nil, nil, nil, nil
		days = append(days, apigen.ShiftGroupDay{Weekday: &weekday, Shift: &shift})
	}
	return apigen.ShiftGroup{Id: &g.ID, Name: &g.Name, Days: &days, CreatedAt: &g.CreatedAt, UpdatedAt: &g.UpdatedAt}
}

func assignmentToAPI(a Assignment) apigen.ShiftAssignment {
	t := apigen.ScheduleType(a.ScheduleType)
	out := apigen.ShiftAssignment{
		PersonnelId: &a.PersonnelID, ScheduleType: &t, ShiftGroupId: a.ShiftGroupID,
		EffectiveDate: apiDate(a.EffectiveDate), UpdatedAt: &a.UpdatedAt,
	}
	if a.GeneratedUntil != nil {
		out.GeneratedUntil = apiDate(*a.GeneratedUntil)
	}
	return out
}

func scheduleToAPI(s ScheduleRow) apigen.Schedule {
	start, end, overnight := formatClock(s.Shift.StartMinute), formatClock(s.Shift.EndMinute), s.Shift.Overnight()
	out := apigen.Schedule{
		Id: &s.ID, PersonnelId: &s.PersonnelID, FullName: &s.FullName, WorkDate: apiDate(s.WorkDate),
		Shift: &apigen.ScheduleShift{
			Id: &s.Shift.ID, Name: &s.Shift.Name, StartTime: &start, EndTime: &end, IsOff: &s.Shift.IsOff, Overnight: &overnight,
		},
		ShiftGroupId: s.ShiftGroupID, CreatedBy: s.CreatedBy, UpdatedBy: s.UpdatedBy, CreatedAt: &s.CreatedAt, UpdatedAt: &s.UpdatedAt,
	}
	if s.NIK != "" {
		masked := pii.MaskNIK(s.NIK)
		out.Nik = &masked
	}
	return out
}

func apiDate(d timeutil.Date) *openapi_types.Date { return &openapi_types.Date{Time: d.Time()} }
