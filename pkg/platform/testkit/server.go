package testkit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"siapp/pkg/platform/httpx"
)

// Deps is what NewServer wires into the router. The zero value is a router with only the
// platform routes, backed by the test database.
type Deps struct {
	// Mount adds a module's routes to the router, the way cmd/api does.
	Mount func(r chi.Router)
	// Logger receives access logs; nil discards them.
	Logger *slog.Logger
}

// NewServer builds the real router (same middleware stack as cmd/api) on an httptest
// server, connected to the test database as siapp_app.
//
// Authentication is a pass-through until Phase 01.1 adds auth.Require; that phase switches
// this to the real middleware, so every later HTTP test goes through it.
func NewServer(t testing.TB, deps Deps) *httptest.Server {
	t.Helper()
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       logger,
		DB:           Pool(t),
		Authenticate: func(next http.Handler) http.Handler { return next },
	})
	if deps.Mount != nil {
		deps.Mount(router)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}
