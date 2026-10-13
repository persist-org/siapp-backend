package httpx_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

type echoRequest struct {
	Name  string `json:"name" validate:"required,min=3"`
	Email string `json:"email" validate:"omitempty,email"`
	Items []struct {
		Qty int `json:"qty" validate:"min=1"`
	} `json:"items" validate:"dive"`
}

type item struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// testRoutes are the test-only routes of this file. They are not in the contract, so
// their responses are checked against the contract's schemas with testkit.ValidateSchema.
func testRoutes(r chi.Router) {
	r.Post("/test/echo", func(w http.ResponseWriter, r *http.Request) {
		var req echoRequest
		if err := httpx.Decode(r, &req); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.OK(w, r, http.StatusCreated, req)
	})
	r.Get("/test/nothing", func(w http.ResponseWriter, r *http.Request) { httpx.OK(w, r, http.StatusOK, nil) })
	r.Get("/test/list", func(w http.ResponseWriter, r *http.Request) {
		q, err := httpx.ParseListQuery(r)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.List(w, r, []item{{ID: 1, Name: "a"}}, q.WithTotal(41))
	})
	r.Get("/test/empty-list", func(w http.ResponseWriter, r *http.Request) {
		httpx.List[item](w, r, nil, httpx.Page{Page: 1, Limit: 20})
	})
	r.Get("/test/delta", func(w http.ResponseWriter, r *http.Request) {
		httpx.Delta(w, r, []item{{ID: 9, Name: "n"}}, []map[string]any{{"id": 8, "deleted_at": "2026-10-12T02:00:00Z"}},
			httpx.DeltaMeta{ServerTime: time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC), NextCursor: "abc", HasMore: true})
	})
	r.Get("/test/delta-last", func(w http.ResponseWriter, r *http.Request) {
		httpx.Delta[item, any](w, r, nil, nil, httpx.DeltaMeta{ServerTime: time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)})
	})
	r.Get("/test/boom", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, errors.New("pq: password authentication failed for user root"))
	})
	r.Get("/test/reason", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
			"ATTENDANCE_QR_EXPIRED", "QR sudah kedaluwarsa. Pindai ulang.").WithDetails(map[string]any{"retry_after": 5}))
	})
	r.Get("/test/wrapped", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, errors.Join(errors.New("context"), apperr.ErrConflict))
	})
}

type result struct {
	status int
	header http.Header
	raw    []byte
	body   map[string]any
}

