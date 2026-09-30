package auth

import "github.com/go-chi/chi/v5"

// Routes mounts the auth and user administration endpoints. Every route that must work
// without a token is also in PublicRoutes.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/v1/auth", func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/refresh", h.refresh)
		r.Post("/logout", h.logout)
		r.Post("/logout-all", h.logoutAll)
		r.Post("/forgot-password", h.forgotPassword)
		r.Post("/reset-password", h.resetPassword)
	})

	// Admin routes are registered one by one behind RequireAdmin, not in a r.Route group:
	// chi allows one Route per prefix, and every later module with /admin endpoints needs to
	// add its own under the same /api/v1/admin.
	admin := r.With(RequireAdmin)
	admin.Post("/api/v1/admin/users", h.createUser)
	admin.Get("/api/v1/admin/users", h.listUsers)
	admin.Get("/api/v1/admin/users/{id}", h.getUser)
	admin.Patch("/api/v1/admin/users/{id}", h.updateUser)
	admin.Post("/api/v1/admin/users/{id}/disable", h.disableUser)
	admin.Post("/api/v1/admin/users/{id}/enable", h.enableUser)
}
