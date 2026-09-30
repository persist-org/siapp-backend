package app_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/api"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// TestEveryRouteRequiresAuthUnlessPublic is the guard that keeps future phases honest: it
// walks every route the router really has, calls it without a token, and demands 401 for
// everything that is not on auth.PublicRoutes. A phase that adds an endpoint and forgets
// about authentication cannot pass this test, because the new route is found automatically.
func TestEveryRouteRequiresAuthUnlessPublic(t *testing.T) {
	ok := func(http.ResponseWriter, *http.Request) {}
	router := testkit.NewRouter(t, testkit.Deps{
		// Routes that are not on the allowlist, standing in for endpoints added later.
		ExtraRoutes: func(r chi.Router) {
			r.Get("/api/v1/test/unlisted", ok)
			r.Post("/api/v1/test/{id}/action", ok)
			r.Delete("/api/v1/admin/test/{id}", ok)
		},
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	public := make(map[auth.Route]bool)
	for _, r := range auth.PublicRoutes {
		public[r] = true
	}

	param := regexp.MustCompile(`\{[^}]*\}`)
	seenPublic := make(map[auth.Route]bool)
	protected := 0
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		r := auth.Route{Method: method, Path: route}
		if public[r] {
			seenPublic[r] = true
			return nil
		}
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+param.ReplaceAllString(route, "x"), nil)
		require.NoError(t, err)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()

		require.Equal(t, http.StatusUnauthorized, res.StatusCode, "%s %s is reachable without a token but is not in auth.PublicRoutes", method, route)
		protected++
		return nil
	})
	require.NoError(t, err)

	require.Equal(t, 3, protected, "the three extra routes must have been walked and found protected")
	for _, r := range auth.PublicRoutes {
		require.True(t, seenPublic[r], "auth.PublicRoutes lists %s %s but the router has no such route", r.Method, r.Path)
	}
}

// TestPublicRoutesMatchTheContract checks that the allowlist and api/openapi.yaml agree: an
// operation is public in one exactly when it is public in the other. Otherwise the contract
// would promise a client a route needs no token, or the reverse.
func TestPublicRoutesMatchTheContract(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(api.OpenAPI)
	require.NoError(t, err)
	require.NotEmpty(t, doc.Security, "the contract must require a bearer token by default")

	fromSpec := make(map[auth.Route]bool)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				fromSpec[auth.Route{Method: method, Path: path}] = true
			}
		}
	}

	fromCode := make(map[auth.Route]bool)
	for _, r := range auth.PublicRoutes {
		fromCode[r] = true
	}
	require.Equal(t, fromCode, fromSpec, "auth.PublicRoutes and the `security: []` operations of api/openapi.yaml differ")
}
