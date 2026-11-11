package main

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"siapp/pkg/access"
	"siapp/pkg/attendance"
	"siapp/pkg/identity"
	"siapp/pkg/leave"
	"siapp/pkg/location"
	"siapp/pkg/masterdata"
	"siapp/pkg/media"
	"siapp/pkg/notifications"
	"siapp/pkg/organization"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/personel"
	"siapp/pkg/presence"
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
	// AttendanceQRSecret is ATTENDANCE_QR_SECRET. Empty leaves the QR method off.
	AttendanceQRSecret string
}

// adminPrefix is the group every web admin route lives under. access.RequireAdmin guards the
// whole group, once.
const adminPrefix = "/api/v1/admin"

// newRouter wires the modules onto the router. Every module a later phase adds is mounted
// here, so the guard tests cover it without being touched: a public route needs a line in
// auth.PublicRoutes, an admin route goes in the admin group below.
func newRouter(d deps) *chi.Mux {
	revocations := auth.NewRevocations(d.Redis, d.RedisPrefix, d.Logger)
	// Presence (08.1): every authenticated request counts as the caller being seen, so the
	// middleware sits right behind the token check, in the same slot.
	presenceService := presence.New(d.Redis, d.RedisPrefix, d.Pool, d.Clock, d.Logger)
	requireAuth, touch := auth.Require(d.Tokens, revocations), presence.Middleware(presenceService, d.Logger)
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       d.Logger,
		CORSOrigins:  d.CORSOrigins,
		DB:           d.Pool,
		Authenticate: func(next http.Handler) http.Handler { return requireAuth(touch(next)) },
	})

	failures := httpx.NewFailureLimiter(d.Redis, d.RedisPrefix, d.Logger)
	idempotency := idem.New(idem.Config{Pool: d.Pool, Identity: auth.Identity})

	identityUsecase := identity.NewUsecase(d.Pool, d.Personel, d.Tokens, revocations, d.Clock, d.Logger)
	// Who serves in which regu is module 02's: a REGION_ADMIN's reach over a person and the
	// memberships /auth/me lists come from it.
	directory := organization.NewDirectory(d.Clock)
	identityUsecase.UseRegionLookup(directory)
	identityUsecase.UseMembershipLookup(directory)
	identityUsecase.UsePresence(presenceService)
	identityHandler := identity.NewHandler(identityUsecase, d.ClientIP, failures, idempotency.Middleware(false))
	identityHandler.Mount(router)

	accessHandler := access.NewHandler(access.NewWebRoles(d.Pool, d.Clock), access.NewFieldPermissions(d.Pool, d.Clock))
	organizationUsecases := organization.NewUsecases(d.Pool, d.Clock, identity.NewPeople(d.Pool, d.Personel, d.Clock))
	organizationUsecases.Roster.UsePresence(presenceService)
	organizationHandler := organization.NewHandler(organizationUsecases)
	// The Regu screens: any signed-in person, scoped by the regu they serve in.
	organizationHandler.MountField(router)

	// Photo uploads (module 04). Registering and completing need a signed-in person; the two
	// blob routes of the local driver are public and authorized by the token in their URL.
	mediaService := media.NewService(d.Pool, d.MediaStorage, d.Clock, d.Logger)
	mediaHandler := media.NewHandler(mediaService, d.MediaStorage, idempotency.Middleware(true))
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

	// Master data (module 06): categories, panic types, stakeholders, letterheads, app links and
	// the leave categories Personel owns. Modules that point at a master register a usage check on
	// this service when they land, so a referenced master is refused a delete (MASTER_IN_USE).
	masterdataService := masterdata.NewService(d.Pool, d.Clock)
	// A letterhead's logo is a photo that goes through the media module.
	masterdataService.UseLogoStore(mediaService)
	masterdataHandler := masterdata.NewHandler(masterdataService)
	masterdataHandler.MountField(router)

	// Attendance capture (module 07): today's shifts and the selfie check-in/out. The shifts come
	// from module 02, the photos from module 04; every change also lands in the Personel outbox,
	// which the worker delivers in Phase 07.3.
	accessService := access.NewService(d.Pool, d.Clock)
	attendanceService := attendance.NewService(d.Pool, d.Clock, organization.NewShifts(d.Pool), organization.NewQuery(d.Pool), mediaService, accessService)
	attendanceService.UseNotifier(notifier)
	if d.AttendanceQRSecret != "" {
		qr, err := attendance.NewQRTokens(d.AttendanceQRSecret, d.Clock)
		if err != nil {
			panic(err) // config.Load refuses a short secret, so this is a test or a caller that skipped it
		}
		attendanceService.UseQR(qr)
	}
	attendanceHandler := attendance.NewHandler(attendanceService, accessService, idempotency.Middleware(true))
	attendanceHandler.MountField(router)

	// Leave, permit and sick requests (07.5): SiAPP's own, nothing of it goes through Personalia.
	// Part A is the categories, quotas and the ledger of annual leave; Part B the requests.
	leaveService := leave.NewService(d.Pool, d.Clock)
	leaveService.UseRegionLookup(directory)
	leaveService.UseSchedule(organization.LeaveSchedule{Shifts: organization.NewShifts(d.Pool)})
	leaveService.UseAttachments(mediaService)
	leaveService.UsePermissions(accessService)
	leaveService.UseNotifier(notifier)
	leaveHandler := leave.NewHandler(leaveService, accessService, idempotency.Middleware(true))
	leaveHandler.MountField(router)

	// GPS tracking (08.2): devices send points while on shift, the DANRU and the web monitor read
	// the maps. "On shift" is the attendance module's notion, so a swapped-in guard is tracked too.
	locationService := location.NewService(d.Pool, d.Redis, d.RedisPrefix, d.Clock,
		location.AttendanceShifts{Service: attendanceService}, presenceService, publisher, d.Logger)
	locationHandler := location.NewHandler(locationService, httpx.NewRateLimiter(d.Redis, d.RedisPrefix, d.Logger))
	locationHandler.MountField(router, accessService)

	// The WebSocket (05.3). The hub is shared with the Redis subscriber main starts, which feeds
	// it the events other instances publish.
	hub := d.Hub
	if hub == nil {
		hub = realtime.NewHub(d.Logger)
	}
	realtimeHandler := realtime.NewHandler(d.Tokens, revocations, realtime.NewTickets(d.Redis, d.RedisPrefix), hub,
		realtime.NewAuthorizer(d.Pool, accessService, d.Clock), d.Clock, d.Logger,
		realtime.Config{OriginPatterns: realtime.OriginPatternsOf(d.CORSOrigins)})
	realtimeHandler.UsePresence(presenceService)
	realtimeHandler.UseLocation(locationService) // location.update frames (08.2)
	realtimeHandler.Mount(router)

	// ⚠ The admin group: access.RequireAdmin is mounted ONCE here, so every route a module
	// adds under /api/v1/admin is protected without anyone having to remember it.
	router.Route(adminPrefix, func(admin chi.Router) {
		admin.Use(access.RequireAdmin(d.Pool))
		identityHandler.MountAdmin(admin)
		accessHandler.MountAdmin(admin)
		organizationHandler.MountAdmin(admin)
		masterdataHandler.MountAdmin(admin)
		leaveHandler.MountAdmin(admin)
		attendanceHandler.MountAdmin(admin)
		locationHandler.MountAdmin(admin)
	})

	return router
}
