package personeltest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/personel/personeltest"
)

// The fake must answer like kpi-personalia-api: the 200 limit counts distinct
// NIKs, and going over it is 422 with error "too_many_niks".
func TestEmployeesLimitCountsDistinctNIKs(t *testing.T) {
	fake := personeltest.New()
	srv := httptest.NewServer(fake.Handler())
	t.Cleanup(srv.Close)

	post := func(niks []string) (int, map[string]any) {
		body, err := json.Marshal(map[string]any{"niks": niks})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, srv.URL+personeltest.BasePath+"/employees", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("X-Integration-Key", fake.Key)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var env map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
		return resp.StatusCode, env
	}

	niks := make([]string, 201)
	for i := range niks {
		niks[i] = fmt.Sprintf("N%04d", i)
	}
	status, env := post(niks)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "too_many_niks", env["error"])

	// 202 entries, but only 200 distinct NIKs once trimmed.
	withDupes := append(append([]string(nil), niks[:200]...), "N0000", " N0001 ")
	status, _ = post(withDupes)
	require.Equal(t, http.StatusOK, status)

	status, env = post([]string{})
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "validation", env["error_type"])
}
