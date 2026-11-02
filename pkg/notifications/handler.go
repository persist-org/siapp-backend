package notifications

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of the inbox: decode, call the use case, respond.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Mount adds the inbox routes. They need a signed-in person and nothing more: a notification is
// the caller's own, and the use cases only ever look at the caller's rows.
//
// None of them takes an Idempotency-Key: reading is safe, and marking read does the same thing
// every time it is repeated.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/api/v1/notifications", h.list)
	// The literal routes win over {id} in chi, so "count" and "read-all" are never read as ids.
	r.Get("/api/v1/notifications/count", h.count)
	r.Post("/api/v1/notifications/read-all", h.readAll)
	r.Get("/api/v1/notifications/{id}", h.get)
	r.Post("/api/v1/notifications/{id}/read", h.read)
}

// listRequest is the validated query of GET /notifications.
type listRequest struct {
	unreadOnly bool
	since      *time.Time
	cursor     *Cursor
	list       httpx.ListQuery
}

// isDelta reports whether the request asks for a delta pull. A cursor alone is enough: it is a
// position in a pull that `since` started.
func (q listRequest) isDelta() bool { return q.cursor != nil || q.since != nil }

// parseListRequest reads unread_only, since, cursor, page and limit. Every problem is reported
// at once, as one VALIDATION_ERROR.
func parseListRequest(r *http.Request) (listRequest, error) {
	var req listRequest
	var problems []apperr.FieldError
	fail := func(field, msg string) {
		problems = append(problems, apperr.FieldError{Field: field, Code: "INVALID", Message: msg})
	}

	list, err := httpx.ParseListQuery(r)
	if err != nil {
		appErr, ok := errors.AsType[*apperr.Error](err)
		if !ok {
			return listRequest{}, err
		}
		problems = append(problems, appErr.Fields...)
	}
	req.list = list

	values := r.URL.Query()
	switch values.Get("unread_only") {
	case "", "false":
	case "true":
		req.unreadOnly = true
	default:
		fail("unread_only", "Harus true atau false.")
	}
	if s := values.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			fail("since", "Harus berformat tanggal-waktu RFC 3339, mis. 2026-06-01T03:00:00Z.")
		}
		req.since = &t
	}
	if s := values.Get("cursor"); s != "" {
		c, err := ParseCursor(s)
		if err != nil {
			fail("cursor", "Cursor tidak valid. Pakai next_cursor dari jawaban sebelumnya.")
		}
		req.cursor = &c
	}
	// ⚠ The unread filter has no meaning in a delta pull. The pull exists to keep a cache in step,
	// and a notification that was just read would drop out of an unread-only stream without a
	// tombstone, leaving the cache showing it as unread for ever. Refused, not ignored, so the
	// app's developer finds out instead of shipping a counter that drifts.
	if req.unreadOnly && req.isDelta() {
		fail("unread_only", "Tidak boleh dikirim bersama since atau cursor.")
	}

	if len(problems) > 0 {
		return listRequest{}, apperr.ErrValidation.WithFields(problems...)
	}
	return req, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	req, err := parseListRequest(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if req.isDelta() {
		in := DeltaInput{Cursor: req.cursor, Limit: req.list.Limit}
		if req.since != nil {
			in.Since = *req.since
		}
		changes, err := h.service.Changes(r.Context(), p, in)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.Delta(w, r, notificationsToAPI(changes.Upserted), tombstonesToAPI(changes.Deleted), httpx.DeltaMeta{
			ServerTime: changes.ServerTime, NextCursor: changes.NextCursor, HasMore: changes.HasMore,
		})
		return
	}

	listing, err := h.service.List(r.Context(), p, ListInput{UnreadOnly: req.unreadOnly, Limit: req.list.Limit, Offset: req.list.Offset()})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.ListAt(w, r, notificationsToAPI(listing.Items), req.list.WithTotal(listing.Total), listing.ServerTime)
}

func (h *Handler) count(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	c, err := h.service.Count(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, countToAPI(c))
}

func (h *Handler) readAll(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	c, err := h.service.MarkAllRead(r.Context(), p)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, countToAPI(c))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
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
	n, err := h.service.Get(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, notificationToAPI(n))
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
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
	n, err := h.service.MarkRead(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, notificationToAPI(n))
}

// idParam reads {id}. An id that cannot exist does not exist: the answer is the 404 of a missing
// notification, not a validation error that would say what an id looks like.
func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id >= 1
}
