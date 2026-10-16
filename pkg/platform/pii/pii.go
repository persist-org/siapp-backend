// Package pii holds the helpers that keep personal data out of responses.
package pii

import "strings"

// MaskNIK hides all but the last 4 characters (`************1234`).
//
// ⚠ NIK is PII with the highest sensitivity (persist AC-W23-6): every response masks it
// except where a phase says otherwise, and unmasking is audited. A value of 4 characters or
// fewer is masked entirely, because showing it would show the whole number.
func MaskNIK(nik string) string {
	runes := []rune(nik)
	if len(runes) == 0 {
		return ""
	}
	keep := 0
	if len(runes) > 4 {
		keep = 4
	}
	return strings.Repeat("*", len(runes)-keep) + string(runes[len(runes)-keep:])
}
