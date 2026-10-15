package idem_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/httpx"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// env is a test server with two idempotent test routes. The handler counts its runs, and
// X-Test-Behavior says how it answers.
type env struct {
	srv  string
	runs atomic.Int32
}

func newEnv(t *testing.T, cfg func(*idem.Config)) *env {
	t.Helper()
	testkit.Reset(t)
	e := &env{}
	c := idem.Config{
		Pool: testkit.Pool(t),
		Identity: func(r *http.Request) (int64, int64, bool) {
			org, err1 := strconv.ParseInt(r.Header.Get("X-Test-Org"), 10, 64)
			actor, err2 := strconv.ParseInt(r.Header.Get("X-Test-Actor"), 10, 64)
			return org, actor, err1 == nil && err2 == nil && org > 0
		},
		WaitTimeout:  3 * time.Second,
		PollInterval: 10 * time.Millisecond,
	}
	if cfg != nil {
		cfg(&c)
	}
	svc := idem.New(c)

	handler := func(w http.ResponseWriter, r *http.Request) {
		n := e.runs.Add(1)
		switch r.Header.Get("X-Test-Behavior") {
		case "500":
			httpx.Error(w, r, errors.New("database exploded"))
		case "409":
			httpx.Error(w, r, apperr.ErrConflict)
		case "429":
			httpx.Error(w, r, apperr.ErrRateLimited)
		case "422":
			httpx.Error(w, r, apperr.ErrUnprocessable)
		case "slow":
			time.Sleep(400 * time.Millisecond)
			httpx.OK(w, r, http.StatusCreated, map[string]any{"run": n})
		case "panic":
			panic("handler blew up")
		case "text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("not json"))
		case "text-400":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("not json"))
		case "big":
			httpx.OK(w, r, http.StatusCreated, map[string]any{"run": n, "pad": strings.Repeat("x", 300<<10)})
		default:
			httpx.OK(w, r, http.StatusCreated, map[string]any{"run": n})
		}
	}
	e.srv = testkit.NewServer(t, testkit.Deps{Mount: func(r chi.Router) {
		r.With(svc.Middleware(true)).Post("/test/idem", handler)
		r.With(svc.Middleware(false)).Post("/test/optional", handler)
	}}).URL
	return e
}

type reply struct {
	status int
	header http.Header
	body   string
}

type call struct {
	path     string
	key      string
	body     string
	org      int64
	actor    int64
	behavior string
}

func (e *env) do(t *testing.T, c call) reply {
	t.Helper()
	if c.path == "" {
		c.path = "/test/idem"
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.srv+c.path, bytes.NewReader([]byte(c.body)))
	require.NoError(t, err)
	if c.key != "" {
		req.Header.Set(idem.HeaderKey, c.key)
	}
	req.Header.Set("X-Test-Org", strconv.FormatInt(c.org, 10))
	req.Header.Set("X-Test-Actor", strconv.FormatInt(c.actor, 10))
	req.Header.Set("X-Test-Behavior", c.behavior)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return reply{status: res.StatusCode, header: res.Header, body: string(raw)}
}

func newKey() string { return uuid.NewString() }

func TestFirstCallRunsAndReplayDoesNot(t *testing.T) {
	e := newEnv(t, nil)
	org, key := testkit.NewOrg(t), newKey()
	c := call{key: key, body: `{"a":1}`, org: org, actor: 5}

	first := e.do(t, c)
	require.Equal(t, http.StatusCreated, first.status)
	require.Empty(t, first.header.Get(idem.HeaderReplayed))
	require.JSONEq(t, `{"run":1}`, mustField(t, first.body, "data"))

	for range 3 {
		again := e.do(t, c)
		require.Equal(t, http.StatusCreated, again.status)
		require.Equal(t, "true", again.header.Get(idem.HeaderReplayed))
		require.JSONEq(t, first.body, again.body, "a replay returns the stored response")
	}
	require.EqualValues(t, 1, e.runs.Load(), "the handler ran once")
}

func TestReplayIgnoresJSONKeyOrderAndWhitespace(t *testing.T) {
	e := newEnv(t, nil)
	org, key := testkit.NewOrg(t), newKey()

	e.do(t, call{key: key, body: `{"a":1,"b":[1,2],"c":{"x":1,"y":2}}`, org: org, actor: 5})
	again := e.do(t, call{key: key, body: "{ \"c\": {\"y\":2, \"x\":1},\n \"b\":[1,2], \"a\":1 }", org: org, actor: 5})

	require.Equal(t, "true", again.header.Get(idem.HeaderReplayed))
	require.EqualValues(t, 1, e.runs.Load())
}

