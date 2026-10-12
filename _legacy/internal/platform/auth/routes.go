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
		r.Get("/help", h.listHelp)
	})

	// The signed-in user's own account. Protected by Require like everything not on the
	// public list.
	r.Get("/api/v1/me", h.getMe)
	r.Patch("/api/v1/me", h.updateMe)
	r.Post("/api/v1/me/password", h.changePassword)
	r.Put("/api/v1/me/device", h.putDevice)

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

	admin.Post("/api/v1/admin/login-help", h.adminCreateHelp)
	admin.Get("/api/v1/admin/login-help", h.adminListHelp)
	admin.Get("/api/v1/admin/login-help/{id}", h.adminGetHelp)
	admin.Patch("/api/v1/admin/login-help/{id}", h.adminUpdateHelp)
	admin.Delete("/api/v1/admin/login-help/{id}", h.adminDeleteHelp)
}
