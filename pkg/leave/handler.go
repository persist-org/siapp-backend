package leave

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of leave: decode, call the service, respond.
type Handler struct {
	service *Service
	access  *access.Service
}

// NewHandler returns the handler.
func NewHandler(service *Service, perms *access.Service) *Handler {
	return &Handler{service: service, access: perms}
}

// MountAdmin adds the admin routes. The caller mounts it inside the admin group, which holds
// access.RequireAdmin once for every admin route. The categories, quotas and switches are the
// organization's, so only an ORG_ADMIN gets through; the ledger of a person is also open to the
// REGION_ADMIN of a person in their region.
func (h *Handler) MountAdmin(r chi.Router) {
	r.Get("/permission-categories", h.listCategories)
	r.Post("/permission-categories", h.createCategory)
	r.Get("/permission-categories/{code}", h.getCategory)
	r.Patch("/permission-categories/{code}", h.updateCategory)
	r.Put("/permission-categories/{code}/quotas", h.setQuotas)
	r.Put("/leave-client-settings/{client_code}", h.setClientSetting)
	r.Get("/personnel/{personnel_id}/leave-ledger", h.listLedger)
	r.Post("/personnel/{personnel_id}/leave-ledger", h.adjustLedger)
}

// MountField adds the routes of the field app. The categories need a signed-in person. The quota
// and the approver are the caller's own and need attendance.leave.request, through a membership
// that is active today.
func (h *Handler) MountField(r chi.Router) {
	r.Get("/api/v1/permission-categories", h.fieldCategories)
	request := h.access.Require(access.AttendanceLeaveRequest)
	r.With(request).Get("/api/v1/permissions/quota", h.quota)
	r.With(request).Get("/api/v1/permissions/approver", h.approver)
}

// ---------- JSON ----------

