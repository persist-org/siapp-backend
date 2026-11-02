package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/pkg/access"
	"siapp/pkg/identity"
	"siapp/pkg/media"
	"siapp/pkg/notifications"
	"siapp/pkg/organization"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/personel"
	"siapp/pkg/realtime"
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
	// MediaStorage is the driver STORAGE_DRIVER chose, already checked at start-up.
	MediaStorage media.Storage
	// PushQueue is where Notify queues its push tasks for the worker. Nil (the guard test) leaves
	// the inbox as the only delivery.
	PushQueue notifications.Enqueuer
	// Hub is the realtime hub; main starts the Redis subscriber that feeds it. Nil makes the
	// router a private one.
	Hub *realtime.Hub
	// PersonelWebhookSecret is PERSONEL_WEBHOOK_SECRET.
	PersonelWebhookSecret string
}

// adminPrefix is the group every web admin route lives under. access.RequireAdmin guards the
// whole group, once.
const adminPrefix = "/api/v1/admin"

// newRouter wires the modules onto the router. Every module a later phase adds is mounted
// here, so the guard tests cover it without being touched: a public route needs a line in
// auth.PublicRoutes, an admin route goes in the admin group below.
func newRouter(d deps) *chi.Mux {
	revocations := auth.NewRevocations(d.Redis, d.RedisPrefix, d.Logger)
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       d.Logger,
		CORSOrigins:  d.CORSOrigins,
		DB:           d.Pool,
		Authenticate: auth.Require(d.Tokens, revocations),
	})

	failures := httpx.NewFailureLimiter(d.Redis, d.RedisPrefix, d.Logger)
	idempotency := idem.New(idem.Config{Pool: d.Pool, Identity: auth.Identity})

	identityUsecase := identity.NewUsecase(d.Pool, d.Personel, d.Tokens, revocations, d.Clock, d.Logger)
	// Who serves in which regu is module 02's: a REGION_ADMIN's reach over a person and the
	// memberships /auth/me lists come from it.
	directory := organization.NewDirectory(d.Clock)
	identityUsecase.UseRegionLookup(directory)
	identityUsecase.UseMembershipLookup(directory)
	identityHandler := identity.NewHandler(identityUsecase, d.ClientIP, failures, idempotency.Middleware(false))
	identityHandler.Mount(router)

	accessHandler := access.NewHandler(access.NewWebRoles(d.Pool, d.Clock), access.NewFieldPermissions(d.Pool, d.Clock))
	organizationUsecases := organization.NewUsecases(d.Pool, d.Clock, identity.NewPeople(d.Pool, d.Personel, d.Clock))
	organizationHandler := organization.NewHandler(organizationUsecases)
	// The Regu screens: any signed-in person, scoped by the regu they serve in.
	organizationHandler.MountField(router)

	// Photo uploads (module 04). Registering and completing need a signed-in person; the two
	// blob routes of the local driver are public and authorized by the token in their URL.
	mediaHandler := media.NewHandler(media.NewService(d.Pool, d.MediaStorage, d.Clock, d.Logger), d.MediaStorage, idempotency.Middleware(true))
	mediaHandler.Mount(router)

	// The inbox (module 05): any signed-in person reads their own notifications. Modules that
	// notify get this service through a small interface of their own, wired here when they land;
	// Delivery after the row is written: realtime (05.3) to the recipient's open app, and push
	// (05.2) through the worker.
	notificationService := notifications.NewService(d.Pool, d.Clock, d.Logger)
	publisher := realtime.NewRedisPublisher(d.Redis, d.RedisPrefix, d.Clock)
	dispatchers := notifications.Dispatchers{realtime.NewNotificationDispatcher(publisher)}
	if d.PushQueue != nil {
		dispatchers = append(dispatchers, notifications.NewPushDispatcher(d.PushQueue))
	}
	notificationService.UseDispatcher(dispatchers)
	// The modules that notify depend on a small interface of their own and are given this
	// adapter, so none of them imports notifications.
	notifier := notifier{service: notificationService}
	organizationUsecases.Members.UseNotifier(notifier)
	notifications.NewHandler(notificationService).Mount(router)

	// The Personel webhook (05.3): public, authenticated by the HMAC signature of its body. It
	// invalidates the same Redis cache every reader of Personel data goes through.
	personelCache := personel.NewCached(d.Personel, nil, d.Redis, d.Logger)
	identity.NewPersonelEvents(identity.PersonelEventsConfig{
		Pool: d.Pool, Directory: personelCache, Cache: personelCache, Revocations: revocations,
		Redis: d.Redis, RedisPrefix: d.RedisPrefix, Secret: d.PersonelWebhookSecret,
		Notifier: notifier, Clock: d.Clock, Logger: d.Logger,
	}).Mount(router)

	// The WebSocket (05.3). The hub is shared with the Redis subscriber main starts, which feeds
	// it the events other instances publish.
	hub := d.Hub
	if hub == nil {
		hub = realtime.NewHub(d.Logger)
	}
	realtime.NewHandler(d.Tokens, revocations, realtime.NewTickets(d.Redis, d.RedisPrefix), hub,
		realtime.NewAuthorizer(d.Pool, access.NewService(d.Pool, d.Clock), d.Clock), d.Clock, d.Logger,
		realtime.Config{OriginPatterns: realtime.OriginPatternsOf(d.CORSOrigins)}).Mount(router)

	// ⚠ The admin group: access.RequireAdmin is mounted ONCE here, so every route a module
	// adds under /api/v1/admin is protected without anyone having to remember it.
	router.Route(adminPrefix, func(admin chi.Router) {
		admin.Use(access.RequireAdmin(d.Pool))
		identityHandler.MountAdmin(admin)
		accessHandler.MountAdmin(admin)
		organizationHandler.MountAdmin(admin)
	})

	return router
}
