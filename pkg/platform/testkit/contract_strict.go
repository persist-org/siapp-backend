package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// ContractReportEnv names a directory. When it is set, StrictResponses records what it finds
// there (one file per test binary) instead of failing the test: the report mode used to measure
// the drift between the API and api/openapi.yaml.
const ContractReportEnv = "SIAPP_CONTRACT_REPORT"

// StrictResponses wraps the handler of a test server so that every JSON response to an operation
// of the contract is checked against the body the contract documents for it, strictly:
//
//   - every property the contract lists is present: a value that is not there is sent as null,
//     never left out;
//   - null is sent only where the contract allows it (type [..., "null"]);
//   - no property is sent that the contract does not list;
//   - a status the operation does not document is a finding too.
//
// It is what keeps api/openapi.yaml one to one with what the API really does. The findings of a
// server fail the test that made it, when it ends.
func StrictResponses(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	doc, routes := loadContract(t)
	var mu sync.Mutex
	var found []finding
	seen := map[observation]int{}
	report := os.Getenv(ContractReportEnv)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if report != "" {
			writeReport(report, found, seen)
			return
		}
		if len(found) == 0 {
			return
		}
		t.Errorf("responses do not match api/openapi.yaml:\n%s", summarize(found))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A websocket or a stream is not a JSON body to check.
		if r.Header.Get("Upgrade") != "" || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			h.ServeHTTP(w, r)
			return
		}
		rec := &teeWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)

		path := strings.TrimPrefix(r.URL.Path, apiPrefix)
		if path == r.URL.Path {
			return // not an API route (/health)
		}
		route, _, ok := findRoute(routes, r.Method, path)
		if !ok {
			return // a route the contract does not have is the router tests' business
		}
		fs, obs := checkResponse(doc, route.Method+" "+route.Path, route.Operation, rec.status, rec.Header().Get("Content-Type"), rec.body.Bytes())
		mu.Lock()
		found = append(found, fs...)
		if report != "" {
			for o, n := range obs {
				seen[o] += n
			}
		}
		mu.Unlock()
	})
}

type teeWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *teeWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *teeWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *teeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finding is one way a response differs from the contract. Pointer is the JSON path with array
// indexes written as [], so that the same field of every item counts as one place.
type finding struct {
	Op      string `json:"op"`
	Status  int    `json:"status"`
	Pointer string `json:"pointer"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail,omitempty"`
}

// observation counts, in report mode, how often a property was null and how often it was not: the
// evidence for which properties the contract should call nullable.
type observation struct {
	Op      string `json:"op"`
	Pointer string `json:"pointer"`
	Null    bool   `json:"null"`
}

func checkResponse(_ *openapi3.T, op string, operation *openapi3.Operation, status int, contentType string, body []byte) ([]finding, map[observation]int) {
	if operation.Responses == nil {
		return nil, nil
	}
	resp := operation.Responses.Status(status)
	if resp == nil {
		resp = operation.Responses.Default()
	}
	if resp == nil || resp.Value == nil {
		return []finding{{Op: op, Status: status, Kind: "status not documented"}}, nil
	}
	if !strings.HasPrefix(contentType, "application/json") || len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}
	media := resp.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil {
		return []finding{{Op: op, Status: status, Kind: "JSON body not documented"}}, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return []finding{{Op: op, Status: status, Kind: "not JSON", Detail: err.Error()}}, nil
	}
	c := &checker{op: op, status: status, seen: map[observation]int{}}
	c.value(media.Schema, v, "")
	return c.found, c.seen
}

type checker struct {
	op     string
	status int
	found  []finding
	seen   map[observation]int
}

func (c *checker) add(ptr, kind, detail string) {
	c.found = append(c.found, finding{Op: c.op, Status: c.status, Pointer: ptr, Kind: kind, Detail: detail})
}

// flat is a schema with its allOf merged in: what a value must look like.
type flat struct {
	types     []string
	nullable  bool
	props     map[string][]*openapi3.SchemaRef // a property named by several allOf branches must meet each
	extra     bool                             // additionalProperties allows more
	extraRef  *openapi3.SchemaRef
	items     *openapi3.SchemaRef
	oneOf     openapi3.SchemaRefs
	anyType   bool // {} : any value
	hasObject bool
	said      bool // some part of it says something (a type, properties, items, a composition)
}

func flatten(ref *openapi3.SchemaRef) flat {
	f := flat{props: map[string][]*openapi3.SchemaRef{}}
	merge(&f, ref, 0)
	f.anyType = !f.said
	return f
}

func merge(f *flat, ref *openapi3.SchemaRef, depth int) {
	if ref == nil || ref.Value == nil || depth > 20 {
		return
	}
	s := ref.Value
	f.types = append(f.types, s.Type.Slice()...)
	if s.Nullable {
		f.nullable = true
	}
	for name, p := range s.Properties {
		f.props[name] = append(f.props[name], p)
		f.hasObject = true
	}
	if !isEmpty(ref) {
		f.said = true
	}
	if s.AdditionalProperties.Has != nil && *s.AdditionalProperties.Has {
		f.extra = true
	}
	if s.AdditionalProperties.Schema != nil {
		f.extra, f.extraRef = true, s.AdditionalProperties.Schema
	}
	if s.Items != nil {
		f.items = s.Items
	}
	f.oneOf = append(f.oneOf, s.OneOf...)
	f.oneOf = append(f.oneOf, s.AnyOf...)
	for _, a := range s.AllOf {
		merge(f, a, depth+1)
	}
}

