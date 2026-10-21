// Package apperr defines the expected, client-facing failures of the API. Handlers and
// services return *Error for these; anything else is treated as an internal error.
//
// The shape follows the PERSIST contract's ErrorEnvelope (docs/api-response.md): Code is
// one value of the contract's ErrorCode enum, Reason is SiAPP's precise module-specific
// reason (sent as details.reason), and Fields fills error.errors for VALIDATION_ERROR.
package apperr

import (
	"fmt"
	"net/http"

	"siapp/internal/apigen"
)

// FieldError is the contract's FieldError: one bad request field.
type FieldError = apigen.FieldError

// Code is the contract's ErrorCode enum.
type Code = apigen.ErrorCode

// The ErrorCode values SiAPP uses. INSUFFICIENT_SCOPE and ORG_MISMATCH are not used: another
// organization's data is answered with 404 NOT_FOUND (docs/api-response.md).
const (
	CodeBadRequest      Code = apigen.BADREQUEST
	CodeUnauthenticated Code = apigen.UNAUTHENTICATED
	CodeTokenExpired    Code = apigen.TOKENEXPIRED
	CodeForbidden       Code = apigen.FORBIDDEN
	CodeRoleDenied      Code = apigen.ROLEDENIED
	CodeScopeDenied     Code = apigen.SCOPEDENIED
	CodeNotFound        Code = apigen.NOTFOUND
	CodeConflict        Code = apigen.CONFLICT
	CodeDuplicate       Code = apigen.DUPLICATE
	CodePayloadTooLarge Code = apigen.PAYLOADTOOLARGE
	CodeValidation      Code = apigen.VALIDATIONERROR
	CodeUnprocessable   Code = apigen.UNPROCESSABLE
	CodeUnderstaffed    Code = apigen.UNDERSTAFFED
	CodeRateLimited     Code = apigen.RATELIMITED
	CodeInternal        Code = apigen.INTERNALERROR
)

// Error is an expected failure with the HTTP status, stable code and Indonesian message
// that are sent to the client.
type Error struct {
	Status int
	Code   Code
	// Reason is the module-specific reason, UPPER_SNAKE and prefixed by the module
	// (AUTH_DEVICE_BOUND, ATTENDANCE_QR_EXPIRED). Sent as details.reason; "" when the code
	// says everything. A shipped reason is never renamed.
	Reason  string
	Message string
	// Details is extra context (retry_after, scope_required). Reason is added to it when
	// the response is written; do not put "reason" here.
	Details map[string]any
	// Fields is filled for VALIDATION_ERROR, one entry per bad field.
	Fields []FieldError
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s (%s): %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Is makes errors.Is see through the copies the With* methods make: an error matches a shared
// one with the same code and reason, whatever its message, details or fields. Errors without
// a reason (the generic ones) only match themselves.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Reason != "" && e.Reason == t.Reason && e.Code == t.Code
}

// New returns an Error with the given status, code, reason and message.
func New(status int, code Code, reason, msg string) *Error {
	return &Error{Status: status, Code: code, Reason: reason, Message: msg}
}

// WithDetails returns a copy of e carrying details. It copies because the shared values
// below are package-level pointers that many requests use at once.
func (e *Error) WithDetails(details map[string]any) *Error {
	c := *e
	c.Details = details
	return &c
}

// WithFields returns a copy of e carrying per-field validation errors.
func (e *Error) WithFields(fields ...FieldError) *Error {
	c := *e
	c.Fields = fields
	return &c
}

// WithMessage returns a copy of e with a more specific message.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

// WithReason returns a copy of e with a module-specific reason.
func (e *Error) WithReason(reason string) *Error {
	c := *e
	c.Reason = reason
	return &c
}

// Shared errors for the codes above. Modules use them directly or derive from them with
// WithReason / WithMessage / WithDetails.
var (
	ErrBadRequest      = New(http.StatusBadRequest, CodeBadRequest, "", "Permintaan tidak dapat dibaca.")
	ErrUnauthenticated = New(http.StatusUnauthorized, CodeUnauthenticated, "", "Autentikasi diperlukan.")
	ErrForbidden       = New(http.StatusForbidden, CodeForbidden, "", "Anda tidak memiliki izin untuk tindakan ini.")
	ErrNotFound        = New(http.StatusNotFound, CodeNotFound, "", "Data tidak ditemukan.")
	ErrConflict        = New(http.StatusConflict, CodeConflict, "", "Data bertentangan dengan kondisi saat ini.")
	ErrDuplicate       = New(http.StatusConflict, CodeDuplicate, "", "Data sudah ada.")
	ErrPayloadTooLarge = New(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "", "Body terlalu besar.")
	ErrValidation      = New(http.StatusUnprocessableEntity, CodeValidation, "", "Validasi gagal pada beberapa field.")
	ErrUnprocessable   = New(http.StatusUnprocessableEntity, CodeUnprocessable, "", "Permintaan tidak dapat diproses.")
	ErrRateLimited     = New(http.StatusTooManyRequests, CodeRateLimited, "", "Terlalu banyak permintaan. Coba lagi nanti.")
	ErrInternal        = New(http.StatusInternalServerError, CodeInternal, "", "Terjadi kesalahan pada server. Silakan coba lagi.")
)
