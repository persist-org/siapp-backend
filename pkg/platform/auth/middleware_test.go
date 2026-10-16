package auth_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

// newServer is the real router with the real middleware and two routes: a protected one that
// echoes the principal, and the public health probe the router mounts itself.
func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	tokens, err := auth.NewTokens([]byte(testSecret), clock.System{})
	require.NoError(t, err)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       logger,
		Authenticate: auth.Require(tokens, auth.NewRevocations(testkit.Redis(t), testkit.RandomPrefix(t), logger)),
	})
	router.Get("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		if !ok {
			t.Error("a protected route ran without a principal")
		}
		httpx.OK(w, r, http.StatusOK, map[string]int64{
			"personnel_id": p.PersonnelID, "organization_id": p.OrganizationID, "session_id": p.SessionID,
		})
	})
	router.Post("/api/v1/whoami/{id}", func(w http.ResponseWriter, r *http.Request) {
		httpx.OK(w, r, http.StatusOK, nil)
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

type answer struct {
	status int
	header http.Header
	body   struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
}

func get(t *testing.T, srv *httptest.Server, method, path, authorization string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
	require.NoError(t, err)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	out := answer{status: res.StatusCode, header: res.Header}
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func tokenAt(t *testing.T, at time.Time) string {
	t.Helper()
	token, _, err := newTokens(t, at).NewAccessToken(15, 7, 99)
	require.NoError(t, err)
	return token
}

func TestRequireLetsAValidTokenThroughAndSetsThePrincipal(t *testing.T) {
	srv := newServer(t)
	res := get(t, srv, http.MethodGet, "/api/v1/whoami", "Bearer "+tokenAt(t, time.Now().UTC()))

	require.Equal(t, http.StatusOK, res.status)
	require.EqualValues(t, 15, res.body.Data["personnel_id"])
	require.EqualValues(t, 7, res.body.Data["organization_id"])
	require.EqualValues(t, 99, res.body.Data["session_id"])
}

func TestRequireAcceptsTheSchemeInAnyCase(t *testing.T) {
	srv := newServer(t)
	res := get(t, srv, http.MethodGet, "/api/v1/whoami", "bearer "+tokenAt(t, time.Now().UTC()))
	require.Equal(t, http.StatusOK, res.status)
}

func TestRequireRefusesBadCredentialsWithAuthTokenInvalid(t *testing.T) {
	srv := newServer(t)
	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "15", "organization_id": 7, "sid": "99", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("another-secret-another-secret-123456"))
	require.NoError(t, err)

	tests := map[string]string{
		"no header":            "",
		"wrong scheme":         "Basic " + tokenAt(t, time.Now().UTC()),
		"scheme only":          "Bearer",
		"empty token":          "Bearer ",
		"malformed token":      "Bearer not-a-jwt",
		"wrongly signed":       "Bearer " + forged,
		"token without scheme": tokenAt(t, time.Now().UTC()),
	}
	for name, authorization := range tests {
		t.Run(name, func(t *testing.T) {
			res := get(t, srv, http.MethodGet, "/api/v1/whoami", authorization)
			require.Equal(t, http.StatusUnauthorized, res.status)
			require.False(t, res.body.Success)
			require.Equal(t, "UNAUTHENTICATED", res.body.Error.Code)
			require.Equal(t, auth.ReasonTokenInvalid, res.body.Error.Details["reason"])
			require.Equal(t, `Bearer realm="siapp"`, res.header.Get("WWW-Authenticate"))
		})
	}
}

func TestRequireAnswersAnExpiredTokenWithTokenExpired(t *testing.T) {
	srv := newServer(t)
	// Issued by a clock two hours behind the server's, so it expired an hour and three
	// quarters ago.
	expired := tokenAt(t, time.Now().Add(-2*time.Hour).UTC())

	res := get(t, srv, http.MethodGet, "/api/v1/whoami", "Bearer "+expired)
	require.Equal(t, http.StatusUnauthorized, res.status)
	require.Equal(t, "TOKEN_EXPIRED", res.body.Error.Code, "the client refreshes instead of logging out")
	require.Equal(t, auth.ReasonTokenExpired, res.body.Error.Details["reason"])
}

func TestRequireIsDefaultDeny(t *testing.T) {
	srv := newServer(t)

	t.Run("an unknown path is 401, not 404, without a token", func(t *testing.T) {
		res := get(t, srv, http.MethodGet, "/api/v1/no/such/route", "")
		require.Equal(t, http.StatusUnauthorized, res.status)
	})
	t.Run("an unknown path is 404 with a token", func(t *testing.T) {
		res := get(t, srv, http.MethodGet, "/api/v1/no/such/route", "Bearer "+tokenAt(t, time.Now().UTC()))
		require.Equal(t, http.StatusNotFound, res.status)
	})
	t.Run("the public allowlist is matched by exact method and path", func(t *testing.T) {
		require.Equal(t, http.StatusOK, get(t, srv, http.MethodGet, "/api/v1/health", "").status)
		// Same path, another method: not public.
		require.Equal(t, http.StatusUnauthorized, get(t, srv, http.MethodDelete, "/api/v1/health", "").status)
		// A sub-path of a public route: not public.
		require.Equal(t, http.StatusUnauthorized, get(t, srv, http.MethodGet, "/api/v1/health/deep", "").status)
		// A trailing slash is another path.
		require.Equal(t, http.StatusUnauthorized, get(t, srv, http.MethodGet, "/api/v1/health/", "").status)
	})
	t.Run("a parameterised route is protected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, get(t, srv, http.MethodPost, "/api/v1/whoami/12", "").status)
	})
}

func TestPublicRoutes(t *testing.T) {
	require.True(t, auth.IsPublic(http.MethodGet, "/api/v1/health"))
	require.True(t, auth.IsPublic(http.MethodPost, "/api/v1/auth/login"))
	require.False(t, auth.IsPublic(http.MethodGet, "/api/v1/auth/login"))
	require.True(t, auth.IsPublic(http.MethodPost, "/api/v1/auth/refresh"))
	require.False(t, auth.IsPublic(http.MethodPost, "/api/v1/auth/logout"), "logout needs a token")
	require.False(t, auth.IsPublic(http.MethodGet, "/api/v1/auth/me"))
}

func TestFromIsEmptyOnAPublicRoute(t *testing.T) {
	_, ok := auth.From(t.Context())
	require.False(t, ok)

	ctx := auth.WithPrincipal(t.Context(), auth.Principal{PersonnelID: 1, OrganizationID: 2, SessionID: 3})
	p, ok := auth.From(ctx)
	require.True(t, ok)
	require.Equal(t, auth.Principal{PersonnelID: 1, OrganizationID: 2, SessionID: 3}, p)
}
