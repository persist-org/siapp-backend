package notifications_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// testClock is a clock tests move by hand, so that an update falls at a time the test chose.
// It starts on a whole second, which is finer than timestamptz's microsecond: a time the test
// holds equals the one the database gives back.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Now().UTC().Truncate(time.Second)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// lockedBuffer is a log sink the server's goroutines write to while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeDispatcher records what Notify hands it, and can be told to fail or panic.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []dispatch
	err   error
	boom  bool
}

type dispatch struct {
	OrganizationID int64
	Sent           []notifications.Notification
}

func (d *fakeDispatcher) Dispatch(_ context.Context, organizationID int64, sent []notifications.Notification) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, dispatch{OrganizationID: organizationID, Sent: sent})
	if d.boom {
		panic("the dispatcher blew up")
	}
	return d.err
}

func (d *fakeDispatcher) got() []dispatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatch(nil), d.calls...)
}

// flow is the real inbox routes behind the real middleware, on the test database. Two
// organizations are registered, to prove one cannot see the other.
type flow struct {
	t          *testing.T
	srv        *httptest.Server
	service    *notifications.Service
	clock      *testClock
	logs       *lockedBuffer
	dispatcher *fakeDispatcher
	orgA       int64
	orgB       int64
	nextUID    atomic.Int64
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	testkit.Reset(t)
	f := &flow{t: t, clock: newTestClock(), logs: &lockedBuffer{}, dispatcher: &fakeDispatcher{}}
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	f.service = notifications.NewService(testkit.Pool(t), f.clock, logger)
	f.service.UseDispatcher(f.dispatcher)
	f.srv = testkit.NewServer(t, testkit.Deps{Logger: logger, Mount: notifications.NewHandler(f.service).Mount})
	f.orgA, f.orgB = testkit.NewOrg(t), testkit.NewOrg(t)
	return f
}

// person adds a person to an organization and returns their token and personnel id.
func (f *flow) person(org int64) (token string, personnelID int64) {
	f.t.Helper()
	uid := f.nextUID.Add(1) + 1000
	f.inTenant(org, func(tx pgx.Tx) {
		require.NoError(f.t, tx.QueryRow(f.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Petugas Inbox', 'ACTIVE') RETURNING id", uid).Scan(&personnelID))
	})
	return testkit.Token(f.t, org, personnelID), personnelID
}

// as is a context like the one a request handler passes to Notify: the caller is a person of an
// organization.
func (f *flow) as(org, personnelID int64) context.Context {
	return auth.WithPrincipal(f.t.Context(), auth.Principal{OrganizationID: org, PersonnelID: personnelID})
}

// notify sends a notification as a person (actor) of org, the way a feature module's use case
// does after its own commit.
func (f *flow) notify(org, actor int64, in notifications.Input) {
	f.t.Helper()
	f.service.Notify(f.as(org, actor), in)
}

func (f *flow) inTenant(org int64, fn func(tx pgx.Tx)) {
	f.t.Helper()
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		fn(tx)
		return nil
	}))
}

// exec runs SQL as the app role inside one organization.
func (f *flow) exec(org int64, sql string, args ...any) {
	f.t.Helper()
	f.inTenant(org, func(tx pgx.Tx) {
		_, err := tx.Exec(f.t.Context(), sql, args...)
		require.NoError(f.t, err)
	})
}

