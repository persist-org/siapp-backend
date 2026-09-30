package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

// Client calls a test server and checks every request and response against
// api/openapi.yaml. An endpoint that is missing from the spec, a response status the spec
// does not list, or a body that does not match its schema fails the test.
type Client struct {
	t      *testing.T
	base   string
	router routers.Router
	token  string
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
	return &Client{t: t, base: srv.URL, router: router}
}

// WithToken returns a copy of the client that sends "Authorization: Bearer <token>".
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = token
	return &cp
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

	opts := &openapi3filter.Options{AuthenticationFunc: bearerPresent}
	reqInput := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: pathParams, Route: route, Options: opts,
	}
	require.NoError(c.t, openapi3filter.ValidateRequest(ctx, reqInput), "request violates api/openapi.yaml")

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

// bearerPresent stands in for real authentication: it only checks that the request carries
// a bearer token, so a test for a protected endpoint has to send one.
func bearerPresent(_ context.Context, in *openapi3filter.AuthenticationInput) error {
	if !strings.HasPrefix(in.RequestValidationInput.Request.Header.Get("Authorization"), "Bearer ") {
		return errors.New("missing bearer token")
	}
	return nil
}
