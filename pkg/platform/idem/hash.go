package idem

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// requestHash is the SHA-256 of the request body in canonical form, so that the same JSON
// with its keys in another order, or with other whitespace, hashes the same. A client
// that rebuilds the body from its local queue does not necessarily write the keys in the
// same order each time, and that must not look like a different request.
//
// The query string is part of the hash: it can change what the request means. A body that
// is empty or is not JSON is hashed as it is.
func requestHash(body []byte, rawQuery string) []byte {
	h := sha256.New()
	h.Write(canonicalJSON(body))
	h.Write([]byte{0})
	h.Write([]byte(rawQuery))
	return h.Sum(nil)
}

// canonicalJSON re-encodes valid JSON with sorted keys and no whitespace. encoding/json
// sorts map keys when it marshals; UseNumber keeps numbers exactly as written instead of
// squeezing them through float64.
func canonicalJSON(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return body
	}
	if _, err := dec.Token(); err == nil { // trailing data: not a single JSON value
		return body
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}
