package httpx_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// TestHealthWithDatabase also proves the contract client: it fails if /api/v1/health or its
// response drifts from api/openapi.yaml.
func TestHealthWithDatabase(t *testing.T) {
	client := testkit.NewClient(t, testkit.NewServer(t, testkit.Deps{}))

	res := client.Do(http.MethodGet, "/api/v1/health", nil)
	require.Equal(t, http.StatusOK, res.Status)

	var body map[string]string
	res.JSON(t, &body)
	require.Equal(t, map[string]string{"status": "ok"}, body)
}
