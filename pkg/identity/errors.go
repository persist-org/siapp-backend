package identity

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons of the login flow (details.reason). Phases 01.2 and 01.3 add their own to
// this file. A reason that has shipped is never renamed.
const (
	ReasonInvalidCredentials    = "AUTH_INVALID_CREDENTIALS" //nolint:gosec // a reason code, not a secret
	ReasonAccountDisabled       = "AUTH_ACCOUNT_DISABLED"
	ReasonDeviceBound           = "AUTH_DEVICE_BOUND"
	ReasonOrganizationSuspended = "AUTH_ORGANIZATION_SUSPENDED"
)

// ErrInvalidCredentials is returned for an unknown email and for a wrong password alike, so
// the response cannot be used to find out which emails have accounts.
var ErrInvalidCredentials = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated,
	ReasonInvalidCredentials, "Email atau kata sandi salah.")

// ErrAccountDisabled is returned after a correct password for a person who may not sign in:
// deactivated or resigned, or without an employee record.
var ErrAccountDisabled = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonAccountDisabled, "Akun Anda tidak aktif. Hubungi HR atau administrator.")

// ErrDeviceBound is returned when the person's account is bound to another phone.
var ErrDeviceBound = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonDeviceBound, "Akun Anda terhubung dengan perangkat lain. Hubungi admin untuk ganti perangkat.")

// ErrOrganizationSuspended is returned when the person's organization has been suspended.
var ErrOrganizationSuspended = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonOrganizationSuspended, "Organisasi Anda sedang dinonaktifkan. Hubungi administrator.")
