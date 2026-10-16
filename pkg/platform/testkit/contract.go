package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/stretchr/testify/require"

	"siapp/api"
)

// apiPrefix is where the contract's server URL puts every path.
const apiPrefix = "/api/v1"

var contract struct {
	once   sync.Once
	doc    *openapi3.T
	routes []route
	err    error
}

func loadContract(t testing.TB) (*openapi3.T, []route) {
	t.Helper()
	contract.once.Do(func() {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromData(api.OpenAPI)
		if err != nil {
			contract.err = err
			return
		}
		allowNullData(doc)
		contract.doc = doc
		contract.routes = buildRoutes(doc)
	})
	require.NoError(t, contract.err, "load api/openapi.yaml")
	return contract.doc, contract.routes
}

// ValidateSchema checks body against a named schema of the contract, for example
// "ErrorEnvelope". Use it for routes that exist only in a test, so they are not in the
// contract's paths, but whose responses must still have the contract's shape.
func ValidateSchema(t testing.TB, schema string, body []byte) {
	t.Helper()
	doc, _ := loadContract(t)
	ref, ok := doc.Components.Schemas[schema]
	require.True(t, ok, "schema %q is not in the contract", schema)

	var v any
	require.NoError(t, json.Unmarshal(body, &v), "response body is not JSON: %s", body)
	require.NoError(t, ref.Value.VisitJSON(v), "body does not match contract schema %s: %s", schema, body)
}

// Client calls a test server and checks every request and every response against
// api/openapi.yaml, so a handler that drifts from the contract fails the test that
// exercises it. A path or status the contract does not describe is a failure too.
type Client struct {
	t       testing.TB
	baseURL string
	// Token, when set, is sent as a bearer token. Authentication itself is not checked
	// against the contract's security schemes: the middleware has its own tests.
	Token string
}

// NewClient returns a contract-checking client for srv (see NewServer).
func NewClient(t testing.TB, srv *httptest.Server) *Client {
	t.Helper()
	loadContract(t)
	return &Client{t: t, baseURL: srv.URL}
}

// Response is a validated response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into dst.
func (r *Response) JSON(t testing.TB, dst any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.Body, dst), "decode response body: %s", r.Body)
}

// Do sends method path (a contract path such as "/auth/login", without /api/v1). body is
// JSON-encoded when not nil.
func (c *Client) Do(method, path string, body any) *Response {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(c.t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.baseURL+apiPrefix+path, reader)
	require.NoError(c.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	_, routes := loadContract(c.t)
	route, pathParams, ok := findRoute(routes, method, path)
	require.True(c.t, ok, "%s %s%s is not in the contract (api/openapi.yaml)", method, apiPrefix, path)
	opts := &openapi3filter.Options{
		// Security is enforced by the auth middleware and tested there.
		AuthenticationFunc: func(context.Context, *openapi3filter.AuthenticationInput) error { return nil },
	}
	reqInput := &openapi3filter.RequestValidationInput{Request: req, PathParams: pathParams, Route: route, Options: opts}
	require.NoError(c.t, openapi3filter.ValidateRequest(c.t.Context(), reqInput), "request does not match the contract")

	res, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.t, err)

	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 res.StatusCode,
		Header:                 res.Header,
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	respInput.SetBodyBytes(raw)
	require.NoError(c.t, openapi3filter.ValidateResponse(c.t.Context(), respInput),
		"response %d does not match the contract: %s", res.StatusCode, raw)

	return &Response{Status: res.StatusCode, Header: res.Header, Body: raw}
}

// allowNullData makes SuccessEnvelope.data accept null. In OpenAPI 3.1 the contract's
// `data: {}` means "any value", null included, and SiAPP sends "data": null for actions
// with nothing to return (docs/api-response.md). kin-openapi does not read an empty schema
// that way and would reject it, so say it explicitly.
func allowNullData(doc *openapi3.T) {
	if env, ok := doc.Components.Schemas["SuccessEnvelope"]; ok && env.Value != nil {
		if data, ok := env.Value.Properties["data"]; ok && data.Value != nil {
			data.Value.Nullable = true
		}
	}
}

type route struct {
	re     *regexp.Regexp
	names  []string
	params int
	route  *routers.Route
}

var templateParam = regexp.MustCompile(`\{([^}/]+)\}`)

// compilePath turns "/admin/teams/{id}" into a matcher and the list of parameter names.
func compilePath(path string) (*regexp.Regexp, []string) {
	var names []string
	var sb strings.Builder
	sb.WriteString("^")
	last := 0
	for _, loc := range templateParam.FindAllStringSubmatchIndex(path, -1) {
		sb.WriteString(regexp.QuoteMeta(path[last:loc[0]]))
		sb.WriteString("([^/]+)")
		names = append(names, path[loc[2]:loc[3]])
		last = loc[1]
	}
	sb.WriteString(regexp.QuoteMeta(path[last:]) + "$")
	return regexp.MustCompile(sb.String()), names
}

// buildRoutes lists every operation of the contract with a matcher for its path. The
// contract's paths are relative to its first server (https://{host}/api/v1). kin-openapi's
// own gorillamux router was not used: it matches only a fraction of this contract's
// operations, which would let a test pass without validating anything.
func buildRoutes(doc *openapi3.T) []route {
	var out []route
	for path, item := range doc.Paths.Map() {
		re, names := compilePath(path)
		for method, op := range item.Operations() {
			out = append(out, route{re: re, names: names, params: len(names), route: &routers.Route{
				Spec: doc, Path: path, PathItem: item, Method: method, Operation: op,
			}})
		}
	}
	return out
}

// findRoute returns the operation for method and path (relative to /api/v1). When both a
// literal and a templated path match (/teams/export and /teams/{id}), the one with fewer
// parameters wins.
func findRoute(routes []route, method, path string) (*routers.Route, map[string]string, bool) {
	var best *route
	var bestParams map[string]string
	for i := range routes {
		r := &routes[i]
		if r.route.Method != method {
			continue
		}
		m := r.re.FindStringSubmatch(path)
		if m == nil || (best != nil && r.params >= best.params) {
			continue
		}
		best = r
		bestParams = make(map[string]string, len(r.names))
		for j, name := range r.names {
			bestParams[name] = m[j+1]
		}
	}
	if best == nil {
		return nil, nil, false
	}
	return best.route, bestParams, true
}

// ContractPublicOperations returns the operations the contract marks `security: []`, as
// "METHOD /api/v1/path" (the form auth.PublicRoutes uses, with path parameters left as
// {name}). A guard test compares it with auth.PublicRoutes.
func ContractPublicOperations(t testing.TB) map[string]bool {
	t.Helper()
	doc, _ := loadContract(t)
	out := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				out[method+" "+apiPrefix+path] = true
			}
		}
	}
	return out
}
