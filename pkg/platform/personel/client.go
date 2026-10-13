package personel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"siapp/pkg/platform/clock"
)

const (
	// personelTimeout bounds one HTTP attempt. Personalia answers in milliseconds when it
	// is healthy; a login must not hang a phone for longer.
	personelTimeout = 5 * time.Second
	// maxAttempts is for idempotent calls only. Auth.Login always makes exactly one.
	maxAttempts = 3
	// retryBackoff is the wait before the second attempt; it doubles for the third.
	retryBackoff = 150 * time.Millisecond
	// employeesBatch is the most NIKs Personalia accepts in one request.
	employeesBatch = 200
	// maxResponseBytes caps what is read from Personalia: 200 identities are ~100 KB.
	maxResponseBytes = 4 << 20

	headerIntegrationKey = "X-Integration-Key"
)

// Config configures Client. BaseURL and Key are required.
type Config struct {
	// BaseURL is the integration root, e.g. http://localhost:8081/api/v3/integrations/siapp
	BaseURL string
	// Key is the shared integration key, sent as X-Integration-Key. Personalia stores only
	// its SHA-256. A shared key and not OAuth (D8): Personalia has no OAuth server, only
	// this backend calls these routes, and the key never leaves the two servers.
	Key string

	Logger *slog.Logger
	// Clock drives the circuit breaker; nil is the system clock.
	Clock clock.Clock
	// HTTPClient is for tests; nil makes one with personelTimeout.
	HTTPClient *http.Client
	// Backoff overrides retryBackoff (tests set it to 0).
	Backoff *time.Duration
}

// Client is the HTTP implementation of Auth and Directory against kpi-personalia-api.
// It is safe for concurrent use.
type Client struct {
	base    string
	key     string
	http    *http.Client
	log     *slog.Logger
	breaker *breaker
	backoff time.Duration
}

var (
	_ Auth      = (*Client)(nil)
	_ Directory = (*Client)(nil)
)

// New builds a Client. It fails on a missing or malformed base URL or key, so a bad
// deployment stops at startup.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("personel: BaseURL must be an http(s) URL")
	}
	if cfg.Key == "" {
		return nil, errors.New("personel: integration key is empty")
	}
	c := &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		key:     cfg.Key,
		http:    cfg.HTTPClient,
		log:     cfg.Logger,
		backoff: retryBackoff,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: personelTimeout}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if cfg.Backoff != nil {
		c.backoff = *cfg.Backoff
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System{}
	}
	c.breaker = newBreaker(clk)
	return c, nil
}

// Login implements Auth. One request, never retried: a retry is a second failed attempt
// against the account.
func (c *Client) Login(ctx context.Context, in LoginInput) (Identity, error) {
	if in.Email == "" || in.Password == "" {
		return Identity{}, ErrInvalidCredentials
	}
	body := map[string]string{
		"email":      in.Email,
		"password":   in.Password,
		"client_ip":  in.ClientIP,
		"user_agent": in.UserAgent,
	}
	var out identityJSON
	if err := c.call(ctx, "/login", body, false, &out); err != nil {
		return Identity{}, err
	}
	return out.identity()
}

// EmployeeByNIK implements Directory.
func (c *Client) EmployeeByNIK(ctx context.Context, orgID int64, nik string) (Identity, error) {
	found, err := c.EmployeesByNIKs(ctx, orgID, []string{nik})
	if err != nil {
		return Identity{}, err
	}
	if len(found) == 0 {
		return Identity{}, ErrNotFound
	}
	return found[0], nil
}

// EmployeesByNIKs implements Directory. It splits into batches of 200 and retries each
// batch: the lookup changes nothing.
func (c *Client) EmployeesByNIKs(ctx context.Context, orgID int64, niks []string) ([]Identity, error) {
	seen := make(map[string]bool, len(niks))
	var unique []string
	for _, n := range niks {
		if n != "" && !seen[n] {
			seen[n] = true
			unique = append(unique, n)
		}
	}
	result := []Identity{}
	for start := 0; start < len(unique); start += employeesBatch {
		end := min(start+employeesBatch, len(unique))
		var out []identityJSON
		if err := c.call(ctx, "/employees", map[string][]string{"niks": unique[start:end]}, true, &out); err != nil {
			return nil, err
		}
		for _, j := range out {
			id, err := j.identity()
			if err != nil {
				return nil, err
			}
			// Tenant guard: Personalia's NIKs are global, SiAPP's tenants are not.
			if id.OrganizationID == orgID {
				result = append(result, id)
			}
		}
	}
	return result, nil
}

