// Package httpx holds the HTTP plumbing shared by every module: router, middleware and server.
package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/apperr"
)

const healthPingTimeout = 2 * time.Second

// Pinger is the part of *pgxpool.Pool the health check needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// RouterConfig holds what the router needs to build its middleware stack.
type RouterConfig struct {
	Logger      *slog.Logger
	CORSOrigins []string
	// DB is pinged by the health check. Nil skips the check: the API has no database until
	// Phase 00.2 wires Postgres, and from then on cmd/api always sets it.
	DB Pinger
	// Authenticate is the authentication middleware (auth.Require). It is required and has
	// no default: a router that forgot it would serve every route to anonymous callers.
	// Tests that exercise other middleware pass a no-op explicitly.
	Authenticate func(http.Handler) http.Handler
}

// NewRouter returns the root router with the shared middleware stack and platform routes.
// Modules mount their routes on the returned router.
func NewRouter(cfg RouterConfig) *chi.Mux {
	if cfg.Authenticate == nil {
		panic("httpx: RouterConfig.Authenticate is required; routes must never be open by accident")
	}
	r := chi.NewRouter()
	// Order matters: the request ID must exist before anything logs, the recoverer sits
	// inside the access log so a recovered panic is still logged with its 500 status, and
	// authentication comes after CORS so that browser preflight requests, which carry no
	// token, are answered before the token check.
	r.Use(requestID, accessLog(cfg.Logger), recoverer(cfg.Logger), cors(cfg.CORSOrigins), cfg.Authenticate)

	// Unknown paths get the standard error shape instead of chi's plain-text 404.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) { Error(w, r, apperr.ErrNotFound) })

	r.Get("/api/v1/health", health(cfg.Logger, cfg.DB))
	return r
}

// health reports 503 when the database is unreachable, so load balancers and orchestrators
// stop routing traffic to an instance that cannot serve requests.
func health(logger *slog.Logger, db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			JSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			logger.WarnContext(r.Context(), "health: database ping failed",
				slog.String("error", err.Error()),
				slog.String("requestId", RequestID(r.Context())),
			)
			JSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