func call(t *testing.T, srv string, method, path, body string, header http.Header) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv+path, strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	out := result{status: res.StatusCode, header: res.Header, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func TestEnvelopes(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: testRoutes})
	reqID := http.Header{httpx.HeaderRequestID: {"req-1"}}

	t.Run("success envelope", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodPost, "/test/echo", `{"name":"Budi"}`, reqID)
		require.Equal(t, http.StatusCreated, res.status)
		require.Equal(t, "application/json; charset=utf-8", res.header.Get("Content-Type"))
		testkit.ValidateSchema(t, "SuccessEnvelope", res.raw)
		require.Equal(t, true, res.body["success"])
		require.Equal(t, map[string]any{"request_id": "req-1"}, res.body["meta"])
	})

	t.Run("action without data sends data null", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/nothing", "", nil)
		testkit.ValidateSchema(t, "SuccessEnvelope", res.raw)
		data, present := res.body["data"]
		require.True(t, present, "data key must be present")
		require.Nil(t, data)
	})

	t.Run("list envelope with pagination", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/list?page=2&limit=20", "", nil)
		require.Equal(t, http.StatusOK, res.status)
		testkit.ValidateSchema(t, "ListEnvelope", res.raw)
		require.Equal(t, map[string]any{"page": 2.0, "limit": 20.0, "total": 41.0, "total_pages": 3.0}, res.body["pagination"])
	})

	t.Run("empty list is [] not null", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/empty-list", "", nil)
		testkit.ValidateSchema(t, "ListEnvelope", res.raw)
		require.Equal(t, []any{}, res.body["data"])
		require.EqualValues(t, 0, res.body["pagination"].(map[string]any)["total_pages"])
	})

	t.Run("delta envelope", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/delta", "", reqID)
		testkit.ValidateSchema(t, "SuccessEnvelope", res.raw)
		meta := res.body["meta"].(map[string]any)
		testkit.ValidateSchema(t, "DeltaMeta", mustJSON(t, meta))
		require.Equal(t, "2026-10-12T03:00:00.000Z", meta["server_time"])
		require.Equal(t, "abc", meta["next_cursor"])
		require.Equal(t, true, meta["has_more"])
		require.NotContains(t, res.body, "pagination", "delta responses carry no pagination")
		require.Len(t, res.body["deleted"], 1)
	})

	t.Run("last delta page omits next_cursor and sends empty arrays", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/delta-last", "", nil)
		meta := res.body["meta"].(map[string]any)
		testkit.ValidateSchema(t, "DeltaMeta", mustJSON(t, meta))
		require.NotContains(t, meta, "next_cursor")
		require.Equal(t, false, meta["has_more"])
		require.Equal(t, []any{}, res.body["data"])
		require.Equal(t, []any{}, res.body["deleted"])
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func TestErrorEnvelope(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: testRoutes})
	reqID := http.Header{httpx.HeaderRequestID: {"req-err"}}

	t.Run("reason goes to details.reason next to other details", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/reason", "", reqID)
		require.Equal(t, http.StatusUnprocessableEntity, res.status)
		testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
		errBody := res.body["error"].(map[string]any)
		require.Equal(t, "UNPROCESSABLE", errBody["code"])
		require.Equal(t, map[string]any{"reason": "ATTENDANCE_QR_EXPIRED", "retry_after": 5.0}, errBody["details"])
		require.Equal(t, false, res.body["success"])
		require.Equal(t, map[string]any{"request_id": "req-err"}, res.body["meta"])
	})

	t.Run("an app error wrapped in another error is still recognised", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/wrapped", "", nil)
		require.Equal(t, http.StatusConflict, res.status)
		testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
	})

	t.Run("unknown error becomes INTERNAL_ERROR and is not exposed", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/test/boom", "", reqID)
		require.Equal(t, http.StatusInternalServerError, res.status)
		testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
		require.Equal(t, "INTERNAL_ERROR", res.body["error"].(map[string]any)["code"])
		require.NotContains(t, string(res.raw), "password")
	})

	t.Run("unknown paths use the envelope", func(t *testing.T) {
		res := call(t, srv.URL, http.MethodGet, "/api/v1/nope", "", nil)
		require.Equal(t, http.StatusNotFound, res.status)
		testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
		require.Equal(t, "NOT_FOUND", res.body["error"].(map[string]any)["code"])
	})

	t.Run("shared errors are not mutated by the With helpers", func(t *testing.T) {
		_ = apperr.ErrConflict.WithDetails(map[string]any{"x": 1}).WithReason("X").WithFields(apperr.FieldError{Field: "f"})
		require.Nil(t, apperr.ErrConflict.Details)
		require.Empty(t, apperr.ErrConflict.Reason)
		require.Nil(t, apperr.ErrConflict.Fields)
	})
}

