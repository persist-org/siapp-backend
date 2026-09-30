package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/platform/apperr"
)

type echoRequest struct {
	Name  string `json:"name" validate:"required,min=3"`
	Email string `json:"email" validate:"omitempty,email"`
	Items []struct {
		Qty int `json:"qty" validate:"min=1"`
	} `json:"items" validate:"dive"`
}

// testRoutes is the test-only route the plan asks for: it decodes into echoRequest so the
// tests can see the real VALIDATION_FAILED shape.
func testRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/test/echo", func(w http.ResponseWriter, r *http.Request) {
		var req echoRequest
		if err := Decode(r, &req); err != nil {
			Error(w, r, err)
			return
		}
		JSON(w, http.StatusOK, req)
	})
	mux.HandleFunc("/test/boom", func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, errors.New("pq: password authentication failed for user root"))
	})
	mux.HandleFunc("/test/conflict", func(w http.ResponseWriter, r *http.Request) {
		Error(w, r, apperr.ErrConflict.WithDetails(map[string]string{"slot": "taken"}))
	})
}

type errorResponse struct {
	Error struct {
		Code    string       `json:"code"`
		Message string       `json:"message"`
		Details []FieldError `json:"details"`
	} `json:"error"`
}

func post(t *testing.T, url, body string) (*http.Response, errorResponse) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var parsed errorResponse
	_ = json.Unmarshal(raw, &parsed)
	return res, parsed
}

func TestDecode(t *testing.T) {
	srv := newTestServer(t, io.Discard, testRoutes)
	url := srv.URL + "/test/echo"

	t.Run("valid body", func(t *testing.T) {
		res, _ := post(t, url, `{"name":"Budi","items":[{"qty":2}]}`)
		require.Equal(t, http.StatusOK, res.StatusCode)
	})

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantField  string // "" when the failure has no per-field details
		wantMsg    string
	}{
		{"missing required field", `{}`, 400, "name", "wajib diisi"},
		{"too short", `{"name":"ab"}`, 400, "name", "minimum 3"},
		{"bad email", `{"name":"Budi","email":"nope"}`, 400, "email", "email"},
		{"nested field uses json path", `{"name":"Budi","items":[{"qty":0}]}`, 400, "items[0].qty", "minimum 1"},
		{"unknown field is rejected", `{"name":"Budi","nickname":"x"}`, 400, "nickname", "tidak dikenal"},
		{"wrong type", `{"name":123}`, 400, "name", "teks"},
		{"empty body", ``, 400, "", "wajib diisi"},
		{"malformed json", `{"name":`, 400, "", "bukan JSON"},
		{"two json values", `{"name":"Budi"}{"name":"Ani"}`, 400, "", "satu objek"},
		{"body over 1 MB", `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 413, "", "terlalu besar"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, body := post(t, url, tc.body)
			require.Equal(t, tc.wantStatus, res.StatusCode)
			require.Equal(t, apperr.CodeValidationFailed, body.Error.Code)
			require.NotEmpty(t, body.Error.Message)

			if tc.wantField == "" {
				require.Contains(t, body.Error.Message, tc.wantMsg)
				return
			}
			require.Len(t, body.Error.Details, 1)
			require.Equal(t, tc.wantField, body.Error.Details[0].Field)
			require.Contains(t, strings.Join(body.Error.Details[0].Messages, " "), tc.wantMsg)
		})
	}

	t.Run("all failing fields are reported", func(t *testing.T) {
		_, body := post(t, url, `{"email":"nope","items":[{"qty":0}]}`)
		fields := make([]string, 0, len(body.Error.Details))
		for _, d := range body.Error.Details {
			fields = append(fields, d.Field)
		}
		require.ElementsMatch(t, []string{"name", "email", "items[0].qty"}, fields)
	})
}

func TestError(t *testing.T) {
	var logs strings.Builder
	srv := newTestServer(t, &logs, testRoutes)

	t.Run("app error keeps status, code and details", func(t *testing.T) {
		res := get(t, srv.URL+"/test/conflict", nil)
		require.Equal(t, http.StatusConflict, res.StatusCode)
		var body struct {
			Error struct {
				Code    string            `json:"code"`
				Details map[string]string `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		require.Equal(t, apperr.CodeConflict, body.Error.Code)
		require.Equal(t, map[string]string{"slot": "taken"}, body.Error.Details)
	})

	t.Run("unknown error becomes INTERNAL_ERROR and is not exposed", func(t *testing.T) {
		res := get(t, srv.URL+"/test/boom", http.Header{HeaderRequestID: {"req-boom"}})
		require.Equal(t, http.StatusInternalServerError, res.StatusCode)
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.Contains(t, string(raw), apperr.CodeInternal)
		require.NotContains(t, string(raw), "password")
	})

	t.Run("shared errors are not mutated by WithDetails", func(t *testing.T) {
		_ = apperr.ErrConflict.WithDetails("x")
		require.Nil(t, apperr.ErrConflict.Details)
	})
}

func TestNotFoundUsesErrorShape(t *testing.T) {
	srv := newTestServer(t, io.Discard, nil)
	res := get(t, srv.URL+"/api/v1/nope", nil)
	require.Equal(t, http.StatusNotFound, res.StatusCode)

	var body errorResponse
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, apperr.CodeNotFound, body.Error.Code)
}
