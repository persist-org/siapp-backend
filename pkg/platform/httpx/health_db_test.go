package httpx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/database"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

func getHealth(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/v1/health", nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, string(body)
}

func TestHealthWithDatabase(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{})

	status, body := getHealth(t, srv.URL)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"status":"ok"}`, body)
}

func TestHealthReturns503WhenDatabaseIsDown(t *testing.T) {
	// A pool that has been closed fails every ping, which is what an unreachable database
	// looks like to the health check.
	pool, err := database.Open(t.Context(), testkit.AppURL(t))
	require.NoError(t, err)
	pool.Close()

	router := httpx.NewRouter(httpx.RouterConfig{
		Logger:       quiet,
		DB:           pool,
		Authenticate: func(next http.Handler) http.Handler { return next },
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	status, body := getHealth(t, srv.URL)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.JSONEq(t, `{"status":"unavailable"}`, body)
}
