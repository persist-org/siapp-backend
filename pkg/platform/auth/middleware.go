package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
)

// Token failure reasons (details.reason).
const (
	// ReasonTokenInvalid: no token, or one that is malformed, wrongly signed or incomplete.
	// 401 UNAUTHENTICATED: the client goes to the login screen.
	ReasonTokenInvalid = "AUTH_TOKEN_INVALID" //nolint:gosec // a reason code, not a secret
	// ReasonTokenExpired: a genuine access token past its expiry. 401 TOKEN_EXPIRED: the
	// client refreshes once and retries.
	ReasonTokenExpired = "AUTH_TOKEN_EXPIRED" //nolint:gosec // a reason code, not a secret
)

var (
	errTokenInvalid = apperr.ErrUnauthenticated.WithReason(ReasonTokenInvalid)
	errTokenExpired = apperr.New(http.StatusUnauthorized, apperr.CodeTokenExpired, ReasonTokenExpired,
		"Sesi sudah kedaluwarsa. Perbarui sesi Anda.")
)

// Route identifies an endpoint by its exact method and path.
type Route struct {
	Method string
	Path   string
}

// PublicRoutes is the allowlist of endpoints that work without an access token. Every
// other request needs a valid Bearer token.
//
// Adding an entry here also needs `security: []` on the operation in api/openapi.yaml,
// except for platform routes the contract does not describe (the health probe); a guard
// test in cmd/api checks that the two agree. Later phases add their entries: the refresh
// endpoint (01.2) and the Personel webhook receiver (05.3).
var PublicRoutes = []Route{
	{http.MethodGet, "/api/v1/health"},
	{http.MethodPost, "/api/v1/auth/login"},
}

// IsPublic reports whether method and path are exactly on the allowlist.
func IsPublic(method, path string) bool {
	for _, r := range PublicRoutes {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}

// Principal is the authenticated caller, taken from the access token.
type Principal struct {
	PersonnelID    int64
	OrganizationID int64
	SessionID      int64
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p. Require does this; tests of code that only reads
// the principal may too.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// From returns the authenticated caller of ctx. ok is false on public routes.
//
// ⚠ This is the tenancy rule (LOCK principle 2): every WithTenant call takes its
// organization from From(ctx).OrganizationID, never from a path, query, header or body. A
// request can therefore only ever name data of the organization its token was issued for.
func From(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Require is the authentication middleware. It is mounted once, on the whole router.
//
// ⚠ It is default-deny. A request is let through without a token only when its method and
// path are exactly in PublicRoutes. A route that someone forgets to think about therefore
// fails closed with 401, instead of silently staying open as it would if each route group
// had to opt in to protection.
//   - Matching is exact, never by prefix, so a public route can never expose a sub-route.
//   - Unknown paths get 401 rather than 404 without a token, so an anonymous caller cannot
//     map out which routes exist.
//
// Access tokens are stateless: a token is verified by signature and expiry, with no
// database lookup. Phase 01.2 adds the revocation check that cuts a token off the moment
// its session ends.
func Require(tokens *Tokens) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsPublic(r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			claims, err := authenticate(tokens, r)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="siapp"`)
				if errors.Is(err, ErrExpiredToken) {
					httpx.Error(w, r, errTokenExpired)
				} else {
					httpx.Error(w, r, errTokenInvalid)
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal(claims))))
		})
	}
}

func authenticate(tokens *Tokens, r *http.Request) (Claims, error) {
	header := r.Header.Get("Authorization")
	// The scheme name is case-insensitive (RFC 7235).
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return Claims{}, ErrInvalidToken
	}
	return tokens.ParseAccessToken(strings.TrimSpace(token))
}
