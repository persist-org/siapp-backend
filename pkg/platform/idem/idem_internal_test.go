package idem

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/testkit"
)

func TestRequestHashIsStableAcrossKeyOrderAndWhitespace(t *testing.T) {
	same := [][2]string{
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`},
		{`{"a":1,"b":2}`, "{\n  \"a\": 1,\n  \"b\": 2\n}"},
		{`{"o":{"x":1,"y":[1,{"p":1,"q":2}]}}`, `{"o":{"y":[1,{"q":2,"p":1}],"x":1}}`},
	}
	for _, pair := range same {
		require.Equal(t, requestHash([]byte(pair[0]), ""), requestHash([]byte(pair[1]), ""), "%s vs %s", pair[0], pair[1])
	}

	different := [][2]string{
		{`{"a":1}`, `{"a":2}`},
		{`{"a":1}`, `{"a":"1"}`},
		{`{"a":[1,2]}`, `{"a":[2,1]}`}, // array order is meaningful
		{`{"a":1}`, `{"a":1,"b":null}`},
		{`{"n":1}`, `{"n":1.0}`}, // numbers are compared as written, not as floats
		{`{"id":9007199254740993}`, `{"id":9007199254740992}`},
	}
	for _, pair := range different {
		require.NotEqual(t, requestHash([]byte(pair[0]), ""), requestHash([]byte(pair[1]), ""), "%s vs %s", pair[0], pair[1])
	}

	t.Run("the query string is part of the request", func(t *testing.T) {
		require.NotEqual(t, requestHash([]byte(`{}`), "a=1"), requestHash([]byte(`{}`), "a=2"))
	})
	t.Run("empty and non-JSON bodies hash as they are", func(t *testing.T) {
		require.Equal(t, requestHash(nil, ""), requestHash([]byte{}, ""))
		require.NotEqual(t, requestHash([]byte("abc"), ""), requestHash([]byte("abd"), ""))
		require.NotEqual(t, requestHash([]byte(`{"a":1}{"b":2}`), ""), requestHash([]byte(`{"a":1}`), ""))
	})
}

func TestCheckOccurredAt(t *testing.T) {
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		at     time.Time
		reason string // "" when accepted
	}{
		{"now", now, ""},
		{"exactly 5 minutes ahead", now.Add(5 * time.Minute), ""},
		{"5 minutes and a second ahead", now.Add(5*time.Minute + time.Second), apperr.ReasonOccurredAtInFuture},
		{"an hour ahead", now.Add(time.Hour), apperr.ReasonOccurredAtInFuture},
		{"exactly 7 days old", now.Add(-7 * 24 * time.Hour), ""},
		{"7 days and a second old", now.Add(-7*24*time.Hour - time.Second), apperr.ReasonOccurredAtTooOld},
		{"a month old", now.AddDate(0, -1, 0), apperr.ReasonOccurredAtTooOld},
		{"yesterday", now.Add(-24 * time.Hour), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOccurredAt(tc.at, now)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			appErr, ok := err.(*apperr.Error)
			require.True(t, ok, "want *apperr.Error, got %v", err)
			require.Equal(t, http.StatusUnprocessableEntity, appErr.Status)
			require.Equal(t, apperr.CodeUnprocessable, appErr.Code)
			require.Equal(t, tc.reason, appErr.Reason)
		})
	}
}

func TestOfflineCreateCheck(t *testing.T) {
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	key := uuid.NewString()
	request := func(header string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/x", nil)
		if header != "" {
			r.Header.Set(HeaderKey, header)
		}
		return r
	}

	require.NoError(t, OfflineCreate{LocalID: key, OccurredAt: now}.Check(request(key), now))
	require.NoError(t, OfflineCreate{LocalID: strings.ToUpper(key), OccurredAt: now}.Check(request(key), now), "letter case does not matter")

	err := OfflineCreate{LocalID: uuid.NewString(), OccurredAt: now}.Check(request(key), now)
	appErr, ok := err.(*apperr.Error)
	require.True(t, ok)
	require.Equal(t, apperr.CodeValidation, appErr.Code)
	require.Equal(t, "local_id", appErr.Fields[0].Field)

	require.Error(t, OfflineCreate{LocalID: key, OccurredAt: now.Add(time.Hour)}.Check(request(key), now))
}

// A key whose first request died is taken over once it is older than InFlightTTL, so a
// crashed process cannot lock a client out of its own request for ever.
func TestStaleInFlightKeyIsTakenOver(t *testing.T) {
	testkit.Reset(t)
	org := testkit.NewOrg(t)
	key := uuid.New()
	body := `{"a":1}`

	_, err := testkit.OwnerPool(t).Exec(t.Context(), `
		INSERT INTO idempotency_keys (organization_id, actor_personnel_id, key, method, path, request_hash, created_at)
		VALUES ($1, 5, $2, 'POST', '/test/idem', $3, now() - interval '1 hour')`,
		org, key, requestHash([]byte(body), ""))
	require.NoError(t, err)

	runs := 0
	svc := New(Config{
		Pool:         testkit.Pool(t),
		Identity:     func(*http.Request) (int64, int64, bool) { return org, 5, true },
		WaitTimeout:  time.Second,
		PollInterval: 10 * time.Millisecond,
		InFlightTTL:  time.Minute,
	})
	srv := testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.With(svc.Middleware(true)).Post("/test/idem", func(w http.ResponseWriter, r *http.Request) {
			runs++
			httpx.OK(w, r, http.StatusCreated, nil)
		})
	}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/test/idem", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set(HeaderKey, key.String())
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()

	require.Equal(t, http.StatusCreated, res.StatusCode)
	require.Equal(t, 1, runs, "the abandoned claim was taken over and the handler ran")
}

func TestReplayedContext(t *testing.T) {
	require.False(t, Replayed(t.Context()))
	require.True(t, Replayed(WithReplayed(t.Context())))
}
