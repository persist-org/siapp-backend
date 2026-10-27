package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// platformRoutes are public routes that are not part of the PERSIST contract, so they carry
// no `security: []` there: the health probe is infrastructure, not an API operation.
var platformRoutes = map[string]bool{"GET /api/v1/health": true}

// productionRouter builds the router exactly as main does, from the test kit's services.
func productionRouter(t *testing.T) *chi.Mux {
	t.Helper()
	testkit.Reset(t)
	_, personelClient := testkit.NewPersonel(t)
	// The local driver is the default one: it needs no service, and its blob routes are the
	// public ones the guard tests below must see mounted.
	storage, err := media.NewLocal(config.Storage{
		LocalDir:        t.TempDir(),
		LocalSigningKey: "guard-test-signing-key-guard-test-signing-key",
		PublicBaseURL:   "http://localhost:8080",
	}, clock.System{})
	require.NoError(t, err)
	return newRouter(deps{
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Pool:        testkit.Pool(t),
		Redis:       testkit.Redis(t),
		RedisPrefix: testkit.RandomPrefix(t),
		Personel:    personelClient,
		Tokens:      testkit.Tokens(t),
		Clock:       clock.System{},
		ClientIP:    httpx.ClientIPFunc(""),

		MediaStorage: storage,
	})
}

type mounted struct{ method, route string }

func (m mounted) key() string { return m.method + " " + m.route }

func walk(t *testing.T, router chi.Routes) []mounted {
	t.Helper()
	var out []mounted
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, mounted{method, route})
		return nil
	})
	require.NoError(t, err)
	return out
}

var pathParam = regexp.MustCompile(`\{[^}]*\}`)

func status(router http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// ⚠ Guard test (keep it, and keep every module mounted in newRouter): authentication is
// default-deny. Every route the API mounts answers 401 without a token unless it is on the
// explicit public allowlist, so a route someone forgets to think about fails closed.
func TestEveryRouteRequiresAuthentication(t *testing.T) {
	router := productionRouter(t)
	routes := walk(t, router)
	require.GreaterOrEqual(t, len(routes), 2, "the walk found the health probe and login at least")

	for _, r := range routes {
		path := pathParam.ReplaceAllString(r.route, "1")
		if auth.IsPublic(r.method, path) {
			require.NotEqual(t, http.StatusUnauthorized, status(router, r.method, path),
				"public route %s must not demand a token", r.key())
			continue
		}
		require.Equal(t, http.StatusUnauthorized, status(router, r.method, path),
			"%s is mounted but not on auth.PublicRoutes, so it must answer 401 without a token", r.key())
	}

	t.Run("an unknown path is 401, not 404", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, status(router, http.MethodGet, "/api/v1/no/such/route"))
		require.Equal(t, http.StatusUnauthorized, status(router, http.MethodGet, "/"))
	})
	t.Run("a public path with another method is protected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, status(router, http.MethodDelete, "/api/v1/health"))
		require.Equal(t, http.StatusUnauthorized, status(router, http.MethodGet, "/api/v1/auth/login"))
	})
}

// ⚠ Guard test: auth.PublicRoutes and the contract's `security: []` operations agree.
// Adding a public route needs a line in both places.
func TestPublicRoutesMatchTheContract(t *testing.T) {
	router := productionRouter(t)
	mountedKeys := map[string]bool{}
	for _, r := range walk(t, router) {
		mountedKeys[r.key()] = true
	}
	contractPublic := testkit.ContractPublicOperations(t)

	for _, p := range auth.PublicRoutes {
		key := p.Method + " " + p.Path
		require.True(t, mountedKeys[key], "auth.PublicRoutes lists %s, which no module mounts", key)
		if platformRoutes[key] {
			require.False(t, contractPublic[key], "%s is a platform route and must not be in the contract", key)
			continue
		}
		require.True(t, contractPublic[key], "%s is public, so the contract needs `security: []` on it", key)
	}
	for key := range contractPublic {
		if !mountedKeys[key] {
			// The contract is ahead of the build: this operation arrives in a later phase,
			// and until then it is not public either (the middleware denies unknown routes).
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		require.True(t, auth.IsPublic(method, path), "the contract marks %s `security: []` but auth.PublicRoutes lacks it", key)
	}
}

// Guard test: every route under /api/v1/admin refuses a person who is not an admin with 403
// NOT_ADMIN. access.RequireAdmin is mounted once on the group, so a route that a later module
// adds there is covered without being touched; this test fails if one ever ends up outside it.
func TestEveryAdminRouteRequiresAnAdmin(t *testing.T) {
	router := productionRouter(t)
	// A genuine token of a person with no web role.
	token := testkit.Token(t, 900101, 42)

	adminRoutes := 0
	for _, r := range walk(t, router) {
		if !strings.HasPrefix(r.route, adminPrefix+"/") {
			continue
		}
		adminRoutes++
		path := pathParam.ReplaceAllString(r.route, "1")

		require.Equal(t, http.StatusUnauthorized, status(router, r.method, path),
			"%s must refuse a request without a token", r.key())

		req := httptest.NewRequestWithContext(t.Context(), r.method, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s must refuse a person who is not an admin", r.key())
		require.Contains(t, rec.Body.String(), "NOT_ADMIN", r.key())
	}
	require.GreaterOrEqual(t, adminRoutes, 4, "the walk found the admin routes: web roles and the device reset")
}
