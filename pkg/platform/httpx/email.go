package httpx

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Email is an email address in a request body. It is trimmed and lowercased while the body is
// decoded, before the validation rules run.
//
// Doing it at the edge matters in practice: phone keyboards often add a trailing space after
// an address, and an address is case-insensitive in every way this API uses it (accounts are
// stored lowercase). Without this, "Budi@Example.com " would fail validation, or worse, be
// validated and then compared in a different form further in.
//
// Use it with the `email` rule: `Email httpx.Email `json:"email" validate:"required,email"“.
type Email string

// UnmarshalJSON trims and lowercases the address.
func (e *Email) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("email must be a string: %w", err)
	}
	*e = Email(strings.ToLower(strings.TrimSpace(s)))
	return nil
}

// String returns the address.
func (e Email) String() string { return string(e) }
