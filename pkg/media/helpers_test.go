package media_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/media"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/config"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
	"siapp/pkg/platform/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.NewDatabase(m)) }

// signingKey is the key of every local storage in these tests.
const signingKey = "media-test-signing-key-media-test-signing-key"

// testClock is a clock tests can move forward, to expire a token without waiting for it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now().UTC()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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

// localConfig is the configuration of a local storage in a directory of its own.
func localConfig(dir, baseURL string) config.Storage {
	return config.Storage{
		Driver:          config.StorageLocal,
		LocalDir:        dir,
		LocalSigningKey: signingKey,
		PublicBaseURL:   baseURL,
	}
}

// minioConfig is the MinIO that `make up` runs. The tests fail, not skip, when it is not
// there: the S3 driver is half of what this phase builds.
func minioConfig() config.Storage {
	return config.Storage{
		Driver:      config.StorageMinIO,
		S3Endpoint:  envOr("TEST_S3_ENDPOINT", "http://localhost:9000"),
		S3Region:    "us-east-1",
		S3Bucket:    envOr("TEST_S3_BUCKET", "persist-media"),
		S3AccessKey: envOr("TEST_S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey: envOr("TEST_S3_SECRET_KEY", "minioadmin"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newMinIO returns the S3 driver on the test MinIO.
func newMinIO(t *testing.T, clk clock.Clock) *media.S3Storage {
	t.Helper()
	if testing.Short() {
		t.Skip("-short set, skipping tests that need MinIO")
	}
	s, err := media.NewS3(t.Context(), minioConfig(), clk)
	require.NoError(t, err, "cannot reach MinIO (is `make up` running? or set TEST_S3_ENDPOINT)")
	return s
}

// driver builds a storage for the HTTP flow. baseURL is the address of the test server, which
// the local driver puts in its URLs.
type driver struct {
	name string
	// build returns the storage; root is the local storage's directory, "" for S3.
	build func(t *testing.T, baseURL string, clk clock.Clock) (storage media.Storage, root string)
}

// drivers are the two the flow runs against: local (always) and minio.
var drivers = []driver{
	{name: "local", build: func(t *testing.T, baseURL string, clk clock.Clock) (media.Storage, string) {
		dir := t.TempDir()
		s, err := media.NewLocal(localConfig(dir, baseURL), clk)
		require.NoError(t, err)
		return s, dir
	}},
	{name: "minio", build: func(t *testing.T, _ string, clk clock.Clock) (media.Storage, string) {
		return newMinIO(t, clk), ""
	}},
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// flow is the real media routes behind the real middleware, on the test database, with one
// driver. Two organizations are registered, to prove one cannot see the other.
type flow struct {
	t       *testing.T
	srv     *httptest.Server
	service *media.Service
	storage media.Storage
	clock   *testClock
	logs    *lockedBuffer
	root    string
	orgA    int64
	orgB    int64
	nextUID atomic.Int64
}

func newFlow(t *testing.T, d driver) *flow {
	t.Helper()
	testkit.Reset(t)
	f := &flow{t: t, clock: newTestClock(), logs: &lockedBuffer{}}
	f.srv = testkit.NewServerAt(t, func(baseURL string) testkit.Deps {
		f.storage, f.root = d.build(t, baseURL, f.clock)
		logger := slog.New(slog.NewJSONHandler(f.logs, nil))
		f.service = media.NewService(testkit.Pool(t), f.storage, f.clock, logger)
		idempotency := idem.New(idem.Config{Pool: testkit.Pool(t), Identity: auth.Identity})
		handler := media.NewHandler(f.service, f.storage, idempotency.Middleware(true))
		return testkit.Deps{Logger: logger, Mount: handler.Mount}
	})
	f.orgA, f.orgB = testkit.NewOrg(t), testkit.NewOrg(t)
	return f
}

// person adds a person to an organization and returns their token and personnel id.
func (f *flow) person(org int64) (token string, personnelID int64) {
	f.t.Helper()
	uid := f.nextUID.Add(1) + 1000
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(f.t.Context(),
			"INSERT INTO personnels (personel_user_id, full_name, status) VALUES ($1, 'Petugas Foto', 'ACTIVE') RETURNING id", uid).Scan(&personnelID)
	}))
	return testkit.Token(f.t, org, personnelID), personnelID
}

// row is what the tests read back from the media table.
type row struct {
	ID          int64
	Org         int64
	Owner       int64
	Purpose     string
	ContentType string
	Size        int64
	Key         string
	Status      string
	CapturedAt  *time.Time
	Latitude    *float64
	Longitude   *float64
	Source      string
}

// rows returns the media of an organization, in id order.
func (f *flow) rows(org int64) []row {
	f.t.Helper()
	var out []row
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		rs, err := tx.Query(f.t.Context(), `SELECT id, organization_id, owner_personnel_id, purpose::text, content_type, size_bytes,
			storage_key, status::text, captured_at, capture_latitude, capture_longitude, capture_source::text FROM media ORDER BY id`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rs, func(r pgx.CollectableRow) (row, error) {
			var x row
			err := r.Scan(&x.ID, &x.Org, &x.Owner, &x.Purpose, &x.ContentType, &x.Size, &x.Key, &x.Status, &x.CapturedAt, &x.Latitude, &x.Longitude, &x.Source)
			return x, err
		})
		return err
	}))
	return out
}

