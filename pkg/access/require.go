package access

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
)

type resolutionKey struct{}

// WithResolution returns ctx carrying the resolution. Require does this; tests of code that
// only reads it may too.
func WithResolution(ctx context.Context, r Resolution) context.Context {
	return context.WithValue(ctx, resolutionKey{}, r)
}

// From returns the resolution Require put in ctx. ok is false outside a route Require guards.
// A usecase reads it here and does not resolve again for the same permission.
func From(ctx context.Context) (r Resolution, ok bool) {
	r, ok = ctx.Value(resolutionKey{}).(Resolution)
	return r, ok
}

// Require guards a field route with a permission: the caller must hold perm through an active
// membership, and the Resolution is put in the request context for the usecase (see From).
// A caller who does not gets 403 PERMISSION_DENIED before the handler runs.
//
// ⚠ This is the coarse gate only. It asks about "today", which is each membership's own business
// date (the date in its project's timezone), because it cannot know which project the request
// is about. The usecase knows the project and the date of the record, so it checks again with
// them (Resolve with that date, or AssertTeam): an ended membership, or one that starts
// tomorrow, is decided there, per record.
//
// ⚠ Web admins never go through Require. They have no field role; they sign in on the web and
// use /admin routes, which RequireAdmin guards. A person who is both is judged here as the
// field person they are.
//
// It must be mounted behind auth.Require, like every route. perm must be a key of the
// catalog: a typo is a mistake in the code that mounts the route, so it panics when the
// middleware is built (at start-up), not on the first request.
func (s *Service) Require(perm Permission) func(http.Handler) http.Handler {
	if !IsKnown(perm) {
		panic(fmt.Sprintf("access.Require: %q is not a permission of the catalog", perm))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.From(r.Context())
			if !ok {
				httpx.Error(w, r, apperr.ErrUnauthenticated)
				return
			}
			var res Resolution
			err := database.WithTenant(r.Context(), s.pool, p.OrganizationID, func(q *db.Queries) error {
				var err error
				res, err = s.ResolveToday(r.Context(), q, p.PersonnelID, perm)
				return err
			})
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !res.Allowed {
				httpx.Error(w, r, ErrPermissionDenied)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithResolution(r.Context(), res)))
		})
	}
}

// ProjectOf says which project a request is about, by its code. "" means it names none.
type ProjectOf func(r *http.Request) string

// ProjectParam reads the project code from a path parameter ({project_code}).
func ProjectParam(name string) ProjectOf {
	return func(r *http.Request) string { return chi.URLParam(r, name) }
}

// ProjectOfTeamParam reads the project from a regu code in a path parameter ({team_code}): a
// regu code is its project's code and a number.
func ProjectOfTeamParam(name string) ProjectOf {
	return func(r *http.Request) string { return ProjectCodeOfTeam(chi.URLParam(r, name)) }
}

// RequireBusinessLine guards a route that exists for one business line (module 16, cleaning):
// the project the request is about must have that line, or the answer is 403
// FEATURE_NOT_ENABLED. A project that does not exist, and a request that names none, get the same
// answer, so the route cannot be used to find out which project codes exist.
//
// It says nothing about whether the caller may use the project: that is Require's and the
// usecase's job. Mount it after Require, behind auth.Require.
func (s *Service) RequireBusinessLine(line BusinessLine, projectOf ProjectOf) func(http.Handler) http.Handler {
	if line != LineSecurity && line != LineCleaning {
		panic(fmt.Sprintf("access.RequireBusinessLine: %q is not a business line", line))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.From(r.Context())
			if !ok {
				httpx.Error(w, r, apperr.ErrUnauthenticated)
				return
			}
			project := projectOf(r)
			if project == "" {
				httpx.Error(w, r, ErrFeatureNotEnabled)
				return
			}
			err := database.WithTenant(r.Context(), s.pool, p.OrganizationID, func(q *db.Queries) error {
				return s.AssertBusinessLine(r.Context(), q, project, line)
			})
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
