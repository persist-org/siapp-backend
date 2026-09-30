package auth

import (
	"context"
	"net/http"
	"strings"

	"siapp/internal/db"
	"siapp/internal/platform/apperr"
	"siapp/internal/platform/httpx"
)

// Route identifies an endpoint by its exact method and path.
type Route struct {
	Method string
	Path   string
}

// PublicRoutes is the allowlist of endpoints that work without an access token: the health
// probe and the sign-in flow. Every other request needs a valid Bearer JWT.
//
// Adding an entry here also needs `security: []` on the operation in api/openapi.yaml; a
// test checks that the two agree.
var PublicRoutes = []Route{
	{http.MethodGet, "/api/v1/health"},
	{http.MethodPost, "/api/v1/auth/login"},
	// Refresh authenticates with the refresh token in its body; the access token has
	// usually expired by then.
	{http.MethodPost, "/api/v1/auth/refresh"},
}

// Principal is the authenticated caller, taken from the access token.
type Principal struct {
	UserID     string
	SystemRole db.UsersSystemRole
	SessionID  string
}

type principalKey struct{}

// From returns the authenticated caller of ctx. ok is false on public routes.
func From(ctx context.Context) (p Principal, ok bool) {
	p, ok = ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Require is the authentication middleware. It is mounted once on the whole router.
//
// ⚠ It is default-deny. A request is let through without a token only when its method and
// path are exactly in PublicRoutes. A route that someone forgets to think about therefore
// fails closed with 401, instead of silently staying open as it would if each route group
// had to opt in to protection.
//   - Matching is exact, never by prefix, so a public route can never expose a sub-route.
//   - Unknown paths get 401 rather than 404 without a token, so an anonymous caller cannot
//     map out which routes exist.
//
// ⚠ Access tokens are stateless: a token is verified by signature and expiry, with no
// database lookup, so by itself it would stay valid for up to 15 minutes after its session
// ended. To close that gap the session ID in the token is also looked up in Revocations, one
// fast Redis read, and a revoked session is refused at once. That lookup fails open: if Redis
// is down the token is accepted on signature and expiry alone, see Revocations.
//
// Not everything is checked live. The role in the token is whatever it was at login or the
// last refresh, so a role change reaches the user within 15 minutes.
func Require(tokens *Tokens, revocations *Revocations) func(http.Handler) http.Handler {
	public := make(map[Route]struct{}, len(PublicRoutes))
	for _, r := range PublicRoutes {
		public[r] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := public[Route{Method: r.Method, Path: r.URL.Path}]; ok {
				next.ServeHTTP(w, r)
				return
			}

			claims, err := authenticate(tokens, r)
			// Cheapest check first: Redis is only asked about a token that is otherwise good.
			if err != nil || revocations.IsRevoked(r.Context(), claims.SessionID) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="siapp"`)
				httpx.Error(w, r, apperr.ErrUnauthenticated)
				return
			}
			ctx := context.WithValue(r.Context(), principalKey{}, Principal(claims))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func authenticate(tokens *Tokens, r *http.Request) (Claims, error) {
	header := r.Header.Get("Authorization")
	// The scheme name is case-insensitive (RFC 7235).
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return Claims{}, ErrInvalidToken
	}
	return tokens.ParseAccessToken(strings.TrimSpace(token))
}
