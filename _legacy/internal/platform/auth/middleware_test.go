package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/httpx"
	"siapp/internal/platform/testkit"
)

// whoami is a route that is not part of the API. It exists only so the tests can see what
// the middleware puts into the context.
func whoami(r chi.Router) {
	r.Get("/api/v1/test/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		if !ok {
			http.Error(w, "no principal", http.StatusInternalServerError)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{
			"userId": p.UserID, "role": string(p.SystemRole), "sessionId": p.SessionID,
		})
	})
}

func get(t *testing.T, srv *httptest.Server, path, authorization string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func codeOf(t *testing.T, res *http.Response) string {
	t.Helper()
	var body errBody
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	return body.Error.Code
}

func TestRequireDefaultDeny(t *testing.T) {
	pool := testkit.DB(t)
	testkit.Reset(t, pool)
	srv := testkit.NewServer(t, testkit.Deps{DB: pool, ExtraRoutes: whoami})
	client := testkit.NewClient(t, srv)
	user := testkit.CreateUser(t, pool, testkit.UserOpts{Role: db.UsersSystemRoleADMIN})

	tokenAt := func(clk clock.Clock, secret string) string {
		token, _, err := auth.NewTokens([]byte(secret), clk).NewAccessToken(user.ID, user.Role, "session-1")
		require.NoError(t, err)
		return token
	}
	now := clock.System{}

	t.Run("a valid token reaches the handler with its principal", func(t *testing.T) {
		var body loginBody
		login(client, user.Email, user.Password).JSON(t, &body)

		got := get(t, srv, "/api/v1/test/whoami", "Bearer "+body.AccessToken)
		require.Equal(t, http.StatusOK, got.StatusCode)
		var who map[string]string
		require.NoError(t, json.NewDecoder(got.Body).Decode(&who))
		require.Equal(t, user.ID, who["userId"])
		require.Equal(t, "ADMIN", who["role"])
		require.NotEmpty(t, who["sessionId"])
	})

	t.Run("the scheme name is case-insensitive", func(t *testing.T) {
		res := get(t, srv, "/api/v1/test/whoami", "bearer "+tokenAt(now, testkit.TestJWTSecret))
		require.Equal(t, http.StatusOK, res.StatusCode)
	})

	rejected := map[string]string{
		"no header":            "",
		"empty bearer":         "Bearer ",
		"scheme only":          "Bearer",
		"other scheme":         "Basic dXNlcjpwYXNz",
		"token without scheme": tokenAt(now, testkit.TestJWTSecret),
		"garbage":              "Bearer not-a-jwt",
		"wrong secret":         "Bearer " + tokenAt(now, "another-secret-another-secret-another-1"),
		"expired":              "Bearer " + tokenAt(clock.Fixed{T: time.Now().Add(-time.Hour)}, testkit.TestJWTSecret),
		"alg none":             "Bearer " + unsigned(t, user),
	}
	for name, header := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			res := get(t, srv, "/api/v1/test/whoami", header)
			require.Equal(t, http.StatusUnauthorized, res.StatusCode)
			require.Equal(t, "UNAUTHENTICATED", codeOf(t, res))
			require.Contains(t, res.Header.Get("WWW-Authenticate"), "Bearer")
		})
	}

	t.Run("health stays public", func(t *testing.T) {
		require.Equal(t, http.StatusOK, get(t, srv, "/api/v1/health", "").StatusCode)
	})

	t.Run("an unknown path is 401 without a token, so routes cannot be probed", func(t *testing.T) {
		res := get(t, srv, "/api/v1/does-not-exist", "")
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
		require.Equal(t, "UNAUTHENTICATED", codeOf(t, res))
	})

	t.Run("an unknown path is 404 with a valid token", func(t *testing.T) {
		res := get(t, srv, "/api/v1/does-not-exist", "Bearer "+tokenAt(now, testkit.TestJWTSecret))
		require.Equal(t, http.StatusNotFound, res.StatusCode)
		require.Equal(t, "NOT_FOUND", codeOf(t, res))
	})

	t.Run("public routes are matched exactly, not by prefix or method", func(t *testing.T) {
		for _, path := range []string{"/api/v1/health/", "/api/v1/health/extra", "/api/v1/healthz", "/api/v1/auth/login/", "/api/v1/auth/login/x"} {
			require.Equal(t, http.StatusUnauthorized, get(t, srv, path, "").StatusCode, path)
		}
		// GET on the POST-only public route is not public either.
		require.Equal(t, http.StatusUnauthorized, get(t, srv, "/api/v1/auth/login", "").StatusCode)
	})
}

// unsigned builds a token with alg "none" and otherwise valid claims.
func unsigned(t *testing.T, user testkit.User) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": user.ID, "role": string(user.Role), "sid": "session-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	return s
}
