package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeDB stands in for the database in tests that exercise middleware only. Phase 00.2
// adds a health test against the real database through testkit.
type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestServer(t *testing.T, logs io.Writer, extra func(r *http.ServeMux)) *httptest.Server {
	t.Helper()
	return newTestServerWithDB(t, logs, fakeDB{}, extra)
}

func newTestServerWithDB(t *testing.T, logs io.Writer, db Pinger, extra func(r *http.ServeMux)) *httptest.Server {
	t.Helper()
	router := NewRouter(RouterConfig{
		Logger:      slog.New(slog.NewJSONHandler(logs, nil)),
		CORSOrigins: []string{"http://localhost:3000"},
		DB:          db,
		// These tests are about the other middleware, not authentication (auth has its
		// own tests), so authentication is explicitly switched off here.
		Authenticate: func(next http.Handler) http.Handler { return next },
	})
	if extra != nil {
		mux := http.NewServeMux()
		extra(mux)
		router.Mount("/test", mux)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t, io.Discard, nil)

	t.Run("echoes the request ID", func(t *testing.T) {
		res := get(t, srv.URL+"/api/v1/health", http.Header{HeaderRequestID: {"req-123"}})
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, "req-123", res.Header.Get(HeaderRequestID))

		var body map[string]string
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		require.Equal(t, map[string]string{"status": "ok"}, body)
	})

	t.Run("generates a request ID when missing or unsafe", func(t *testing.T) {
		for _, id := range []string{"", "has spaces and \"quotes\"", strings.Repeat("a", maxRequestIDLen+1)} {
			res := get(t, srv.URL+"/api/v1/health", http.Header{HeaderRequestID: {id}})
			require.Equal(t, http.StatusOK, res.StatusCode)
			got := res.Header.Get(HeaderRequestID)
			require.NotEmpty(t, got)
			require.NotEqual(t, id, got)
		}
	})
}

func TestHealthWithoutDatabase(t *testing.T) {
	router := NewRouter(RouterConfig{
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Authenticate: func(next http.Handler) http.Handler { return next },
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	res := get(t, srv.URL+"/api/v1/health", http.Header{HeaderRequestID: {"req-no-db"}})
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "req-no-db", res.Header.Get(HeaderRequestID))

	var body map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, map[string]string{"status": "ok"}, body)
}

func TestNewRouterRequiresAuthenticate(t *testing.T) {
	require.Panics(t, func() {
		NewRouter(RouterConfig{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), DB: fakeDB{}})
	})
}

func TestClientIP(t *testing.T) {
	request := func(remote string, headers map[string]string) *http.Request {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	t.Run("no header configured uses the connection address and ignores spoofable headers", func(t *testing.T) {
		got := ClientIPFunc("")(request("10.0.0.5:4321", map[string]string{"X-Forwarded-For": "1.2.3.4"}))
		require.Equal(t, "10.0.0.5", got)
	})
	t.Run("configured header: last entry, the one our proxy appended", func(t *testing.T) {
		got := ClientIPFunc("X-Forwarded-For")(request("10.0.0.5:1", map[string]string{"X-Forwarded-For": "6.6.6.6, 203.0.113.9"}))
		require.Equal(t, "203.0.113.9", got)
	})
	t.Run("single value and IPv6", func(t *testing.T) {
		require.Equal(t, "2001:db8::1", ClientIPFunc("X-Real-IP")(request("10.0.0.5:1", map[string]string{"X-Real-IP": "2001:db8::1"})))
	})
	t.Run("missing or garbage header falls back to the connection address", func(t *testing.T) {
		require.Equal(t, "10.0.0.5", ClientIPFunc("X-Real-IP")(request("10.0.0.5:1", nil)))
		require.Equal(t, "10.0.0.5", ClientIPFunc("X-Real-IP")(request("10.0.0.5:1", map[string]string{"X-Real-IP": "not-an-ip"})))
	})
	t.Run("remote address without a port", func(t *testing.T) {
		require.Equal(t, "pipe", ClientIPFunc("")(request("pipe", nil)))
	})
}

func TestHealthDatabaseDown(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServerWithDB(t, &logs, fakeDB{err: errors.New("connection refused")}, nil)

	res := get(t, srv.URL+"/api/v1/health", nil)
	require.Equal(t, http.StatusServiceUnavailable, res.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, map[string]string{"status": "unavailable"}, body)
	require.Contains(t, logs.String(), "connection refused")
}

func TestRecovererAndAccessLog(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServer(t, &logs, func(mux *http.ServeMux) {
		mux.HandleFunc("/test/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	})

	res := get(t, srv.URL+"/test/panic", http.Header{
		HeaderRequestID: {"req-panic"},
		"Authorization": {"Bearer secret-token"},
	})
	require.Equal(t, http.StatusInternalServerError, res.StatusCode)

	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, "INTERNAL_ERROR", body.Error.Code)

	out := logs.String()
	require.Contains(t, out, `"panic":"boom"`)
	require.Contains(t, out, `"status":500`)
	require.Contains(t, out, `"requestId":"req-panic"`)
	require.NotContains(t, out, "secret-token")
}

func TestCORS(t *testing.T) {
	srv := newTestServer(t, io.Discard, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, srv.URL+"/api/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "http://localhost:3000", res.Header.Get("Access-Control-Allow-Origin"))

	res = get(t, srv.URL+"/api/v1/health", http.Header{"Origin": {"https://evil.example.com"}})
	require.Empty(t, res.Header.Get("Access-Control-Allow-Origin"))
}
