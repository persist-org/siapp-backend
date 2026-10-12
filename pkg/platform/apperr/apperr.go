// Package apperr defines the expected, client-facing failures of the API. Handlers and
// services return *Error for these; anything else is treated as an internal error.
package apperr

import (
	"fmt"
	"net/http"
)

// Codes shared by every module. They are stable UPPER_SNAKE strings that clients switch on.
const (
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeNotFound           = "NOT_FOUND"
	CodeConflict           = "CONFLICT"
	CodeIDConflict         = "ID_CONFLICT"
	CodeOccurredAtInFuture = "OCCURRED_AT_IN_FUTURE"
	CodeOccurredAtTooOld   = "OCCURRED_AT_TOO_OLD"
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodePermissionDenied   = "PERMISSION_DENIED"
	CodeRateLimited        = "RATE_LIMITED"
	CodeInternal           = "INTERNAL_ERROR"
)

// Error is an expected failure with the HTTP status, stable code and Indonesian message
// that are sent to the client. Details is optional structured context, e.g. field errors.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// New returns an Error with the given status, code and message.
func New(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

// WithDetails returns a copy of e carrying details. It copies because the shared values
// below are package-level pointers that many requests use at once.
func (e *Error) WithDetails(details any) *Error {
	c := *e
	c.Details = details
	return &c
}

// WithMessage returns a copy of e with a more specific message.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

// Shared errors for the codes above. Modules use them directly or derive from them with
// WithMessage / WithDetails.
var (
	ErrValidationFailed   = New(http.StatusBadRequest, CodeValidationFailed, "Data yang dikirim tidak valid.")
	ErrNotFound           = New(http.StatusNotFound, CodeNotFound, "Data tidak ditemukan.")
	ErrConflict           = New(http.StatusConflict, CodeConflict, "Data bertentangan dengan kondisi saat ini.")
	ErrIDConflict         = New(http.StatusConflict, CodeIDConflict, "ID sudah digunakan oleh pengguna lain.")
	ErrOccurredAtInFuture = New(http.StatusUnprocessableEntity, CodeOccurredAtInFuture, "Waktu kejadian tidak boleh di masa depan.")
	ErrOccurredAtTooOld   = New(http.StatusUnprocessableEntity, CodeOccurredAtTooOld, "Waktu kejadian terlalu lama.")
	ErrUnauthenticated    = New(http.StatusUnauthorized, CodeUnauthenticated, "Autentikasi diperlukan.")
	ErrPermissionDenied   = New(http.StatusForbidden, CodePermissionDenied, "Anda tidak memiliki izin untuk tindakan ini.")
	ErrRateLimited        = New(http.StatusTooManyRequests, CodeRateLimited, "Terlalu banyak permintaan. Coba lagi nanti.")
	ErrInternal           = New(http.StatusInternalServerError, CodeInternal, "Terjadi kesalahan pada server. Silakan coba lagi.")
)
