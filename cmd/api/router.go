package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/pkg/identity"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/personel"
)

// deps is everything the routes are built from. main fills it from the configuration; the
// guard test in this package fills it from the test kit, so the test walks the same router
// that production serves.
type deps struct {
	Logger      *slog.Logger
	CORSOrigins []string
	Pool        *pgxpool.Pool
	Redis       redis.UniversalClient
	// RedisPrefix goes in front of every Redis key; tests use a random one.
	RedisPrefix string
	Personel    *personel.Client
	Tokens      *auth.Tokens
	Clock       clock.Clock
	ClientIP    func(*http.Request) string
}

// newRouter wires the modules onto the router. Every module a later phase adds is mounted
// here, so TestEveryRouteRequiresAuthentication covers it without being touched.
func newRouter(d deps) *chi.Mux {
	revocations := auth.NewRevocations(d.Redis, d.RedisPrefix, d.Logger)
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       d.Logger,
		CORSOrigins:  d.CORSOrigins,
		DB:           d.Pool,
		Authenticate: auth.Require(d.Tokens, revocations),
	})

	failures := httpx.NewFailureLimiter(d.Redis, d.RedisPrefix, d.Logger)
	identityUsecase := identity.NewUsecase(d.Pool, d.Personel, d.Tokens, revocations, d.Clock, d.Logger)
	identity.NewHandler(identityUsecase, d.ClientIP, failures).Mount(router)

	return router
}
