package auth

import "github.com/go-chi/chi/v5"

// Routes mounts the auth endpoints. Every route that must work without a token is also in
// PublicRoutes.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Post("/login", h.login)
	})
}
