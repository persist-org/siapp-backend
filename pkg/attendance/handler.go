package attendance

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/access"
	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/timeutil"
)

// Handler is the HTTP side of attendance: decode, call the service, respond.
type Handler struct {
	service    *Service
	access     *access.Service
	idempotent func(http.Handler) http.Handler
}

// NewHandler returns the handler. idempotent is the Idempotency-Key middleware
// (idem.Service.Middleware(true)): a check-in that is retried after a lost answer must give back
// the same record and write no second outbox row.
func NewHandler(service *Service, perms *access.Service, idempotent func(http.Handler) http.Handler) *Handler {
	return &Handler{service: service, access: perms, idempotent: idempotent}
}

// MountField adds the routes of the field app. Every one needs attendance.checkin, through a
// membership that is active today; the usecase asks again for the date of the shift it is about.
func (h *Handler) MountField(r chi.Router) {
	checkin := h.access.Require(access.AttendanceCheckin)
	r.With(checkin).Get("/api/v1/attendances/today", h.today)
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/selfie/check-in", record(h.service.SelfieCheckIn, http.StatusCreated))
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/selfie/check-out", record(h.service.SelfieCheckOut, http.StatusOK))
	// Idempotent too: a retried "update pulang" must not count twice or send two events.
	r.With(checkin, h.idempotent).Patch("/api/v1/attendances/{id}/check-out", h.updateCheckOut)

	// QR (07.2): the guard scans with attendance.checkin like any check-in; the DANRU's phone asks
	// for the code to show with attendance.qr.display.
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/qr/check-in", record(h.service.QRCheckIn, http.StatusCreated))
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/qr/check-out", record(h.service.QRCheckOut, http.StatusOK))
	r.With(h.access.Require(access.AttendanceQRDisplay)).Get("/api/v1/attendances/qr/display", h.qrDisplay)

	// History (07.4). The calendar is the caller's own. The detail has no route permission: whether
	// the caller may read that record (the owner, or a DANRU whose scope reaches its regu) is decided
	// by the usecase, and everyone else gets 404.
	r.With(checkin).Get("/api/v1/attendances/me", h.me)
	r.Get("/api/v1/attendances/{id}", h.detail)

	// 07.6: the DANRU marking a team member absent, and the regu monitor. The monitor has no route
	// permission either, same reason as the detail: List itself refuses with PERMISSION_DENIED
	// when the caller's group/absence view grant reaches no regu at all.
	r.With(h.access.Require(access.AttendanceAbsenceNew), h.idempotent).Post("/api/v1/attendances/absent", record(h.service.MarkAbsent, http.StatusCreated))
	r.Get("/api/v1/attendances", h.list)
	r.With(h.access.Require(access.AttendanceGroupView), h.idempotent).Post("/api/v1/attendances/group", h.group)
}

// MountAdmin adds the routes of /api/v1/admin (Part C): a correction and the admin monitor. Both
// are reached by an ORG_ADMIN or the REGION_ADMIN of the record's region (access.RequireAdmin is
// mounted once on the whole group in cmd/api); the usecase checks the reach itself, outside it a
// record is 404, the same as leave/decide.go's admin side.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/attendances", h.adminList)
	r.Patch("/attendances/{id}", h.adminCorrect)
}

// anyAdmin is the admin behind an admin request, of either role.
func anyAdmin(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, bool) {
	p, okP := auth.From(r.Context())
	s, okS := access.ScopeFrom(r.Context())
	if !okP || !okS {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return auth.Principal{}, access.AdminScope{}, false
	}
	return p, s, true
}

// adminList is GET /admin/attendances.
func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := Filter{
		TeamCode: r.URL.Query().Get("team_code"), ProjectCode: r.URL.Query().Get("project_code"),
		Limit: query.Limit, Offset: query.Offset(),
	}
	if f.From, ok = dateQueryParam(w, r, "date_from"); !ok {
		return
	}
	if f.To, ok = dateQueryParam(w, r, "date_to"); !ok {
		return
	}
	page, err := h.service.ListForAdmin(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]attendanceJSON, 0, len(page.Attendances))
	for _, a := range page.Attendances {
		out = append(out, attendanceToAPI(a))
	}
	httpx.List(w, r, out, query.WithTotal(page.Total))
}

// adminCorrect is PATCH /admin/attendances/{id}.
func (h *Handler) adminCorrect(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	var in CorrectionInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	a, err := h.service.Correct(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, attendanceToAPI(a))
}

