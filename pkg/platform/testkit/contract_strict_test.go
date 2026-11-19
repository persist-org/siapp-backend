package testkit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The strict checker itself: it must see each way a response can drift from the contract, or the
// tests that rely on it prove nothing.
func TestCheckResponseFindsEveryKindOfDrift(t *testing.T) {
	doc, routes := loadContract(t)
	route, _, ok := findRoute(routes, "GET", "/patrols/active")
	require.True(t, ok)
	op := route.Method + " " + route.Path
	check := func(status int, body string) []finding {
		fs, _ := checkResponse(doc, op, route.Operation, status, "application/json", []byte(body))
		return fs
	}
	kinds := func(fs []finding) map[string]string {
		out := map[string]string{}
		for _, f := range fs {
			out[f.Pointer] = f.Kind
		}
		return out
	}

	// meta.server_time is an optional, non-nullable date-time in the contract (Meta), so a documented response
	// sends a time there, never null.
	const serverTime = `"2026-06-01T03:00:00Z"`
	t.Run("a response as documented passes", func(t *testing.T) {
		require.Empty(t, check(200, `{"success":true,"data":[],"meta":{"request_id":"r","server_time":`+serverTime+`}}`))
	})
	t.Run("a property left out is missing, even when the contract does not require it", func(t *testing.T) {
		require.Equal(t, map[string]string{"/meta/server_time": "missing"},
			kinds(check(200, `{"success":true,"data":[],"meta":{"request_id":"r"}}`)))
	})
	t.Run("a property the contract does not list is not documented", func(t *testing.T) {
		require.Equal(t, map[string]string{"/extra": "not documented"},
			kinds(check(200, `{"success":true,"data":[],"extra":1,"meta":{"request_id":"r","server_time":`+serverTime+`}}`)))
	})
	t.Run("null where the contract does not allow it", func(t *testing.T) {
		require.Equal(t, map[string]string{"/meta/request_id": "null not allowed", "/meta/server_time": "null not allowed"},
			kinds(check(200, `{"success":true,"data":[],"meta":{"request_id":null,"server_time":null}}`)))
	})
	t.Run("a wrong type", func(t *testing.T) {
		require.Equal(t, map[string]string{"/success": "wrong type"},
			kinds(check(200, `{"success":"yes","data":[],"meta":{"request_id":"r","server_time":`+serverTime+`}}`)))
	})
	t.Run("a status the operation does not document", func(t *testing.T) {
		fs := check(418, `{}`)
		require.Len(t, fs, 1)
		require.Equal(t, "status not documented", fs[0].Kind)
	})
}