type categoryJSON struct {
	Code              string    `json:"code"`
	Name              string    `json:"name"`
	Type              string    `json:"type"`
	IsAnnual          bool      `json:"is_annual"`
	SkipsDayCheck     bool      `json:"skips_day_check"`
	RequiresTimeRange bool      `json:"requires_time_range"`
	RequiresChangeDay bool      `json:"requires_change_day"`
	MakesLeaveDays    bool      `json:"makes_leave_days"`
	ExcusesLateness   bool      `json:"excuses_lateness"`
	HasQuota          bool      `json:"has_quota"`
	SortOrder         int       `json:"sort_order"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func categoryToAPI(c Category) categoryJSON {
	return categoryJSON{
		Code: c.Code, Name: c.Name, Type: string(c.Type), IsAnnual: c.IsAnnual, SkipsDayCheck: c.SkipsDayCheck,
		RequiresTimeRange: c.RequiresTimeRange, RequiresChangeDay: c.RequiresChangeDay, MakesLeaveDays: c.MakesLeaveDays,
		ExcusesLateness: c.ExcusesLateness, HasQuota: c.HasQuota, SortOrder: c.SortOrder, IsActive: c.Active,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
}

type categoryDetailJSON struct {
	categoryJSON
	Quotas Quotas `json:"quotas"`
}

type tombstoneJSON struct {
	Code      string    `json:"code"`
	DeletedAt time.Time `json:"deleted_at"`
}

type quotaJSON struct {
	CategoryCode  string `json:"category_code"`
	QuotaDays     int    `json:"quota_days"`
	UsedDays      int    `json:"used_days"`
	RemainingDays int    `json:"remaining_days"`
}

type approverJSON struct {
	PersonnelID int64  `json:"personnel_id"`
	Name        string `json:"name"`
}

type ledgerEntryJSON struct {
	ID             int64     `json:"id"`
	Kind           string    `json:"kind"`
	Days           int       `json:"days"`
	BalanceAfter   int       `json:"balance_after"`
	LeaveRequestID *int64    `json:"leave_request_id"`
	Note           string    `json:"note"`
	CreatedByID    *int64    `json:"created_by_id"`
	CreatedAt      time.Time `json:"created_at"`
}

func ledgerEntryToAPI(e LedgerEntry) ledgerEntryJSON {
	return ledgerEntryJSON{
		ID: e.ID, Kind: string(e.Kind), Days: e.Days, BalanceAfter: e.BalanceAfter, LeaveRequestID: e.LeaveRequestID,
		Note: e.Note, CreatedByID: e.CreatedByID, CreatedAt: e.CreatedAt,
	}
}

// ---------- admin ----------

// caller is the ORG_ADMIN behind an admin request: a REGION_ADMIN is told so before the body is
// read, whatever is in it. The service checks it again.
func caller(w http.ResponseWriter, r *http.Request) (auth.Principal, access.AdminScope, bool) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return p, scope, false
	}
	if !scope.All {
		httpx.Error(w, r, access.ErrOrgAdminOnly)
		return auth.Principal{}, access.AdminScope{}, false
	}
	return p, scope, true
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

func typeParam(w http.ResponseWriter, r *http.Request) (Type, bool) {
	switch t := Type(strings.ToUpper(r.URL.Query().Get("type"))); t {
	case "", TypeLeave, TypePermit, TypeSick:
		return t, true
	}
	httpx.Error(w, r, validationError("type", "Harus LEAVE, PERMIT atau SICK."))
	return "", false
}

func (h *Handler) listCategories(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	t, ok := typeParam(w, r)
	if !ok {
		return
	}
	f := CategoryFilter{Type: t, Q: query.Q, Limit: query.Limit, Offset: query.Offset()}
	if s := r.URL.Query().Get("is_active"); s != "" {
		active, err := strconv.ParseBool(s)
		if err != nil {
			httpx.Error(w, r, validationError("is_active", "Harus true atau false."))
			return
		}
		f.Active = &active
	}
	rows, total, err := h.service.ListCategories(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]categoryJSON, 0, len(rows))
	for _, c := range rows {
		out = append(out, categoryToAPI(c))
	}
	httpx.List(w, r, out, query.WithTotal(total))
}

func (h *Handler) createCategory(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in CategoryInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := h.service.CreateCategory(r.Context(), p, scope, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, categoryToAPI(c))
}

func (h *Handler) getCategory(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	c, err := h.service.GetCategory(r.Context(), p, scope, chi.URLParam(r, "code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, categoryDetailJSON{categoryJSON: categoryToAPI(c.Category), Quotas: c.Quotas})
}

func (h *Handler) updateCategory(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in CategoryUpdate
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := h.service.UpdateCategory(r.Context(), p, scope, chi.URLParam(r, "code"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, categoryToAPI(c))
}

func (h *Handler) setQuotas(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in QuotasInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.service.SetQuotas(r.Context(), p, scope, chi.URLParam(r, "code"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, out)
}

type clientSettingInput struct {
	RequestsEnabled *bool `json:"requests_enabled" validate:"required"`
}

func (h *Handler) setClientSetting(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var in clientSettingInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	code := chi.URLParam(r, "client_code")
	if err := h.service.SetClientEnabled(r.Context(), p, scope, code, *in.RequestsEnabled); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, map[string]any{"client_code": code, "requests_enabled": *in.RequestsEnabled})
}

func personnelParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "personnel_id"), 10, 64)
	return id, err == nil && id > 0
}

func (h *Handler) listLedger(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	id, ok := personnelParam(r)
	if !ok {
		httpx.Error(w, r, apperr.ErrNotFound)
		return
	}
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page, err := h.service.ListLedger(r.Context(), p, scope, id, query.Limit, query.Offset())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]ledgerEntryJSON, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, ledgerEntryToAPI(e))
	}
	httpx.List(w, r, out, query.WithTotal(page.Total))
}

func (h *Handler) adjustLedger(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	id, ok := personnelParam(r)
	if !ok {
		httpx.Error(w, r, apperr.ErrNotFound)
		return
	}
	var in LedgerInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	e, err := h.service.AdjustLedger(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, ledgerEntryToAPI(e))
}

// ---------- field ----------

// sinceParam reads the delta-sync watermark. delta is false when none was sent (a full pull).
func sinceParam(w http.ResponseWriter, r *http.Request) (since time.Time, delta, ok bool) {
	s := r.URL.Query().Get("since")
	if s == "" {
		return time.Time{}, false, true
	}
	// A "+07:00" offset sent without URL encoding arrives as " 07:00": put the plus back.
	t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "+", 1))
	if err != nil {
		httpx.Error(w, r, validationError("since", "Harus berformat tanggal-waktu RFC 3339, mis. 2026-06-01T03:00:00Z."))
		return time.Time{}, false, false
	}
	return t, true, true
}

func fullPage(n int) httpx.Page { return httpx.Page{Page: 1, Limit: max(n, 1), Total: n} }

func (h *Handler) fieldCategories(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	t, ok := typeParam(w, r)
	if !ok {
		return
	}
	since, delta, ok := sinceParam(w, r)
	if !ok {
		return
	}
	if delta {
		ch, err := h.service.FieldCategoryChanges(r.Context(), p, t, since)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		up := make([]categoryJSON, 0, len(ch.Upserted))
		for _, c := range ch.Upserted {
			up = append(up, categoryToAPI(c))
		}
		gone := make([]tombstoneJSON, 0, len(ch.Deleted))
		for _, d := range ch.Deleted {
			gone = append(gone, tombstoneJSON(d))
		}
		httpx.Delta(w, r, up, gone, httpx.DeltaMeta{ServerTime: ch.ServerTime})
		return
	}
	rows, serverTime, err := h.service.FieldCategories(r.Context(), p, t)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]categoryJSON, 0, len(rows))
	for _, c := range rows {
		out = append(out, categoryToAPI(c))
	}
	httpx.ListAt(w, r, out, fullPage(len(out)), serverTime)
}

func (h *Handler) quota(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	code := r.URL.Query().Get("category_code")
	if code == "" {
		httpx.Error(w, r, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "category_code", Code: "REQUIRED", Message: "category_code wajib diisi."}))
		return
	}
	q, err := h.service.QuotaOf(r.Context(), p, code, r.URL.Query().Get("team_code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, quotaJSON(q))
}

func (h *Handler) approver(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	a, err := h.service.ApproverOf(r.Context(), p, r.URL.Query().Get("team_code"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, approverJSON(a))
}