// ---------- JSON ----------

type attendanceJSON struct {
	ID                  int64         `json:"id"`
	PersonnelID         int64         `json:"personnel_id"`
	TeamCode            string        `json:"team_code"`
	ProjectCode         string        `json:"project_code"`
	WorkDate            timeutil.Date `json:"work_date"`
	ShiftID             int64         `json:"shift_id"`
	ShiftStartsAt       string        `json:"shift_starts_at"`
	ShiftEndsAt         string        `json:"shift_ends_at"`
	Timezone            string        `json:"timezone"`
	Status              string        `json:"status"`
	CheckInAt           *string       `json:"check_in_at"`
	CheckInMethod       *string       `json:"check_in_method"`
	CheckInDistance     *int          `json:"check_in_distance_meters"`
	CheckInWithinArea   *bool         `json:"check_in_within_area"`
	LateMinutes         int           `json:"late_minutes"`
	CheckOutAt          *string       `json:"check_out_at"`
	CheckOutMethod      *string       `json:"check_out_method"`
	CheckOutDistance    *int          `json:"check_out_distance_meters"`
	CheckOutWithinArea  *bool         `json:"check_out_within_area"`
	CheckOutUpdateCount int           `json:"check_out_update_count"`
	LeaveRequestID      *int64        `json:"leave_request_id"`
	PersonelSyncStatus  string        `json:"personel_sync_status"`
	CreatedAt           string        `json:"created_at"`
	UpdatedAt           string        `json:"updated_at"`
}

func attendanceToAPI(a Attendance) attendanceJSON {
	return attendanceJSON{
		ID: a.ID, PersonnelID: a.PersonnelID, TeamCode: a.TeamCode, ProjectCode: a.ProjectCode, WorkDate: a.WorkDate,
		ShiftID: a.ShiftID, ShiftStartsAt: timeutil.FormatTimestamp(a.StartsAt), ShiftEndsAt: timeutil.FormatTimestamp(a.EndsAt),
		Timezone: a.Timezone, Status: string(a.Status),
		CheckInAt: stamp(a.CheckInAt), CheckInMethod: methodName(a.CheckInMethod),
		CheckInDistance: a.CheckInDistance, CheckInWithinArea: a.CheckInInArea, LateMinutes: a.LateMinutes,
		CheckOutAt: stamp(a.CheckOutAt), CheckOutMethod: methodName(a.CheckOutMethod),
		CheckOutDistance: a.CheckOutDistance, CheckOutWithinArea: a.CheckOutInArea,
		CheckOutUpdateCount: a.CheckOutUpdateCount, LeaveRequestID: a.LeaveRequestID, PersonelSyncStatus: string(a.PersonelSyncStatus),
		CreatedAt: timeutil.FormatTimestamp(a.CreatedAt), UpdatedAt: timeutil.FormatTimestamp(a.UpdatedAt),
	}
}

// todayJSON is one shift of GET /attendances/today.
type todayJSON struct {
	ShiftRef     string        `json:"shift_ref"`
	AttendanceID *int64        `json:"attendance_id"`
	WorkDate     timeutil.Date `json:"work_date"`
	ProjectCode  string        `json:"project_code"`
	ProjectName  string        `json:"project_name"`
	TeamCode     string        `json:"team_code"`
	TeamName     string        `json:"team_name"`
	ShiftName    string        `json:"shift_name"`
	StartsAt     string        `json:"starts_at"`
	EndsAt       string        `json:"ends_at"`
	Timezone     string        `json:"timezone"`
	// Status is the record's status, null while nothing is captured for the shift.
	Status     *string `json:"status"`
	CheckInAt  *string `json:"check_in_at"`
	CheckOutAt *string `json:"check_out_at"`
}

func todayToAPI(s CheckableShift) todayJSON {
	out := todayJSON{
		ShiftRef: s.Ref, WorkDate: s.Key.WorkDate, ProjectCode: s.Project.Code, ProjectName: s.Project.Description,
		TeamCode: s.Team.Code, TeamName: s.Team.Description, ShiftName: s.ShiftName,
		StartsAt: timeutil.FormatTimestamp(s.StartsAt), EndsAt: timeutil.FormatTimestamp(s.EndsAt), Timezone: s.Location.String(),
	}
	if s.Record != nil {
		status := string(s.Record.Status)
		out.AttendanceID, out.Status = &s.Record.ID, &status
		out.CheckInAt, out.CheckOutAt = stamp(s.Record.CheckInAt), stamp(s.Record.CheckOutAt)
	}
	return out
}

