package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ETag is the entity tag of a response's data: a hash of its JSON, in quotes. It hashes the data, not the whole
// envelope, because the envelope carries a request id that changes on every call.
//
// ⚠ A hash of the body is the honest key for a read that depends on who asks and on what they did: the tag
// changes exactly when what the client would receive changes, however that came about.
func ETag(data any) (string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:16]) + `"`, nil
}

// IfNoneMatch says whether the request's If-None-Match names the tag (or is *), so the answer can be 304. A
// weak validator (W/"...") counts: for a GET the two compare the same.
func IfNoneMatch(r *http.Request, tag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}
