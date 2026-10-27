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
// behind authentication. Its session id is 1 and nothing checks that the session exists, so
// the person and organization need not exist in the database; create them with NewOrg when
// the handler under test reads tenant data. A test of logout or revocation logs in for real.
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
	// Revocations is the revocation list the middleware checks. A test that ends sessions
	// passes the same one to the code under test; nil makes a private one.
	Revocations *auth.Revocations
}

// NewRevocations returns a revocation list on the test Redis with a key prefix of its own.
func NewRevocations(t testing.TB, logger *slog.Logger) *auth.Revocations {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return auth.NewRevocations(Redis(t), RandomPrefix(t), logger)
}

// NewServer builds the real router (same middleware stack as cmd/api) on an httptest
// server, connected to the test database as siapp_app.
//
// Authentication is the real default-deny middleware (auth.Require): a test route needs a
// token from Token, unless it is one of auth.PublicRoutes.
func NewServer(t testing.TB, deps Deps) *httptest.Server {
	t.Helper()
	return NewServerAt(t, func(string) Deps { return deps })
}

// NewServerAt is NewServer for a module that has to know the server's own address before its
// routes exist (the local media storage builds its URLs from it). The listener is opened
// first, deps is called with the base URL it will answer on (http://127.0.0.1:port), and only
// then is the router built and the server started.
func NewServerAt(t testing.TB, makeDeps func(baseURL string) Deps) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(srv.Close)
	deps := makeDeps("http://" + srv.Listener.Addr().String())

	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	revocations := deps.Revocations
	if revocations == nil {
		revocations = NewRevocations(t, logger)
	}
	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       logger,
		DB:           Pool(t),
		Authenticate: auth.Require(Tokens(t), revocations),
	})
	if deps.Mount != nil {
		deps.Mount(router)
	}
	srv.Config.Handler = router
	srv.Start()
	return srv
}