// historyDayJSON is one day of GET /attendances/me.
type historyDayJSON struct {
	WorkDate timeutil.Date      `json:"work_date"`
	Shifts   []historyShiftJSON `json:"shifts"`
}

type historyShiftJSON struct {
	AttendanceID  *int64  `json:"attendance_id"`
	ShiftName     string  `json:"shift_name"`
	TeamCode      string  `json:"team_code"`
	TeamName      string  `json:"team_name"`
	Status        string  `json:"status"`
	CheckInAt     *string `json:"check_in_at"`
	CheckInMethod *string `json:"check_in_method"`
	CheckOutAt    *string `json:"check_out_at"`
}

// detailJSON is GET /attendances/{id}: the record and what the detail screen shows around it.
type detailJSON struct {
	attendanceJSON
	ProjectName    string      `json:"project_name"`
	TeamName       string      `json:"team_name"`
	ShiftName      string      `json:"shift_name"`
	DanruNames     []string    `json:"danru_names"`
	CheckInSelfie  *selfieJSON `json:"check_in_selfie"`
	CheckOutSelfie *selfieJSON `json:"check_out_selfie"`
}

// selfieJSON is a photo's view: the URL is signed and expires, so it is never stored.
type selfieJSON struct {
	ID          int64   `json:"id"`
	URL         string  `json:"url"`
	ContentType string  `json:"content_type"`
	CapturedAt  *string `json:"captured_at"`
}

func selfieToAPI(v *media.View) *selfieJSON {
	if v == nil {
		return nil
	}
	return &selfieJSON{ID: v.ID, URL: v.URL, ContentType: v.ContentType, CapturedAt: stamp(v.CapturedAt)}
}

func stamp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := timeutil.FormatTimestamp(*t)
	return &s
}

func methodName(m *Method) *string {
	if m == nil {
		return nil
	}
	s := string(*m)
	return &s
}

// ---------- handlers ----------

func (h *Handler) today(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	shifts, err := h.service.Today(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]todayJSON, 0, len(shifts))
	for _, s := range shifts {
		out = append(out, todayToAPI(s))
	}
	httpx.OK(w, r, http.StatusOK, out)
}

// me is GET /attendances/me?month=YYYY-MM.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	month := r.URL.Query().Get("month")
	if month == "" {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "month", Code: "REQUIRED", Message: "month wajib diisi (YYYY-MM)."}))
		return
	}
	days, err := h.service.Month(r.Context(), p, month)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]historyDayJSON, 0, len(days))
	for _, d := range days {
		day := historyDayJSON{WorkDate: d.WorkDate, Shifts: make([]historyShiftJSON, 0, len(d.Shifts))}
		for _, s := range d.Shifts {
			day.Shifts = append(day.Shifts, historyShiftJSON{
				AttendanceID: s.AttendanceID, ShiftName: s.ShiftName, TeamCode: s.TeamCode, TeamName: s.TeamName,
				Status: s.Status, CheckInAt: stamp(s.CheckInAt), CheckInMethod: methodName(s.CheckInMethod), CheckOutAt: stamp(s.CheckOutAt),
			})
		}
		out = append(out, day)
	}
	httpx.OK(w, r, http.StatusOK, out)
}

// list is GET /attendances: the regu monitor. No route permission: List itself refuses with
// PERMISSION_DENIED when the caller's group/absence view grant reaches no regu, or when team_code
// asks for one outside it.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := Filter{
		TeamCode: r.URL.Query().Get("team_code"), ProjectCode: r.URL.Query().Get("project_code"),
		Limit: query.Limit, Offset: query.Offset(),
	}
	if f.From, ok = dateQueryParam(w, r, "date_from"); !ok {
		return
	}
	if f.To, ok = dateQueryParam(w, r, "date_to"); !ok {
		return
	}
	page, err := h.service.List(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]attendanceJSON, 0, len(page.Attendances))
	for _, a := range page.Attendances {
		out = append(out, attendanceToAPI(a))
	}
	httpx.List(w, r, out, query.WithTotal(page.Total))
}