// envelope is kpi-personalia-api's response shape. `error` is a string code for the
// integration's own failures and an object for validation failures.
type envelope struct {
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

func (e envelope) code() string {
	var s string
	if json.Unmarshal(e.Error, &s) == nil {
		return s
	}
	return ""
}

// call POSTs body to path and decodes the envelope's data into out. It returns the typed
// errors of this package; any other failure is wrapped in ErrUnavailable.
func (c *Client) call(ctx context.Context, path string, body any, idempotent bool, out any) error {
	if !c.breaker.allow() {
		return fmt.Errorf("%w: circuit breaker open", ErrUnavailable)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		c.breaker.done(false)
		return fmt.Errorf("personel: encode request: %w", err)
	}

	attempts := 1
	if idempotent {
		attempts = maxAttempts
	}
	var (
		status int
		env    envelope
		last   error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		status, env, last = c.send(ctx, path, payload)
		if last == nil && status < 500 {
			break
		}
		// An outage: network error, timeout or 5xx.
		if last == nil {
			last = fmt.Errorf("status %d", status)
		}
		c.log.Warn("personel request failed", "path", path, "attempt", attempt, "of", attempts, "status", status, "error", last)
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		if !sleep(ctx, c.backoff<<(attempt-1)) {
			break
		}
	}

	if last != nil || status >= 500 {
		c.breaker.done(true)
		if last == nil {
			last = fmt.Errorf("status %d", status)
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, last)
	}
	// Personalia answered: whatever the status, it is up.
	c.breaker.done(false)
	return c.interpret(path, status, env, out)
}

// send makes one HTTP attempt. err is set for a transport failure or an unreadable body;
// status is the HTTP status otherwise.
func (c *Client) send(ctx context.Context, path string, payload []byte) (int, envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(headerIntegrationKey, c.key)

	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the URL; it holds no secret (the key is a header).
		return 0, envelope{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, envelope{}, err
	}
	var env envelope
	// A gateway's HTML 502 is not JSON: that is fine for a 5xx, it is judged by status.
	if err := json.Unmarshal(raw, &env); err != nil && resp.StatusCode < 500 {
		return resp.StatusCode, envelope{}, fmt.Errorf("undecodable response (status %d)", resp.StatusCode)
	}
	return resp.StatusCode, env, nil
}

// interpret maps a non-outage answer to data or a typed error.
func (c *Client) interpret(path string, status int, env envelope, out any) error {
	code := env.code()
	switch {
	case status >= 200 && status < 300:
		if err := json.Unmarshal(env.Data, out); err != nil {
			c.log.Error("personel response does not match the contract", "path", path, "error", err)
			return fmt.Errorf("%w: undecodable data", ErrUnavailable)
		}
		return nil
	case status == http.StatusUnauthorized && code == "invalid_credentials":
		return ErrInvalidCredentials
	case status == http.StatusUnauthorized:
		// The integration key was rejected: a configuration error on our side, and it
		// must never look like a wrong password.
		c.log.Error("personel rejected the integration key: check PERSONEL_INTEGRATION_KEY against integrations.siapp.key_hashes", "path", path)
		return fmt.Errorf("%w: integration key rejected", ErrUnavailable)
	case status == http.StatusForbidden && (code == DisabledInactive || code == DisabledWorkMissing):
		return &DisabledError{Reason: code}
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	default:
		// 404 means the integration is not mounted (empty key_hashes); 400/422 mean our
		// request does not match the contract. Neither is the person's fault.
		c.log.Error("personel answered an unexpected status", "path", path, "status", status, "code", code)
		return fmt.Errorf("%w: unexpected status %d", ErrUnavailable, status)
	}
}

// sleep waits d or until ctx is done; it reports whether the wait completed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// identityJSON is the Identity JSON of kpi-personalia-api (docs/plan/00-platform.md,
// Phase 00.5). Nullable fields are pointers.
type identityJSON struct {
	UserID   int64   `json:"user_id"`
	Email    string  `json:"email"`
	NIK      *string `json:"nik"`
	FullName string  `json:"full_name"`
	Phone    *string `json:"phone"`
	PhotoURL *string `json:"photo_url"`
	Company  *ref    `json:"company"`
	Client   *ref    `json:"client"`
	Position *ref    `json:"position"`
	Active   bool    `json:"active"`
	Domain   Domain  `json:"domain"`
}

type ref struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (j identityJSON) identity() (Identity, error) {
	if !j.Domain.Valid() {
		return Identity{}, fmt.Errorf("%w: unknown account domain %q", ErrUnavailable, j.Domain)
	}
	id := Identity{
		PersonelUserID: j.UserID,
		Email:          j.Email,
		NIK:            deref(j.NIK),
		FullName:       j.FullName,
		Phone:          deref(j.Phone),
		PhotoURL:       deref(j.PhotoURL),
		Active:         j.Active,
		Domain:         j.Domain,
	}
	if j.Company != nil {
		id.OrganizationID, id.OrganizationName = j.Company.ID, j.Company.Name
	}
	if j.Client != nil {
		id.ClientID, id.ClientName = j.Client.ID, j.Client.Name
	}
	if j.Position != nil {
		id.PositionID, id.PositionName = j.Position.ID, j.Position.Name
	}
	return id, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
