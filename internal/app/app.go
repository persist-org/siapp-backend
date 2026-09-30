// Package app wires the modules into the HTTP router. cmd/api and the tests both build the
// API through it, so the tests exercise the same wiring that runs in production.
package app

import (
	"database/sql"
	"log/slog"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/httpx"
)

// Deps are the dependencies the API is built from.
type Deps struct {
	Logger *slog.Logger
	DB     *sql.DB
	Redis  redis.UniversalClient
	// Clock defaults to the system clock.
	Clock          clock.Clock
	JWTSecret      []byte
	CORSOrigins    []string
	ClientIPHeader string
	// RateLimitPrefix is put in front of rate limiter Redis keys.
	RateLimitPrefix string
	// Passwords defaults to argon2id with DefaultPasswordParams. Tests pass a cheaper one.
	Passwords *auth.Passwords
	// ExtraRoutes mounts additional routes. They sit behind the same authentication as the
	// real ones; tests use it to add a route that is not part of the API.
	ExtraRoutes func(chi.Router)
}

// NewRouter builds the API router: shared middleware, default-deny authentication, and the
// routes of every module.
func NewRouter(d Deps) *chi.Mux {
	clk := d.Clock
	if clk == nil {
		clk = clock.System{}
	}
	passwords := d.Passwords
	if passwords == nil {
		passwords = auth.NewPasswords(auth.DefaultPasswordParams)
	}
	tokens := auth.NewTokens(d.JWTSecret, clk)

	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       d.Logger,
		CORSOrigins:  d.CORSOrigins,
		DB:           d.DB,
		Authenticate: auth.Require(tokens),
	})

	clientIP := httpx.ClientIPFunc(d.ClientIPHeader)
	failures := httpx.NewFailureLimiter(d.Redis, d.RateLimitPrefix, d.Logger)

	authService := auth.NewService(d.DB, passwords, tokens, clk)
	auth.NewHandler(authService, clientIP, failures).Routes(router)

	if d.ExtraRoutes != nil {
		d.ExtraRoutes(router)
	}
	return router
}
