package patrol

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/timeutil"
)

// DeclineInput is the body of POST /scans/{id}/decline. The note is the DANRU's feedback to the
// guard (the APK's danru_patrol_feedback), so a decline without one is refused.
type DeclineInput struct {
	Note string `json:"note" validate:"required,notblank,max=1000"`
}

// BulkApproveInput is the body of POST /scans/approve.
type BulkApproveInput struct {
	IDs []int64 `json:"ids" validate:"required,min=1,max=50,dive,min=1"`
}

func scanIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		return 0, ErrScanNotFound
	}
	return id, nil
}

// dateQuery reads an optional YYYY-MM-DD query parameter.
func dateQuery(r *http.Request, name string) (*timeutil.Date, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil //nolint:nilnil // absent is not an error
	}
	d, err := timeutil.ParseDate(raw)
	if err != nil {
		return nil, apperr.ErrValidation.WithFields(apperr.FieldError{Field: name, Code: "INVALID", Message: "Harus berformat YYYY-MM-DD."})
	}
	return &d, nil
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
	}
	return p, ok
}

// progress is GET /zones/progress.
func (h *Handler) progress(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	typ, err := typeQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	day, err := dateQuery(r, "work_date")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.Progress(r.Context(), p, ProgressFilter{
		ProjectCode: r.URL.Query().Get("project_code"), TeamCode: r.URL.Query().Get("team_code"), Type: typ, WorkDate: day,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.ZoneProgressItem, 0, len(got))
	for _, g := range got {
		item := apigen.ZoneProgressItem{Required: g.Required, Submitted: g.Submitted, Approved: g.Approved, Declined: g.Declined}
		item.Zone.Id, item.Zone.ProjectCode, item.Zone.TeamCode = g.Zone.ID, g.Zone.ProjectCode, g.Zone.TeamCode
		item.Zone.Type, item.Zone.Name, item.Zone.Alias = apigen.ZoneProgressItemZoneType(g.Zone.Type), g.Zone.Name, g.Zone.Alias
		item.Zone.MinVisit = g.Zone.MinVisit
		out = append(out, item)
	}
	httpx.OK(w, r, http.StatusOK, out)
}

// scanners is GET /zones/{id}/scanners.
func (h *Handler) scanners(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := idParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	day, err := dateQuery(r, "work_date")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.Scanners(r.Context(), p, id, r.URL.Query().Get("team_code"), day)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]apigen.ZoneScanner, 0, len(got))
	for _, g := range got {
		out = append(out, apigen.ZoneScanner{PersonnelId: g.PersonnelID, FullName: g.FullName, SubmittedCount: g.SubmittedCount, ApprovedCount: g.ApprovedCount})
	}
	httpx.OK(w, r, http.StatusOK, out)
}

// zoneScans is GET /zones/{id}/scans.
func (h *Handler) zoneScans(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
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
	day, err := dateQuery(r, "work_date")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f := ScanFilter{WorkDate: day, Limit: query.Limit, Offset: query.Offset()}
	var problems []apperr.FieldError
	if s := r.URL.Query().Get("status"); s != "" {
		if s != "SUBMITTED" && s != "APPROVED" && s != "DECLINED" {
			problems = append(problems, apperr.FieldError{Field: "status", Code: "INVALID", Message: "Harus salah satu dari: SUBMITTED, APPROVED, DECLINED."})
		}
		f.Status = s
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
	got, total, err := h.service.ListZoneScans(r.Context(), p, id, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.List(w, r, scansToAPI(got), query.WithTotal(total))
}

// getScan is GET /scans/{id}.
func (h *Handler) getScan(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := scanIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, err := h.service.GetScan(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, scanToAPI(got))
}

// pending is GET /patrol/pending-approvals.
func (h *Handler) pending(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	got, total, err := h.service.Pending(r.Context(), p, r.URL.Query().Get("team_code"), query.Limit, query.Offset())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.List(w, r, scansToAPI(got), query.WithTotal(total))
}

// approve is POST /scans/{id}/approve. A scan that was decided already is 409: the caller asked
// for this one scan and it is not theirs to change any more.
func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := scanIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.service.Approve(r.Context(), p, []int64{id})
	h.respondSingle(w, r, d, err)
}

// decline is POST /scans/{id}/decline.
func (h *Handler) decline(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, err := scanIDParam(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var in DeclineInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.service.Decline(r.Context(), p, []int64{id}, in.Note)
	h.respondSingle(w, r, d, err)
}

func (h *Handler) respondSingle(w http.ResponseWriter, r *http.Request, d Decision, err error) {
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if len(d.Skipped) > 0 || len(d.Scans) != 1 {
		httpx.Error(w, r, ErrScanAlreadyDecided)
		return
	}
	httpx.OK(w, r, http.StatusOK, scanToAPI(d.Scans[0]))
}

// approveMany is POST /scans/approve.
func (h *Handler) approveMany(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in BulkApproveInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	d, err := h.service.Approve(r.Context(), p, in.IDs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	skipped := d.Skipped
	if skipped == nil {
		skipped = []int64{}
	}
	httpx.OK(w, r, http.StatusOK, apigen.BulkApproveResult{Approved: scansToAPI(d.Scans), Skipped: skipped})
}