// dateQueryParam reads an optional YYYY-MM-DD query parameter. ok is false when the answer is
// already written (a malformed date is one VALIDATION_ERROR).
func dateQueryParam(w http.ResponseWriter, r *http.Request, name string) (*timeutil.Date, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, true
	}
	d, err := timeutil.ParseDate(s)
	if err != nil {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: name, Code: "INVALID", Message: name + " harus berformat YYYY-MM-DD.",
		}))
		return nil, false
	}
	return &d, true
}

// groupEntryJSON is one entry's outcome of POST /attendances/group.
type groupEntryJSON struct {
	PersonnelID int64           `json:"personnel_id"`
	Status      string          `json:"status"`
	Attendance  *attendanceJSON `json:"attendance"`
	Error       *groupErrorJSON `json:"error"`
}

type groupErrorJSON struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func groupEntryToAPI(r GroupEntryResult) groupEntryJSON {
	out := groupEntryJSON{PersonnelID: r.PersonnelID, Status: string(r.Status)}
	if r.Attendance != nil {
		a := attendanceToAPI(*r.Attendance)
		out.Attendance = &a
	}
	if r.Error != nil {
		var ae *apperr.Error
		if errors.As(r.Error, &ae) {
			out.Error = &groupErrorJSON{Reason: ae.Reason, Message: ae.Message}
		} else {
			out.Error = &groupErrorJSON{Message: r.Error.Error()}
		}
	}
	return out
}

// group is POST /attendances/group: the DANRU's group-present muster. A bad entry does not fail
// the batch (201 with a per-entry result); only a permission or infrastructure failure answers
// with an error envelope.
func (h *Handler) group(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	var in GroupInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	results, err := h.service.GroupPresent(r.Context(), p, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]groupEntryJSON, 0, len(results))
	for _, res := range results {
		out = append(out, groupEntryToAPI(res))
	}
	httpx.OK(w, r, http.StatusCreated, out)
}

// detail is GET /attendances/{id}.
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	d, err := h.service.Detail(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, detailJSON{
		attendanceJSON: attendanceToAPI(d.Attendance),
		ProjectName:    d.ProjectName, TeamName: d.TeamName, ShiftName: d.ShiftName, DanruNames: d.DanruNames,
		CheckInSelfie: selfieToAPI(d.CheckInSelfie), CheckOutSelfie: selfieToAPI(d.CheckOutSelfie),
	})
}

// record is the handler of every check-in and check-out, selfie or QR: they differ only in the body
// they decode, the usecase and the status of a success.
func record[In any](do func(ctx context.Context, p auth.Principal, in In) (Attendance, error), status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		if !ok {
			httpx.Error(w, r, apperr.ErrUnauthenticated)
			return
		}
		var in In
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		a, err := do(r.Context(), p, in)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, status, attendanceToAPI(a))
	}
}

// qrDisplay is GET /attendances/qr/display?team_code=&direction=IN|OUT: the code a DANRU's phone
// shows, asked again about every 30 s.
//
// The permission is held through the route's resolution; the regu is checked against it here: the
// caller can show the code of a regu their grant reaches (their own, or a project's with a PROJECT
// grant), not of any regu. Asking for a regu outside it is 403, whether or not the regu exists.
func (h *Handler) qrDisplay(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	team, dir := r.URL.Query().Get("team_code"), Direction(r.URL.Query().Get("direction"))
	var fields []apperr.FieldError
	if team == "" {
		fields = append(fields, apperr.FieldError{Field: "team_code", Code: "REQUIRED", Message: "team_code wajib diisi."})
	}
	if !dir.Valid() {
		fields = append(fields, apperr.FieldError{Field: "direction", Code: "INVALID", Message: "direction harus IN atau OUT."})
	}
	if len(fields) > 0 {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(fields...))
		return
	}
	res, ok := access.From(r.Context())
	if !ok || !res.HasTeam(team) {
		httpx.Error(w, r, access.ErrPermissionDenied)
		return
	}
	token, err := h.service.QRDisplay(p, team, dir)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	// A code that rotates every minute is not for a cache or a shared proxy to keep.
	w.Header().Set("Cache-Control", "no-store")
	httpx.OK(w, r, http.StatusOK, qrTokenJSON{Token: token.Token, ExpiresAt: timeutil.FormatTimestamp(token.ExpiresAt)})
}

type qrTokenJSON struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func (h *Handler) updateCheckOut(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	var in UpdateInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	a, err := h.service.UpdateSelfieCheckOut(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, attendanceToAPI(a))
}
