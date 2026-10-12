package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/stretchr/testify/require"

	"siapp/api"
	"siapp/internal/platform/auth"
	"siapp/internal/platform/clock"
)

// Client calls a test server and checks every request and response against
// api/openapi.yaml. An endpoint that is missing from the spec, a response status the spec
// does not list, or a body that does not match its schema fails the test.
type Client struct {
	t      *testing.T
	base   string
	router routers.Router
	token  string
	tokens *auth.Tokens
}

// NewClient returns a Client for srv.
func NewClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(api.OpenAPI)
	require.NoError(t, err, "load api/openapi.yaml")
	require.NoError(t, doc.Validate(loader.Context), "api/openapi.yaml is not a valid OpenAPI document")
	router, err := legacy.NewRouter(doc)
	require.NoError(t, err)
	return &Client{
		t: t, base: srv.URL, router: router,
		tokens: auth.NewTokens([]byte(TestJWTSecret), clock.System{}),
	}
}

// WithToken returns a copy of the client that sends "Authorization: Bearer <token>".
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = token
	return &cp
}

// WithClock returns a copy that judges token expiry by clk. Use it when the server under
// test runs on a fixed clock.
func (c *Client) WithClock(clk clock.Clock) *Client {
	cp := *c
	cp.tokens = auth.NewTokens([]byte(TestJWTSecret), clk)
	return &cp
}

// Login logs a user in through POST /auth/login (checked against the contract like any
// other call) and returns a copy of the client that sends the access token it received.
func (c *Client) Login(identifier, password string) *Client {
	c.t.Helper()
	res := c.Do(http.MethodPost, "/api/v1/auth/login", map[string]any{
		"identifier": identifier,
		"password":   password,
	})
	require.Equal(c.t, http.StatusOK, res.Status, "login failed: %s", res.Body)

	var body struct {
		AccessToken string `json:"accessToken"`
	}
	res.JSON(c.t, &body)
	return c.WithToken(body.AccessToken)
}

// Response is a validated response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into v.
func (r *Response) JSON(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, v), "response body: %s", r.Body)
}

// Do sends method path with body (nil, or a value sent as JSON) and returns the
// response after validating both sides against the contract. The path may carry a query
// string.
func (c *Client) Do(method, path string, body any) *Response {
	c.t.Helper()
	return c.do(method, path, body, true)
}

// DoInvalid is Do without checking the request against the contract, for tests that show
// the server rejects what the contract forbids: a bad body, a missing or expired token. The
// response is still checked, so the server's error replies must match the contract too.
func (c *Client) DoInvalid(method, path string, body any) *Response {
	c.t.Helper()
	return c.do(method, path, body, false)
}

func (c *Client) do(method, path string, body any, validateRequest bool) *Response {
	c.t.Helper()
	ctx := context.Background()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(c.t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	require.NoError(c.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	route, pathParams, err := c.router.FindRoute(req)
	require.NoError(c.t, err, "%s %s is not in api/openapi.yaml", method, path)

	opts := &openapi3filter.Options{AuthenticationFunc: c.checkBearer}
	reqInput := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: pathParams, Route: route, Options: opts,
	}
	if validateRequest {
		require.NoError(c.t, openapi3filter.ValidateRequest(ctx, reqInput), "request violates api/openapi.yaml")
	}

	res, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = res.Body.Close() }()
	respBody, err := io.ReadAll(res.Body)
	require.NoError(c.t, err)

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 res.StatusCode,
		Header:                 res.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	respInput.SetBodyBytes(respBody)
	require.NoError(c.t, openapi3filter.ValidateResponse(ctx, respInput),
		"response %d violates api/openapi.yaml: %s", res.StatusCode, respBody)

	return &Response{Status: res.StatusCode, Header: res.Header, Body: respBody}
}

// checkBearer enforces the contract's bearer security: a request to an operation that is
// not marked `security: []` must carry a valid access token, one that auth.Tokens issued
// and that has not expired. A test that forgets to log in fails here, with a clear message.
func (c *Client) checkBearer(_ context.Context, in *openapi3filter.AuthenticationInput) error {
	header := in.RequestValidationInput.Request.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return errors.New("the operation requires a bearer token; use Client.Login or WithToken")
	}
	if _, err := c.tokens.ParseAccessToken(token); err != nil {
		return fmt.Errorf("invalid access token: %w", err)
	}
	return nil
}
