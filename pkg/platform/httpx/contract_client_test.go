package httpx_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

type loginRequest struct {
	Identifier string `json:"identifier" validate:"required"`
	Password   string `json:"password" validate:"required"`
}

// The contract client checks the request and the response of a real contract operation.
// POST /auth/login is stood in by a stub here (the real one arrives in Phase 01.1): the
// point is that the client's checks work, and that the envelopes the stub returns are
// the ones the contract says login answers with.
func TestContractClient(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Post("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
			var req loginRequest
			if err := httpx.Decode(r, &req); err != nil {
				httpx.Error(w, r, err)
				return
			}
			httpx.Error(w, r, apperr.ErrUnauthenticated.WithReason("AUTH_INVALID_CREDENTIALS"))
		})
	}})
	client := testkit.NewClient(t, srv)

	t.Run("a valid request and a documented error pass", func(t *testing.T) {
		res := client.Do(http.MethodPost, "/auth/login", map[string]any{"identifier": "user01", "password": "x"})
		require.Equal(t, http.StatusUnauthorized, res.Status)
	})

	t.Run("a validation error matches the contract's 422", func(t *testing.T) {
		res := client.Do(http.MethodPost, "/auth/login", map[string]any{"identifier": "user01", "password": ""})
		require.Equal(t, http.StatusUnprocessableEntity, res.Status)
	})
}

// A client that silently accepted anything would prove nothing, so check that it fails
// when it should. These run the failing call against a recording TB.
func TestContractClientRejectsDrift(t *testing.T) {
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.Post("/api/v1/auth/login", func(w http.ResponseWriter, _ *http.Request) {
			// Wrong on purpose: login documents no 418 and a bare object is no envelope.
			httpx.JSON(w, http.StatusTeapot, map[string]string{"nope": "x"})
		})
	}})

	rec := &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		testkit.NewClient(rec, srv).Do(http.MethodPost, "/auth/login", map[string]any{"identifier": "a", "password": "b"})
	}()
	require.True(t, rec.failed, "an undocumented status must fail the test")

	rec = &recordingTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		testkit.NewClient(rec, srv).Do(http.MethodGet, "/no/such/route", nil)
	}()
	require.True(t, rec.failed, "a path outside the contract must fail the test")
}

// recordingTB records a failure instead of failing the real test. FailNow panics, like
// runtime.Goexit would, so the code under test stops where testify stops it.
type recordingTB struct {
	testing.TB
	failed bool
}

func (r *recordingTB) Errorf(string, ...any) { r.failed = true }
func (r *recordingTB) Fatalf(string, ...any) { r.failed = true; panic("recordingTB: Fatalf") }
func (r *recordingTB) FailNow()              { r.failed = true; panic("recordingTB: FailNow") }
