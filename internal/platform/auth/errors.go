package auth

import (
	"net/http"

	"siapp/internal/platform/apperr"
)

// Error codes of the login flow. Later auth phases add their own to this file.
const (
	// CodeInvalidCredentials is a name, not a credential; gosec's G101 matches on "CREDENTIALS".
	CodeInvalidCredentials = "AUTH_INVALID_CREDENTIALS" //nolint:gosec // an error code, not a secret
	CodeAccountLocked      = "AUTH_ACCOUNT_LOCKED"
	CodeAccountDisabled    = "AUTH_ACCOUNT_DISABLED"
	CodeTokenInvalid       = "AUTH_TOKEN_INVALID" //nolint:gosec // an error code, not a secret
	CodeRefreshReused      = "AUTH_REFRESH_REUSED"
)

// ErrInvalidCredentials is returned for an unknown user and for a wrong password alike, so
// the response cannot be used to find out which emails have accounts.
var ErrInvalidCredentials = apperr.New(http.StatusUnauthorized, CodeInvalidCredentials, "Email/username atau kata sandi salah.")

// ErrAccountLocked is returned while an account is locked after too many failed logins.
var ErrAccountLocked = apperr.New(http.StatusLocked, CodeAccountLocked, "Akun dikunci sementara karena terlalu banyak percobaan masuk yang gagal. Coba lagi dalam 15 menit.")

// ErrRefreshTokenInvalid is returned for a refresh token that is unknown or expired. The
// client has to log in again.
var ErrRefreshTokenInvalid = apperr.New(http.StatusUnauthorized, CodeTokenInvalid, "Sesi tidak valid atau sudah berakhir. Silakan masuk kembali.")

// ErrRefreshReused is returned when a refresh token that was already used, or whose session
// was ended, is presented again. Every session in its family has been revoked.
var ErrRefreshReused = apperr.New(http.StatusUnauthorized, CodeRefreshReused, "Sesi dihentikan karena token digunakan ulang. Silakan masuk kembali.")

// ErrAccountDisabled is returned after a correct password for a disabled account.
var ErrAccountDisabled = apperr.New(http.StatusForbidden, CodeAccountDisabled, "Akun Anda dinonaktifkan. Hubungi administrator.")
