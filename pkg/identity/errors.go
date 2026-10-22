package identity

import (
	"net/http"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
)

// Error reasons of module 01's sessions (details.reason). The token reasons
// (AUTH_TOKEN_INVALID, AUTH_TOKEN_EXPIRED) live with the middleware in pkg/platform/auth, and
// the admin reasons (NOT_ADMIN, ACCESS_*) in pkg/access. A reason that has shipped is never
// renamed.
const (
	ReasonInvalidCredentials    = "AUTH_INVALID_CREDENTIALS" //nolint:gosec // a reason code, not a secret
	ReasonAccountDisabled       = "AUTH_ACCOUNT_DISABLED"
	ReasonDeviceBound           = "AUTH_DEVICE_BOUND"
	ReasonOrganizationSuspended = "AUTH_ORGANIZATION_SUSPENDED"
	ReasonNotInGroup            = "AUTH_NOT_IN_GROUP"
	ReasonRefreshReused         = "AUTH_REFRESH_REUSED"
	ReasonDeviceChanged         = "AUTH_DEVICE_CHANGED"
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

// ErrNotInGroup is returned after a correct password for a person employed by a company
// outside the group (KPI, IDS, PCD): SiAPP serves only the group (D17).
var ErrNotInGroup = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonNotInGroup, "Perusahaan Anda tidak terdaftar di SiAPP. Hubungi administrator.")

// ErrRefreshTokenInvalid is returned for a refresh token that is malformed, unknown, expired
// or from another organization. The reasons are deliberately not told apart. The client has
// to log in again.
var ErrRefreshTokenInvalid = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated,
	auth.ReasonTokenInvalid, "Sesi tidak valid atau sudah berakhir. Silakan masuk kembali.")

// ErrRefreshReused is returned when a refresh token that was already used is presented
// again. Every session in its chain has been ended.
var ErrRefreshReused = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated,
	ReasonRefreshReused, "Sesi dihentikan karena token digunakan ulang. Silakan masuk kembali.")

// ErrDeviceChanged is returned when the phone of a session is no longer the bound phone of
// the person: the binding was reset, or another phone was bound since. The chain has been
// ended; signing in again binds the phone they use now.
var ErrDeviceChanged = apperr.New(http.StatusUnauthorized, apperr.CodeUnauthenticated,
	ReasonDeviceChanged, "Perangkat Anda tidak lagi terdaftar untuk akun ini. Silakan masuk kembali.")
