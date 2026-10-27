package media

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/httpx"
)

// Handler is the HTTP side of the media module: decode, call the usecase, respond.
type Handler struct {
	service    *Service
	storage    Storage
	idempotent func(http.Handler) http.Handler
}

// NewHandler returns the handler. idempotent is the Idempotency-Key middleware for routes that
// must be safe to retry (idem.Service.Middleware(true)); storage is the same driver the
// service uses, and decides whether the blob routes answer.
func NewHandler(service *Service, storage Storage, idempotent func(http.Handler) http.Handler) *Handler {
	return &Handler{service: service, storage: storage, idempotent: idempotent}
}

// Mount adds the media routes. The two blob routes are public (auth.PublicRoutes): the
// signature in their URL is the authorization, and a phone uploading straight to a URL it was
// given has no bearer token to send there.
func (h *Handler) Mount(r chi.Router) {
	// Idempotency-Key is required: the offline media queue retries this request, and a replay
	// must give back the same id and upload URL instead of registering a second photo.
	r.With(h.idempotent).Post("/api/v1/media", h.register)
	r.Post("/api/v1/media/{id}/complete", h.complete)
	MountBlobs(r, h.storage)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	var in RegisterInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	reg, err := h.service.Register(r.Context(), p, in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusCreated, registrationToAPI(reg))
}

func (h *Handler) complete(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok {
		httpx.Error(w, r, apperr.ErrUnauthenticated)
		return
	}
	// An id that cannot exist does not exist.
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Error(w, r, ErrNotFound)
		return
	}
	m, err := h.service.Complete(r.Context(), p, id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, mediaToAPI(m))
}