// count returns how many rows of the notifications table an organization has, deleted ones
// included.
func (f *flow) count(org int64) (n int) {
	f.t.Helper()
	f.inTenant(org, func(tx pgx.Tx) {
		require.NoError(f.t, tx.QueryRow(f.t.Context(), "SELECT count(*) FROM notifications").Scan(&n))
	})
	return n
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// view is one notification as the API sends it.
type view struct {
	ID             int64          `json:"id"`
	OrganizationID int64          `json:"organization_id"`
	PersonnelID    int64          `json:"personnel_id"`
	Type           string         `json:"type"`
	Title          string         `json:"title"`
	Body           string         `json:"body"`
	Priority       string         `json:"priority"`
	EntityType     *string        `json:"entity_type"`
	EntityID       *int64         `json:"entity_id"`
	Data           map[string]any `json:"data"`
	IsRead         bool           `json:"is_read"`
	ReadAt         *time.Time     `json:"read_at"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type tombstone struct {
	ID        int64     `json:"id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// listBody is the answer of GET /notifications in either shape.
type listBody struct {
	Success    bool        `json:"success"`
	Data       []view      `json:"data"`
	Deleted    []tombstone `json:"deleted"`
	Pagination *struct {
		Page       int `json:"page"`
		Limit      int `json:"limit"`
		Total      int `json:"total"`
		TotalPages int `json:"total_pages"`
	} `json:"pagination"`
	Meta struct {
		RequestID  string     `json:"request_id"`
		ServerTime *time.Time `json:"server_time"`
		NextCursor string     `json:"next_cursor"`
		HasMore    *bool      `json:"has_more"`
	} `json:"meta"`
}

func (l listBody) ids() []int64 {
	out := make([]int64, len(l.Data))
	for i, v := range l.Data {
		out[i] = v.ID
	}
	return out
}

type count struct {
	Unread int `json:"unread"`
	Total  int `json:"total"`
}

// envelope is a success or error envelope, with the data of the caller's choosing.
type envelope[T any] struct {
	Success bool `json:"success"`
	Data    T    `json:"data"`
	Error   struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
		Errors  []struct {
			Field string `json:"field"`
			Code  string `json:"code"`
		} `json:"errors"`
	} `json:"error"`
}

func (e envelope[T]) reason() string {
	reason, _ := e.Error.Details["reason"].(string)
	return reason
}

func (e envelope[T]) fields() []string {
	out := make([]string, len(e.Error.Errors))
	for i, f := range e.Error.Errors {
		out[i] = f.Field
	}
	return out
}

func decode[T any](t *testing.T, status int, body []byte) envelope[T] {
	t.Helper()
	var e envelope[T]
	require.NoError(t, json.Unmarshal(body, &e), "not an envelope: %s", body)
	if status >= 400 {
		testkit.ValidateSchema(t, "ErrorEnvelope", body)
	}
	return e
}

// api is a contract-checking call (see testkit.Client): the request and the response must both
// match api/openapi.yaml.
func (f *flow) api(token, method, path string) (status int, raw []byte) {
	f.t.Helper()
	c := testkit.NewClient(f.t, f.srv)
	c.Token = token
	res := c.Do(method, path, nil)
	return res.Status, res.Body
}

// raw sends a request that the contract client would refuse to send (a query the contract's
// parameters forbid, or no token), and returns the answer.
func (f *flow) raw(method, path, token string) (status int, out []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+"/api/v1"+path, http.NoBody)
	require.NoError(f.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err = io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, out
}

// list calls GET /notifications through the contract client and expects 200.
func (f *flow) list(token, query string) listBody {
	f.t.Helper()
	path := "/notifications"
	if query != "" {
		path += "?" + query
	}
	status, raw := f.api(token, http.MethodGet, path)
	require.Equal(f.t, http.StatusOK, status, string(raw))
	var out listBody
	require.NoError(f.t, json.Unmarshal(raw, &out), string(raw))
	require.True(f.t, out.Success)
	require.NotEmpty(f.t, out.Meta.RequestID)
	// Both shapes carry the watermark: the contract types the meta as DeltaMeta.
	require.NotNil(f.t, out.Meta.ServerTime, "meta.server_time is mandatory: %s", raw)
	return out
}

func (f *flow) get(token string, id int64) (int, envelope[view]) {
	f.t.Helper()
	status, raw := f.api(token, http.MethodGet, "/notifications/"+itoa(id))
	return status, decode[view](f.t, status, raw)
}

func (f *flow) read(token string, id int64) (int, envelope[view]) {
	f.t.Helper()
	status, raw := f.api(token, http.MethodPost, "/notifications/"+itoa(id)+"/read")
	return status, decode[view](f.t, status, raw)
}

func (f *flow) counter(token string) count {
	f.t.Helper()
	status, raw := f.api(token, http.MethodGet, "/notifications/count")
	require.Equal(f.t, http.StatusOK, status, string(raw))
	return decode[count](f.t, status, raw).Data
}

func (f *flow) readAll(token string) count {
	f.t.Helper()
	status, raw := f.api(token, http.MethodPost, "/notifications/read-all")
	require.Equal(f.t, http.StatusOK, status, string(raw))
	return decode[count](f.t, status, raw).Data
}