func TestSameKeyDifferentRequestIsAConflict(t *testing.T) {
	e := newEnv(t, nil)
	org, key := testkit.NewOrg(t), newKey()
	e.do(t, call{key: key, body: `{"a":1}`, org: org, actor: 5})

	for name, c := range map[string]call{
		"another body": {key: key, body: `{"a":2}`, org: org, actor: 5},
		"another path": {path: "/test/optional", key: key, body: `{"a":1}`, org: org, actor: 5},
	} {
		t.Run(name, func(t *testing.T) {
			res := e.do(t, c)
			require.Equal(t, http.StatusConflict, res.status)
			testkit.ValidateSchema(t, "ErrorEnvelope", []byte(res.body))
			require.Contains(t, res.body, `"CONFLICT"`)
			require.Contains(t, res.body, `"IDEMPOTENCY_KEY_REUSED"`)
			require.Empty(t, res.header.Get(idem.HeaderReplayed))
		})
	}
	require.EqualValues(t, 1, e.runs.Load())
}

func TestWhichResponsesAreStored(t *testing.T) {
	tests := []struct {
		behavior   string
		wantStatus int
		stored     bool
	}{
		{"422", http.StatusUnprocessableEntity, true}, // the client would get the same answer again
		{"500", http.StatusInternalServerError, false},
		{"409", http.StatusConflict, false},
		{"429", http.StatusTooManyRequests, false},
		{"text-400", http.StatusBadRequest, false}, // not JSON, and a 4xx changed nothing
	}
	for _, tc := range tests {
		t.Run(tc.behavior, func(t *testing.T) {
			e := newEnv(t, nil)
			org, key := testkit.NewOrg(t), newKey()
			c := call{key: key, body: `{}`, org: org, actor: 5, behavior: tc.behavior}

			first := e.do(t, c)
			require.Equal(t, tc.wantStatus, first.status)
			second := e.do(t, c)
			require.Equal(t, tc.wantStatus, second.status)

			if tc.stored {
				require.Equal(t, "true", second.header.Get(idem.HeaderReplayed))
				require.EqualValues(t, 1, e.runs.Load())
			} else {
				require.Empty(t, second.header.Get(idem.HeaderReplayed))
				require.EqualValues(t, 2, e.runs.Load(), "a transient failure must be retried for real")
			}
		})
	}

	t.Run("a retry after a 500 can succeed and is then stored", func(t *testing.T) {
		e := newEnv(t, nil)
		org, key := testkit.NewOrg(t), newKey()
		require.Equal(t, http.StatusInternalServerError, e.do(t, call{key: key, body: `{}`, org: org, actor: 5, behavior: "500"}).status)
		require.Equal(t, http.StatusCreated, e.do(t, call{key: key, body: `{}`, org: org, actor: 5}).status)
		replay := e.do(t, call{key: key, body: `{}`, org: org, actor: 5})
		require.Equal(t, "true", replay.header.Get(idem.HeaderReplayed))
		require.EqualValues(t, 2, e.runs.Load())
	})

	t.Run("a panic releases the key", func(t *testing.T) {
		e := newEnv(t, nil)
		org, key := testkit.NewOrg(t), newKey()
		require.Equal(t, http.StatusInternalServerError, e.do(t, call{key: key, body: `{}`, org: org, actor: 5, behavior: "panic"}).status)
		require.Equal(t, http.StatusCreated, e.do(t, call{key: key, body: `{}`, org: org, actor: 5}).status)
		require.EqualValues(t, 2, e.runs.Load())
	})
}

// A 2xx whose body cannot be kept (not JSON for the jsonb column, or too large) still did
// its work: the retry must not run the handler again, and says why there is no response.
func TestSuccessThatCannotBeStoredIsNotRunAgain(t *testing.T) {
	for _, tc := range []struct {
		behavior   string
		wantStatus int
	}{
		{"text", http.StatusOK},
		{"big", http.StatusCreated},
	} {
		t.Run(tc.behavior, func(t *testing.T) {
			e := newEnv(t, nil)
			org, key := testkit.NewOrg(t), newKey()
			c := call{key: key, body: `{}`, org: org, actor: 5, behavior: tc.behavior}

			first := e.do(t, c)
			require.Equal(t, tc.wantStatus, first.status, "the first caller gets the real response")
			require.Empty(t, first.header.Get(idem.HeaderReplayed))

			for range 2 {
				again := e.do(t, c)
				require.Equal(t, http.StatusConflict, again.status)
				testkit.ValidateSchema(t, "ErrorEnvelope", []byte(again.body))
				require.Contains(t, again.body, `"IDEMPOTENCY_RESPONSE_NOT_STORED"`)
				require.Equal(t, "true", again.header.Get(idem.HeaderReplayed))
			}
			require.EqualValues(t, 1, e.runs.Load(), "the handler's side effects must not run twice")
		})
	}
}

