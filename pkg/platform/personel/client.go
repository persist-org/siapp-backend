package personel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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
	// Personalia's request limits, in characters (kpi-personalia-api
	// internal/modules/integration/dto.go). A value over one makes it refuse the whole
	// request with 422.
	maxUserAgentLen = 512
	maxClientIPLen  = 64
	maxNIKLen       = 100

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

	// Group says which companies form the one organization. See Group.
	Group Group

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
	group   Group
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
		group:   cfg.Group,
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
	// Surrounding spaces (a phone keyboard's autocomplete) are never part of an address,
	// and Personalia would refuse them as malformed.
	email := strings.TrimSpace(in.Email)
	// An address without "@" can never pass Personalia's email check: no round trip.
	// Subtler format errors come back as 422 and are mapped in interpret.
	if email == "" || in.Password == "" || !strings.Contains(email, "@") {
		return Identity{}, ErrInvalidCredentials
	}
	body := map[string]string{
		"email":    email,
		"password": in.Password,
		// Both only feed Personalia's security log, so an over-long value is cut rather
		// than allowed to refuse the login.
		"client_ip":  truncate(in.ClientIP, maxClientIPLen),
		"user_agent": truncate(in.UserAgent, maxUserAgentLen),
	}
	var out identityJSON
	if err := c.call(ctx, "/login", body, false, &out); err != nil {
		return Identity{}, err
	}
	return out.identity(c.group)
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
		// A NIK longer than Personalia accepts cannot exist there; sending it would make
		// it refuse the whole batch with 422. It is simply not found.
		if n != "" && !seen[n] && utf8.RuneCountInString(n) <= maxNIKLen {
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
			id, err := j.identity(c.group)
			if err != nil {
				return nil, err
			}
			// Tenant guard: Personalia's NIKs are global, SiAPP's tenants are not. A person
			// of a company outside the group has no organization (0), so never matches.
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

// invalidFields returns the field names of a validation failure, whose `error` is an
// object of field -> messages. It is nil for any other error.
func (e envelope) invalidFields() []string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(e.Error, &fields) != nil {
		return nil
	}
	return slices.Sorted(maps.Keys(fields))
}

// onlyPersonFields reports whether a login's validation failure names only what the person
// typed: that is their mistake (a malformed email). Any other field is ours: the contract
// drifted.
func onlyPersonFields(fields []string) bool {
	return len(fields) > 0 && !slices.ContainsFunc(fields, func(f string) bool {
		return f != "email" && f != "password"
	})
}

// call POSTs body to path and decodes the envelope's data into out. It returns the typed
// errors of this package; any other failure is wrapped in ErrUnavailable.
func (c *Client) call(ctx context.Context, path string, body any, idempotent bool, out any) error {
	// Encoded before the breaker is asked, so a call that never goes out holds no slot.
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("personel: encode request: %w", err)
	}
	if !c.breaker.allow() {
		return fmt.Errorf("%w: circuit breaker open", ErrUnavailable)
	}

	attempts := 1
	if idempotent {
		attempts = maxAttempts
	}
	var (
		status int
		env    envelope
		last   error
		// outage is set when an attempt failed while the caller was still waiting. An
		// attempt that failed because ctx was cancelled or ran out is the caller giving
		// up, not Personalia failing. The client's own per-attempt timeout
		// (http.Client.Timeout) does not touch ctx, so it still counts.
		outage bool
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		status, env, last = c.send(ctx, path, payload)
		if last == nil && status < 500 {
			break
		}
		// A failure: network error, timeout or 5xx.
		if last == nil {
			last = fmt.Errorf("status %d", status)
		}
		if ctx.Err() != nil {
			c.log.Debug("personel request abandoned by the caller", "path", path, "attempt", attempt, "error", last)
			break
		}
		outage = true
		c.log.Warn("personel request failed", "path", path, "attempt", attempt, "of", attempts, "status", status, "error", last)
		if attempt == attempts {
			break
		}
		if !sleep(ctx, c.backoff<<(attempt-1)) {
			break
		}
	}

	if last != nil || status >= 500 {
		if last == nil {
			last = fmt.Errorf("status %d", status)
		}
		if !outage {
			// Every failed attempt failed because the caller went away (ctx.Err() is set).
			// Counting it would let a few users closing the app during login open the
			// breaker for everyone while Personalia is healthy.
			c.breaker.abandon()
			return fmt.Errorf("%w: %w", ErrUnavailable, ctx.Err())
		}
		c.breaker.done(true)
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
	case status == http.StatusUnprocessableEntity && path == "/login" && onlyPersonFields(env.invalidFields()):
		// What the person typed failed Personalia's validation (a malformed email): to
		// them it is a wrong email, not an outage.
		return ErrInvalidCredentials
	default:
		// 404 means the integration is not mounted (empty key_hashes); 400 and any other
		// 422 mean our request does not match the contract. Neither is the person's fault.
		c.log.Error("personel answered an unexpected status", "path", path, "status", status, "code", code,
			"fields", env.invalidFields())
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

func (j identityJSON) identity(group Group) (Identity, error) {
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
		id.CompanyID, id.CompanyName = j.Company.ID, j.Company.Name
		if orgID, orgName, ok := group.tenant(j.Company.ID, j.Company.Name); ok {
			id.OrganizationID, id.OrganizationName = orgID, orgName
		}
	}
	if j.Client != nil {
		id.ClientID, id.ClientName = j.Client.ID, j.Client.Name
	}
	if j.Position != nil {
		id.PositionID, id.PositionName = j.Position.ID, j.Position.Name
	}
	return id, nil
}

// truncate cuts s to at most n characters, counted as Personalia's max= rule counts them
// (runes; each invalid UTF-8 byte is one).
func truncate(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