// all is one schema that is every one of refs (an allOf), for a property several branches name.
func all(refs []*openapi3.SchemaRef) *openapi3.SchemaRef {
	if len(refs) == 1 {
		return refs[0]
	}
	return &openapi3.SchemaRef{Value: &openapi3.Schema{AllOf: refs}}
}

func isEmpty(ref *openapi3.SchemaRef) bool {
	s := ref.Value
	return s != nil && s.Type.IsEmpty() && len(s.Properties) == 0 && s.Items == nil && len(s.AllOf) == 0 &&
		len(s.OneOf) == 0 && len(s.AnyOf) == 0 && len(s.Enum) == 0 &&
		s.AdditionalProperties.Has == nil && s.AdditionalProperties.Schema == nil
}

func (f flat) allowsNull() bool {
	if f.nullable || f.anyType {
		return true
	}
	for _, t := range f.types {
		if t == openapi3.TypeNull {
			return true
		}
	}
	return false
}

func (c *checker) value(ref *openapi3.SchemaRef, v any, ptr string) {
	f := flatten(ref)
	if f.anyType && len(f.props) == 0 && len(f.oneOf) == 0 {
		return
	}
	if len(f.oneOf) > 0 {
		c.oneOf(f, v, ptr)
		return
	}
	if v == nil {
		if !f.allowsNull() {
			c.add(ptr, "null not allowed", "")
		}
		return
	}
	switch x := v.(type) {
	case map[string]any:
		if len(f.props) == 0 && !f.hasObject && !f.extra && !permits(f.types, "object") {
			c.add(ptr, "wrong type", "object")
			return
		}
		names := make([]string, 0, len(f.props))
		for name := range f.props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pv, ok := x[name]
			if !ok {
				c.add(ptr+"/"+name, "missing", "")
				continue
			}
			if c.seen != nil && c.status < 300 {
				c.seen[observation{Op: c.op, Pointer: ptr + "/" + name, Null: pv == nil}]++
			}
			c.value(all(f.props[name]), pv, ptr+"/"+name)
		}
		for name, pv := range x {
			if _, ok := f.props[name]; ok {
				continue
			}
			switch {
			case f.extraRef != nil:
				c.value(f.extraRef, pv, ptr+"/*")
			case f.extra:
			default:
				c.add(ptr+"/"+name, "not documented", "")
			}
		}
	case []any:
		if !permits(f.types, "array") && f.items == nil {
			c.add(ptr, "wrong type", "array")
			return
		}
		for _, item := range x {
			c.value(f.items, item, ptr+"/[]")
		}
	case string:
		if !permits(f.types, "string") {
			c.add(ptr, "wrong type", "string")
		}
	case json.Number:
		if !permits(f.types, "number") && !permits(f.types, "integer") {
			c.add(ptr, "wrong type", "number")
		} else if _, err := strconv.ParseInt(string(x), 10, 64); err != nil && !permits(f.types, "number") {
			c.add(ptr, "wrong type", "not an integer")
		}
	case bool:
		if !permits(f.types, "boolean") {
			c.add(ptr, "wrong type", "boolean")
		}
	}
}

// permits says whether a value of a JSON type fits the schema's types. No type at all permits
// anything (a schema that only lists properties, or an enum).
func permits(types []string, typ string) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if t == typ || (typ == "integer" && t == "number") {
			return true
		}
	}
	return false
}

// oneOf passes when one branch passes; otherwise it reports the branch that came closest.
func (c *checker) oneOf(f flat, v any, ptr string) {
	if v == nil && f.allowsNull() {
		return
	}
	var best []finding
	for _, branch := range f.oneOf {
		// Each branch counts into its own map: only the branch that passes is evidence of what was sent.
		sub := &checker{op: c.op, status: c.status, seen: map[observation]int{}}
		sub.value(branch, v, ptr)
		if len(sub.found) == 0 {
			for o, n := range sub.seen {
				c.seen[o] += n
			}
			return
		}
		if best == nil || len(sub.found) < len(best) {
			best = sub.found
		}
	}
	c.found = append(c.found, best...)
}

func summarize(fs []finding) string {
	seen := map[string]int{}
	var keys []string
	for _, f := range fs {
		k := fmt.Sprintf("%s -> %d %s: %s %s", f.Op, f.Status, f.Pointer, f.Kind, f.Detail)
		if seen[k] == 0 {
			keys = append(keys, k)
		}
		seen[k]++
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %s (x%d)\n", k, seen[k])
	}
	return b.String()
}

var reportMu sync.Mutex

func writeReport(dir string, fs []finding, seen map[observation]int) {
	reportMu.Lock()
	defer reportMu.Unlock()
	_ = os.MkdirAll(dir, 0o750) //nolint:gosec // a test report path
	appendJSONL(filepath.Join(dir, fmt.Sprintf("findings-%d.jsonl", os.Getpid())), func(enc *json.Encoder) {
		for _, f := range fs {
			_ = enc.Encode(f)
		}
	})
	appendJSONL(filepath.Join(dir, fmt.Sprintf("observed-%d.jsonl", os.Getpid())), func(enc *json.Encoder) {
		for o, n := range seen {
			_ = enc.Encode(struct {
				observation
				Count int `json:"count"`
			}{o, n})
		}
	})
}

func appendJSONL(name string, write func(*json.Encoder)) {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // a test report path
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	write(json.NewEncoder(file))
}
