package leave

import (
	"context"
	"net/http"

	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// managedFilter reads the filters of a list of requests to decide. ok is false when the answer is
// already written.
func managedFilter(w http.ResponseWriter, r *http.Request) (ManagedFilter, httpx.ListQuery, bool) {
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return ManagedFilter{}, query, false
	}
	f := ManagedFilter{
		TeamCode: r.URL.Query().Get("team_code"), ProjectCode: r.URL.Query().Get("project_code"),
		RegionCode: r.URL.Query().Get("region_code"), Q: query.Q, Limit: query.Limit, Offset: query.Offset(),
	}
	for _, v := range listValues(r, "type") {
		switch t := Type(v); t {
		case TypeLeave, TypePermit, TypeSick:
			f.Types = append(f.Types, t)
		default:
			httpx.Error(w, r, validationError("type", "Harus LEAVE, PERMIT atau SICK."))
			return f, query, false
		}
	}
	for _, v := range listValues(r, "status") {
		switch st := Status(v); st {
		case StatusPending, StatusApproved, StatusRejected, StatusCancelled:
			f.Statuses = append(f.Statuses, st)
		default:
			httpx.Error(w, r, validationError("status", "Harus PENDING, APPROVED, REJECTED atau CANCELLED."))
			return f, query, false
		}
	}
	var ok bool
	if f.From, ok = dateQuery(w, r, "from_date"); !ok {
		return f, query, false
	}
	if f.To, ok = dateQuery(w, r, "to_date"); !ok {
		return f, query, false
	}
	return f, query, true
}

func writePage(w http.ResponseWriter, r *http.Request, page Page, query httpx.ListQuery) {
	out := make([]requestJSON, 0, len(page.Requests))
	for _, req := range page.Requests {
		out = append(out, requestToAPI(req))
	}
	httpx.List(w, r, out, query.WithTotal(page.Total))
}

// decideBody reads the optional body of an approval or a rejection (an approval may come with none).
func decideBody(w http.ResponseWriter, r *http.Request) (DecideInput, bool) {
	var in DecideInput
	if r.ContentLength == 0 {
		return in, true
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return in, false
	}
	return in, true
}

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	f, query, ok := managedFilter(w, r)
	if !ok {
		return
	}
	f.ProjectCode, f.RegionCode = "", "" // the DANRU's reach is their regu, whatever they ask
	page, err := h.service.ListForDanru(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	writePage(w, r, page, query)
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	h.decideAsDanru(w, r, h.service.ApproveByDanru)
}

func (h *Handler) decline(w http.ResponseWriter, r *http.Request) {
	h.decideAsDanru(w, r, h.service.DeclineByDanru)
}

func (h *Handler) decideAsDanru(w http.ResponseWriter, r *http.Request, do func(context.Context, auth.Principal, int64, DecideInput) (Request, error)) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	in, ok := decideBody(w, r)
	if !ok {
		return
	}
	out, err := do(r.Context(), p, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, requestToAPI(out))
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	f, query, ok := managedFilter(w, r)
	if !ok {
		return
	}
	page, err := h.service.ListForAdmin(r.Context(), p, scope, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	writePage(w, r, page, query)
}

func (h *Handler) adminDetail(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	out, err := h.service.GetForAdmin(r.Context(), p, scope, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, requestToAPI(out))
}

func (h *Handler) adminApprove(w http.ResponseWriter, r *http.Request) {
	h.decideAsAdmin(w, r, h.service.ApproveByAdmin)
}

func (h *Handler) adminDecline(w http.ResponseWriter, r *http.Request) {
	h.decideAsAdmin(w, r, h.service.DeclineByAdmin)
}

func (h *Handler) decideAsAdmin(w http.ResponseWriter, r *http.Request, do func(context.Context, auth.Principal, access.AdminScope, int64, DecideInput) (Request, error)) {
	p, scope, ok := anyAdmin(w, r)
	if !ok {
		return
	}
	id, ok := idParam(r)
	if !ok {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	in, ok := decideBody(w, r)
	if !ok {
		return
	}
	out, err := do(r.Context(), p, scope, id, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, requestToAPI(out))
}
