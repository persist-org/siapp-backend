package auth

import (
	"strings"
	"unicode/utf8"

	"siapp/internal/platform/timeutil"
)

const minPasswordRunes = 8

// PasswordSubject is what the password rules compare a password against.
type PasswordSubject struct {
	Email    string
	Username string
}

// ValidatePassword checks a password that a user chose. It returns ErrPasswordWeak, with the
// broken rule as the message, or nil.
//
// The rules: at least 8 characters, and not the same as the part of the email before the "@"
// or the username (compared without regard to case).
//
// ⚠ A birthdate-style password such as 10051994 (DDMMYYYY) is deliberately ALLOWED. It is the
// default password every new user starts with (DefaultPassword), and it is the owner's
// decision to keep it, so a rule against it would also forbid the account's own starting
// password. Do not add that rule back.
//
// The rules apply only when a user picks a password (reset, change). The default password
// made by DefaultPassword is never checked.
func ValidatePassword(password string, subject PasswordSubject) error {
	if utf8.RuneCountInString(password) < minPasswordRunes {
		return ErrPasswordWeak.WithMessage("Kata sandi minimal 8 karakter.")
	}
	if local, _, _ := strings.Cut(subject.Email, "@"); local != "" && strings.EqualFold(password, local) {
		return ErrPasswordWeak.WithMessage("Kata sandi tidak boleh sama dengan bagian depan email Anda.")
	}
	if subject.Username != "" && strings.EqualFold(password, subject.Username) {
		return ErrPasswordWeak.WithMessage("Kata sandi tidak boleh sama dengan username Anda.")
	}
	return nil
}

// DefaultPassword is the password a new user starts with: their birthdate as DDMMYYYY, with
// day and month zero-padded, so 1994-05-10 gives "10051994".
//
// ⚠ This is the v1.2 behaviour, kept on purpose (owner decision). It is guessable by anyone
// who knows the person's birthdate, and the user is not forced to change it on first login.
// What limits the damage is the login rate limits and the account lockout (Handler.login,
// Service.Login). The password is hashed like any other and is never returned, logged or
// emailed. Changing a user's birthDate later does not change their password.
func DefaultPassword(birthDate timeutil.Date) string {
	return birthDate.Time().Format("02012006")
}
