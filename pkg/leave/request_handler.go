package leave

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/timeutil"
)

type decisionJSON struct {
	Level  string  `json:"level"`
	ByName string  `json:"by_name"`
	At     string  `json:"at"`
	Note   *string `json:"note"`
}

type sickJSON struct {
	Hospitalization bool           `json:"hospitalization"`
	LetterStatus    bool           `json:"letter_status"`
	LetterDate      *timeutil.Date `json:"letter_date"`
	Disease         *string        `json:"disease"`
}

type attachmentJSON struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

type requestJSON struct {
	ID             int64           `json:"id"`
	Number         string          `json:"number"`
	TeamCode       string          `json:"team_code"`
	Type           string          `json:"type"`
	CategoryCode   string          `json:"category_code"`
	CategoryName   string          `json:"category_name"`
	Status         string          `json:"status"`
	StartDate      timeutil.Date   `json:"start_date"`
	EndDate        timeutil.Date   `json:"end_date"`
	StartTime      *string         `json:"start_time"`
	EndTime        *string         `json:"end_time"`
	WorkingDays    int             `json:"working_days"`
	Reason         string          `json:"reason"`
	Responsibility *string         `json:"responsibility"`
	ContactInfo    *string         `json:"contact_info"`
	ChangeDayFrom  *timeutil.Date  `json:"change_day_from"`
	ChangeDayTo    *timeutil.Date  `json:"change_day_to"`
	RequestedAt    string          `json:"requested_at"`
	Decision       *decisionJSON   `json:"decision"`
	CanCancel      bool            `json:"can_cancel"`
	Sick           *sickJSON       `json:"sick"`
	Attachment     *attachmentJSON `json:"attachment"`
}

func requestToAPI(r Request) requestJSON {
	out := requestJSON{
		ID: r.ID, Number: r.Number, TeamCode: r.TeamCode, Type: string(r.Type), CategoryCode: r.CategoryCode,
		CategoryName: r.CategoryName, Status: string(r.Status), StartDate: r.StartDate, EndDate: r.EndDate,
		StartTime: r.StartTime, EndTime: r.EndTime, WorkingDays: r.WorkingDays, Reason: r.Reason,
		Responsibility: r.Responsibility, ContactInfo: r.ContactInfo, ChangeDayFrom: r.ChangeDayFrom, ChangeDayTo: r.ChangeDayTo,
		RequestedAt: timeutil.FormatTimestamp(r.RequestedAt), CanCancel: r.CanCancel,
	}
	if d := r.Decision; d != nil {
		out.Decision = &decisionJSON{Level: d.Level, ByName: d.ByName, At: timeutil.FormatTimestamp(d.At)}
		if d.Note != "" {
			out.Decision.Note = &d.Note
		}
	}
	if s := r.Sick; s != nil {
		out.Sick = &sickJSON{Hospitalization: s.Hospitalization, LetterStatus: s.LetterStatus, LetterDate: s.LetterDate}
		if s.Disease != "" {
			out.Sick.Disease = &s.Disease
		}
	}
	out.Attachment = attachmentToAPI(r.Attachment)
	return out
}

func attachmentToAPI(v *media.View) *attachmentJSON {
	if v == nil {
		return nil
	}
	return &attachmentJSON{ID: v.ID, URL: v.URL, ContentType: v.ContentType}
}

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	var in SubmitInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	out, err := h.service.Submit(r.Context(), p, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, requestToAPI(out))
}

// listValues reads a filter that may be repeated ("type=LEAVE&type=SICK", "type[]=LEAVE") or
// comma-separated ("type=LEAVE,SICK"), upper-cased.
func listValues(r *http.Request, name string) []string {
	var out []string
	for _, key := range []string{name + "[]", name} {
		for _, v := range r.URL.Query()[key] {
			for part := range strings.SplitSeq(v, ",") {
				if part = strings.ToUpper(strings.TrimSpace(part)); part != "" {
					out = append(out, part)
				}
			}
		}
	}
	return out
}

func (h *Handler) mine(w http.ResponseWriter, r *http.Request) {
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
	f := Filter{Limit: query.Limit, Offset: query.Offset()}
	for _, v := range listValues(r, "type") {
		switch t := Type(v); t {
		case TypeLeave, TypePermit, TypeSick:
			f.Types = append(f.Types, t)
		default:
			httpx.Error(w, r, validationError("type", "Harus LEAVE, PERMIT atau SICK."))
			return
		}
	}
	for _, v := range listValues(r, "status") {
		switch st := Status(v); st {
		case StatusPending, StatusApproved, StatusRejected, StatusCancelled:
			f.Statuses = append(f.Statuses, st)
		default:
			httpx.Error(w, r, validationError("status", "Harus PENDING, APPROVED, REJECTED atau CANCELLED."))
			return
		}
	}
	if f.From, ok = dateQuery(w, r, "from_date"); !ok {
		return
	}
	if f.To, ok = dateQuery(w, r, "to_date"); !ok {
		return
	}
	page, err := h.service.ListMine(r.Context(), p, f)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]requestJSON, 0, len(page.Requests))
	for _, req := range page.Requests {
		out = append(out, requestToAPI(req))
	}
	httpx.List(w, r, out, query.WithTotal(page.Total))
}

// dateQuery reads an optional YYYY-MM-DD parameter. ok is false when the answer is already written.
func dateQuery(w http.ResponseWriter, r *http.Request, name string) (*timeutil.Date, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return nil, true
	}
	d, err := timeutil.ParseDate(s)
	if err != nil {
		httpx.Error(w, r, validationError(name, "Harus berformat YYYY-MM-DD."))
		return nil, false
	}
	return &d, true
}

// idParam is the request id of the path. A malformed one is the same 404 as a missing one.
func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.service.Get(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, requestToAPI(out))
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
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
	out, err := h.service.Cancel(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, requestToAPI(out))
}