func TestDecode(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: testRoutes})
	post := func(body string) result {
		return call(t, srv.URL, http.MethodPost, "/test/echo", body, nil)
	}
	fieldsOf := func(res result) []map[string]any {
		var out []map[string]any
		for _, f := range res.body["error"].(map[string]any)["errors"].([]any) {
			out = append(out, f.(map[string]any))
		}
		return out
	}

	t.Run("valid body", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, post(`{"name":"Budi","items":[{"qty":2}]}`).status)
	})

	validation := []struct {
		name      string
		body      string
		wantField string
		wantCode  string
	}{
		{"missing required field", `{}`, "name", "REQUIRED"},
		{"too short", `{"name":"ab"}`, "name", "TOO_SHORT"},
		{"bad email", `{"name":"Budi","email":"nope"}`, "email", "INVALID"},
		{"nested field uses the json path", `{"name":"Budi","items":[{"qty":0}]}`, "items[0].qty", "TOO_SMALL"},
		{"unknown field is rejected", `{"name":"Budi","nickname":"x"}`, "nickname", "INVALID"},
		{"wrong type", `{"name":123}`, "name", "INVALID"},
	}
	for _, tc := range validation {
		t.Run(tc.name, func(t *testing.T) {
			res := post(tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, res.status)
			testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
			errBody := res.body["error"].(map[string]any)
			require.Equal(t, "VALIDATION_ERROR", errBody["code"])
			fields := fieldsOf(res)
			require.Len(t, fields, 1)
			require.Equal(t, tc.wantField, fields[0]["field"], "field must be the JSON name")
			require.Equal(t, tc.wantCode, fields[0]["code"])
			require.NotEmpty(t, fields[0]["message"])
		})
	}

	t.Run("every failing field is reported", func(t *testing.T) {
		var names []string
		for _, f := range fieldsOf(post(`{"email":"nope","items":[{"qty":0}]}`)) {
			names = append(names, f["field"].(string))
		}
		require.ElementsMatch(t, []string{"name", "email", "items[0].qty"}, names)
	})

	other := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"empty body", ``, http.StatusBadRequest, "BAD_REQUEST"},
		{"malformed json", `{"name":`, http.StatusBadRequest, "BAD_REQUEST"},
		{"two json values", `{"name":"Budi"}{"name":"Ani"}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"body over 1 MB", `{"name":"` + strings.Repeat("a", 1<<20) + `"}`, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE"},
	}
	for _, tc := range other {
		t.Run(tc.name, func(t *testing.T) {
			res := post(tc.body)
			require.Equal(t, tc.wantStatus, res.status)
			testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
			require.Equal(t, tc.wantCode, res.body["error"].(map[string]any)["code"])
		})
	}
}

func TestParseListQuery(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: testRoutes})
	get := func(query string) result {
		return call(t, srv.URL, http.MethodGet, "/test/list?"+query, "", nil)
	}
	invalidFields := func(res result) []string {
		var out []string
		for _, f := range res.body["error"].(map[string]any)["errors"].([]any) {
			out = append(out, f.(map[string]any)["field"].(string))
		}
		return out
	}

	t.Run("defaults", func(t *testing.T) {
		res := get("")
		require.Equal(t, http.StatusOK, res.status)
		require.Equal(t, 1.0, res.body["pagination"].(map[string]any)["page"])
		require.Equal(t, 20.0, res.body["pagination"].(map[string]any)["limit"])
	})

	t.Run("limit of exactly 100 is allowed", func(t *testing.T) {
		require.Equal(t, http.StatusOK, get("limit=100").status)
	})

	rejected := []struct {
		name, query string
		fields      []string
	}{
		{"limit above 100", "limit=101", []string{"limit"}},
		{"limit zero", "limit=0", []string{"limit"}},
		{"limit not a number", "limit=many", []string{"limit"}},
		{"page zero", "page=0", []string{"page"}},
		{"date and month together", "date=2026-02-10&month=2026-02", []string{"date", "month"}},
		{"bad date", "date=10-02-2026", []string{"date"}},
		{"bad month", "month=2026-13", []string{"month"}},
		{"bad category", "category_id=abc", []string{"category_id"}},
		{"long search", "q=" + strings.Repeat("a", 101), []string{"q"}},
		{"all problems reported together", "limit=500&page=-1&month=x", []string{"limit", "page", "month"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			res := get(tc.query)
			require.Equal(t, http.StatusUnprocessableEntity, res.status)
			testkit.ValidateSchema(t, "ErrorEnvelope", res.raw)
			require.Equal(t, "VALIDATION_ERROR", res.body["error"].(map[string]any)["code"])
			require.ElementsMatch(t, tc.fields, invalidFields(res))
		})
	}
}

func TestPage(t *testing.T) {
	require.Equal(t, 0, httpx.Page{Page: 1, Limit: 20, Total: 0}.TotalPages())
	require.Equal(t, 1, httpx.Page{Page: 1, Limit: 20, Total: 20}.TotalPages())
	require.Equal(t, 2, httpx.Page{Page: 1, Limit: 20, Total: 21}.TotalPages())
	require.Equal(t, 40, httpx.Page{Page: 3, Limit: 20}.Offset())
}