func TestKeyBelongsToOrganizationAndActor(t *testing.T) {
	e := newEnv(t, nil)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	key := newKey()

	a := e.do(t, call{key: key, body: `{}`, org: orgA, actor: 5})
	b := e.do(t, call{key: key, body: `{}`, org: orgB, actor: 5})
	otherActor := e.do(t, call{key: key, body: `{}`, org: orgA, actor: 6})

	for _, res := range []reply{a, b, otherActor} {
		require.Equal(t, http.StatusCreated, res.status)
		require.Empty(t, res.header.Get(idem.HeaderReplayed), "the same key elsewhere is a different key")
	}
	require.EqualValues(t, 3, e.runs.Load())
}

func TestKeyValidation(t *testing.T) {
	e := newEnv(t, nil)
	org := testkit.NewOrg(t)

	t.Run("missing key on a required route", func(t *testing.T) {
		res := e.do(t, call{body: `{}`, org: org, actor: 5})
		require.Equal(t, http.StatusUnprocessableEntity, res.status)
		testkit.ValidateSchema(t, "ErrorEnvelope", []byte(res.body))
		require.Contains(t, res.body, `"VALIDATION_ERROR"`)
		require.Contains(t, res.body, `"Idempotency-Key"`)
	})
	t.Run("key that is not a UUID", func(t *testing.T) {
		res := e.do(t, call{key: "not-a-uuid", body: `{}`, org: org, actor: 5})
		require.Equal(t, http.StatusUnprocessableEntity, res.status)
		require.Contains(t, res.body, `"INVALID"`)
	})
	t.Run("an optional route runs without a key, unprotected", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, e.do(t, call{path: "/test/optional", body: `{}`, org: org, actor: 5}).status)
		require.Equal(t, http.StatusCreated, e.do(t, call{path: "/test/optional", body: `{}`, org: org, actor: 5}).status)
		require.EqualValues(t, 2, e.runs.Load())
	})
	t.Run("no identity is 401", func(t *testing.T) {
		res := e.do(t, call{key: newKey(), body: `{}`})
		require.Equal(t, http.StatusUnauthorized, res.status)
	})
}

func TestParallelFirstCallsRunTheHandlerOnce(t *testing.T) {
	e := newEnv(t, nil)
	org, key := testkit.NewOrg(t), newKey()
	c := call{key: key, body: `{"a":1}`, org: org, actor: 5, behavior: "slow"}

	const n = 4
	replies := make([]reply, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replies[i] = e.do(t, c)
		}()
	}
	wg.Wait()

	require.EqualValues(t, 1, e.runs.Load(), "the handler must run once for parallel first calls")
	replayed := 0
	for _, res := range replies {
		require.Equal(t, http.StatusCreated, res.status)
		require.JSONEq(t, replies[0].body, res.body)
		if res.header.Get(idem.HeaderReplayed) == "true" {
			replayed++
		}
	}
	require.Equal(t, n-1, replayed, "everyone but the winner got a replay")
}

func TestStillInFlightAfterWaitTimeoutIs409(t *testing.T) {
	e := newEnv(t, func(c *idem.Config) { c.WaitTimeout = 100 * time.Millisecond })
	org, key := testkit.NewOrg(t), newKey()
	c := call{key: key, body: `{}`, org: org, actor: 5, behavior: "slow"}

	var first reply
	done := make(chan struct{})
	go func() { defer close(done); first = e.do(t, c) }()
	time.Sleep(100 * time.Millisecond) // let the first request claim the key

	second := e.do(t, c)
	require.Equal(t, http.StatusConflict, second.status)
	testkit.ValidateSchema(t, "ErrorEnvelope", []byte(second.body))
	require.Contains(t, second.body, `"IDEMPOTENCY_KEY_IN_FLIGHT"`)

	<-done
	require.Equal(t, http.StatusCreated, first.status)
	require.EqualValues(t, 1, e.runs.Load())
}

func TestCleanupDeletesOnlyExpiredKeysPerOrganization(t *testing.T) {
	testkit.Reset(t)
	orgA, orgB := testkit.NewOrg(t), testkit.NewOrg(t)
	owner := testkit.OwnerPool(t)

	insert := func(org int64, age string) {
		_, err := owner.Exec(t.Context(), `
			INSERT INTO idempotency_keys (organization_id, actor_personnel_id, key, method, path, request_hash, status_code, created_at)
			VALUES ($1, 5, $2, 'POST', '/x', '\x00', 201, now() - $3::interval)`, org, uuid.New(), age)
		require.NoError(t, err)
	}
	insert(orgA, "9 days")
	insert(orgA, "1 day")
	insert(orgB, "30 days")
	insert(orgB, "7 days")

	deleted, err := idem.Cleanup(t.Context(), testkit.Pool(t))
	require.NoError(t, err)
	require.EqualValues(t, 2, deleted)

	var left int
	require.NoError(t, owner.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&left))
	require.Equal(t, 2, left, "the 1-day and the 7-day keys stay: the offline window is 7 days")
}

func mustField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	return string(m[field])
}
