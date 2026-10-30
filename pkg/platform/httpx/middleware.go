package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"siapp/pkg/platform/apperr"
)

// HeaderRequestID carries the request ID in both directions.
const HeaderRequestID = "X-Request-Id"

const (
	maxRequestIDLen = 128
	corsMaxAgeSec   = 600
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the request ID stored in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requestID reuses the caller's X-Request-Id so a request can be traced across the mobile
// app, a proxy and this API. A client-supplied ID is only trusted when it is short and made
// of safe characters, because it is written into every log line for the request.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

func newRequestID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// accessLog logs one line per request. It deliberately logs only the fields below and
// never the headers, so Authorization tokens cannot leak into logs.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", loggedPath(r.URL.Path)),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
				slog.String("requestId", RequestID(r.Context())),
			)
		})
	}
}

// blobPathMarker is what precedes the token in the local storage's blob URLs
// (/api/v1/media/blobs/{token}).
const blobPathMarker = "/media/blobs/"

// loggedPath is the path as the access log writes it.
//
// ⚠ The token of a blob URL is a credential for as long as it lives: whoever holds an upload
// token can write that object, and whoever holds a view token can read that photo. The access
// log is read by operators and shipped to log stores, so the token is replaced by a
// placeholder; the rest of the path stays, so a log line still says what was asked for.
func loggedPath(path string) string {
	if i := strings.Index(path, blobPathMarker); i >= 0 {
		return path[:i+len(blobPathMarker)] + "{token}"
	}
	return path
}

// recoverer turns a panic into a logged stack trace and a 500 INTERNAL_ERROR response.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is net/http's signal to abort the response silently.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
					slog.String("requestId", RequestID(r.Context())),
				)
				writeAppError(w, r, apperr.ErrInternal)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// cors allows browser requests from the configured origins only. Origins are matched
// exactly; API auth uses bearer tokens, so credentials (cookies) are never allowed.
func cors(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		allowed[strings.TrimSpace(o)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin")
			if _, ok := allowed[origin]; !ok {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", HeaderRequestID)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+HeaderRequestID)
				h.Set("Access-Control-Max-Age", strconv.Itoa(corsMaxAgeSec))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
