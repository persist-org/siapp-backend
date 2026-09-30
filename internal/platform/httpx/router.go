// Package httpx holds the HTTP plumbing shared by every module: router, middleware and server.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// RouterConfig holds what the router needs to build its middleware stack.
type RouterConfig struct {
	Logger      *slog.Logger
	CORSOrigins []string
}

// NewRouter returns the root router with the shared middleware stack and platform routes.
// Modules mount their routes under /api/v1 on the returned router.
func NewRouter(cfg RouterConfig) *chi.Mux {
	r := chi.NewRouter()
	// Order matters: the request ID must exist before anything logs, and the recoverer sits
	// inside the access log so a recovered panic is still logged with its 500 status.
	r.Use(requestID, accessLog(cfg.Logger), recoverer(cfg.Logger), cors(cfg.CORSOrigins))

	r.Get("/api/v1/health", health)
	return r
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