// envelope is the success or error envelope, with the data of the caller's choosing.
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

// decode reads a response body as an envelope, checking an error one against the contract.
func decode[T any](t *testing.T, status int, body []byte) envelope[T] {
	t.Helper()
	var e envelope[T]
	require.NoError(t, json.Unmarshal(body, &e), "not an envelope: %s", body)
	if status >= 400 {
		testkit.ValidateSchema(t, "ErrorEnvelope", body)
	}
	return e
}

// uploadInfo is the data of a successful POST /media.
type uploadInfo struct {
	ID            int64             `json:"id"`
	UploadURL     string            `json:"upload_url"`
	UploadMethod  string            `json:"upload_method"`
	UploadHeaders map[string]string `json:"upload_headers"`
	ExpiresAt     time.Time         `json:"expires_at"`
}

func (u uploadInfo) asUpload() media.Upload {
	return media.Upload{URL: u.UploadURL, Method: u.UploadMethod, Headers: u.UploadHeaders, ExpiresAt: u.ExpiresAt}
}

// mediaInfo is the data of a successful complete.
type mediaInfo struct {
	ID            int64    `json:"id"`
	Purpose       string   `json:"purpose"`
	ContentType   string   `json:"content_type"`
	SizeBytes     int64    `json:"size_bytes"`
	Status        string   `json:"status"`
	CaptureSource string   `json:"capture_source"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
}

var keySeq atomic.Int64

// newIdempotencyKey returns a UUID-shaped key that no other call of the run used.
func newIdempotencyKey() string {
	n := keySeq.Add(1)
	return "0193a6f0-7c00-7000-8000-" + strings.Repeat("0", 12-len(strconv.FormatInt(n, 16))) + strconv.FormatInt(n, 16)
}

// validBody is a register request that the contract and the usecase both accept.
func validBody(size int) map[string]any {
	return map[string]any{
		"purpose":        "REPORT",
		"content_type":   "image/jpeg",
		"size_bytes":     size,
		"capture_source": "CAMERA",
	}
}

// api is a contract-checking call (see testkit.Client): the request and the response must both
// match api/openapi.yaml.
func (f *flow) api(token, method, path string, body any, headers map[string]string) (status int, header http.Header, raw []byte) {
	f.t.Helper()
	c := testkit.NewClient(f.t, f.srv)
	c.Token = token
	res := c.DoWithHeaders(method, path, body, headers)
	return res.Status, res.Header, res.Body
}

// register registers a photo through the contract client with a fresh Idempotency-Key.
func (f *flow) register(token string, body map[string]any) (int, envelope[uploadInfo]) {
	f.t.Helper()
	status, _, raw := f.api(token, http.MethodPost, "/media", body, map[string]string{"Idempotency-Key": newIdempotencyKey()})
	return status, decode[uploadInfo](f.t, status, raw)
}

// complete confirms a photo through the contract client.
func (f *flow) complete(token string, id int64) (int, envelope[mediaInfo]) {
	f.t.Helper()
	status, _, raw := f.api(token, http.MethodPost, "/media/"+strconv.FormatInt(id, 10)+"/complete", nil, nil)
	return status, decode[mediaInfo](f.t, status, raw)
}

// raw sends a request that the contract client would refuse to send (a body that breaks the
// request schema on purpose), and returns the answer.
func (f *flow) raw(method, path, token string, body any, headers map[string]string) (status int, header http.Header, out []byte) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(f.t, err)
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+"/api/v1"+path, reader)
	require.NoError(f.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	out, err = io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, res.Header, out
}
