package testkit

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/httpx"
)

// JWTSecret is the signing secret of every test server. Tokens that a test signs itself
// (an expired one, one from another clock) use it too.
const JWTSecret = "testkit-jwt-secret-testkit-jwt-secret" //nolint:gosec // a fixed test key, not a credential

// Tokens returns the token signer the test servers verify with, on the system clock.
// Build a module's usecase with it, so the tokens it issues pass the server's middleware.
func Tokens(t testing.TB) *auth.Tokens {
	t.Helper()
	tokens, err := auth.NewTokens([]byte(JWTSecret), clock.System{})
	require.NoError(t, err)
	return tokens
}

// Token returns a valid access token for a person of an organization, for tests of routes
// behind authentication. Nothing checks the session id (revocation arrives in Phase 01.2),
// so the person and organization need not exist in the database; create them with NewOrg
// when the handler under test reads tenant data.
func Token(t testing.TB, organizationID, personnelID int64) string {
	t.Helper()
	token, _, err := Tokens(t).NewAccessToken(personnelID, organizationID, 1)
	require.NoError(t, err)
	return token
}

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
// Authentication is the real default-deny middleware (auth.Require): a test route needs a
// token from Token, unless it is one of auth.PublicRoutes.
func NewServer(t testing.TB, deps Deps) *httptest.Server {
	t.Helper()
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       logger,
		DB:           Pool(t),
		Authenticate: auth.Require(Tokens(t)),
	})
	if deps.Mount != nil {
		deps.Mount(router)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}
