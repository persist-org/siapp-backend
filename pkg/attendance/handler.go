package attendance

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/access"
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
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/selfie/check-in", h.selfie(h.service.SelfieCheckIn, http.StatusCreated))
	r.With(checkin, h.idempotent).Post("/api/v1/attendances/selfie/check-out", h.selfie(h.service.SelfieCheckOut, http.StatusOK))
	// Idempotent too: a retried "update pulang" must not count twice or send two events.
	r.With(checkin, h.idempotent).Patch("/api/v1/attendances/{id}/check-out", h.updateCheckOut)
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
		CheckOutUpdateCount: a.CheckOutUpdateCount, PersonelSyncStatus: string(a.PersonelSyncStatus),
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

// selfie is the handler of the selfie check-in and check-out: they differ only in the usecase and
// the status of a success.
func (h *Handler) selfie(do func(ctx context.Context, p auth.Principal, in SelfieInput) (Attendance, error), status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		if !ok {
			httpx.Error(w, r, apperr.ErrUnauthenticated)
			return
		}
		var in SelfieInput
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
