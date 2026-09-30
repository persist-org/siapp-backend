package auth

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/apperr"
	"siapp/internal/platform/timeutil"
)

func TestValidatePassword(t *testing.T) {
	subject := PasswordSubject{Email: "Budi.Santoso@example.com", Username: "budi_s"}

	tests := []struct {
		name     string
		password string
		weak     bool
	}{
		{"8 characters is enough", "abcdefgh", false},
		{"7 characters is not", "abcdefg", true},
		{"empty", "", true},
		{"long passphrase", "correct horse battery staple", false},
		// Length counts characters, not bytes: 7 Japanese characters are 21 bytes.
		{"7 multi-byte characters", "あいうえおかき", true},
		{"8 multi-byte characters", "あいうえおかきく", false},

		{"the email name", "budi.santoso", true},
		{"the email name in other case", "BUDI.SANTOSO", true},
		{"a longer password that merely contains the email name", "budi.santoso123", false},

		// ⚠ Owner decision: a birthdate is allowed, because it is the default password.
		{"a birthdate as DDMMYYYY", "10051994", false},
		{"another birthdate", "01012000", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password, subject)
			if !tc.weak {
				require.NoError(t, err)
				return
			}
			var appErr *apperr.Error
			require.ErrorAs(t, err, &appErr)
			require.Equal(t, CodePasswordWeak, appErr.Code)
			require.Equal(t, 400, appErr.Status)
			require.NotEmpty(t, appErr.Message)
		})
	}
}

func TestValidatePasswordUsernameRuleOnItsOwn(t *testing.T) {
	// Long enough to pass the length rule, so only the username rule can fail it.
	err := ValidatePassword("longusername", PasswordSubject{Email: "x@example.com", Username: "LongUsername"})
	var appErr *apperr.Error
	require.ErrorAs(t, err, &appErr)
	require.Equal(t, CodePasswordWeak, appErr.Code)
	require.Contains(t, appErr.Message, "username")
}

func TestValidatePasswordEmptySubjectFieldsNeverMatch(t *testing.T) {
	// An empty username must not reject the password "" or anything else by accident.
	require.NoError(t, ValidatePassword("abcdefgh", PasswordSubject{}))
}

func TestDefaultPassword(t *testing.T) {
	tests := []struct {
		birth time.Time
		want  string
	}{
		{time.Date(1994, time.May, 10, 0, 0, 0, 0, time.UTC), "10051994"},
		{time.Date(2001, time.January, 2, 0, 0, 0, 0, time.UTC), "02012001"}, // day and month are zero-padded
		{time.Date(1999, time.December, 31, 0, 0, 0, 0, time.UTC), "31121999"},
		{time.Date(1980, time.February, 29, 0, 0, 0, 0, time.UTC), "29021980"},
	}
	for _, tc := range tests {
		d := timeutil.NewDate(tc.birth.Year(), tc.birth.Month(), tc.birth.Day())
		require.Equal(t, tc.want, DefaultPassword(d), d.String())
		require.Len(t, DefaultPassword(d), minPasswordRunes, "the default password satisfies the length rule")
	}
}

func TestResetLink(t *testing.T) {
	link := func(resetURL, token string) (string, error) {
		return (&Service{resetURL: resetURL}).resetLink(token)
	}

	got, err := link("https://app.example.com/reset-password", "abc_DEF-123")
	require.NoError(t, err)
	require.Equal(t, "https://app.example.com/reset-password?token=abc_DEF-123", got)

	t.Run("keeps an existing query", func(t *testing.T) {
		got, err := link("https://app.example.com/reset?lang=id", "tok")
		require.NoError(t, err)
		require.Equal(t, "https://app.example.com/reset?lang=id&token=tok", got)
	})
	t.Run("escapes the token", func(t *testing.T) {
		got, err := link("https://app.example.com/reset", "a b&c")
		require.NoError(t, err)
		require.Equal(t, "https://app.example.com/reset?token=a+b%26c", got)
	})
	t.Run("a relative or empty URL is a configuration error", func(t *testing.T) {
		for _, bad := range []string{"", "/reset", "reset-password", "://x"} {
			_, err := link(bad, "tok")
			require.Error(t, err, bad)
		}
	})
}

func TestEscapeLike(t *testing.T) {
	tests := map[string]string{
		"budi":      "budi",
		"50%":       `50\%`,
		"a_b":       `a\_b`,
		`back\`:     `back\\`,
		`%_\`:       `\%\_\\`,
		"":          "",
		"日本語":       "日本語",
		"100% sure": `100\% sure`,
	}
	for in, want := range tests {
		require.Equal(t, want, escapeLike(in), in)
	}
}

func TestCheckBirthDate(t *testing.T) {
	now := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		date timeutil.Date
		ok   bool
	}{
		{"a normal date", timeutil.NewDate(1994, time.May, 10), true},
		{"the first allowed day", timeutil.NewDate(1900, time.January, 1), true},
		{"the day before", timeutil.NewDate(1899, time.December, 31), false},
		{"today in UTC", timeutil.NewDate(2026, time.March, 10), true},
		// 12:00 UTC is 21:00 in Jayapura, still the 10th there. Midnight there is 15:00 UTC,
		// so on the 10th at 16:00 UTC it is already the 11th in Jayapura.
		{"tomorrow is not a birthday yet", timeutil.NewDate(2026, time.March, 11), false},
		{"the far future", timeutil.NewDate(2999, time.January, 1), false},
		{"the zero date", timeutil.Date{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBirthDate(tc.date, now)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}

	t.Run("a date that is already today in Jayapura is accepted late in the UTC day", func(t *testing.T) {
		late := time.Date(2026, time.March, 10, 16, 0, 0, 0, time.UTC) // 01:00 on the 11th in Jayapura
		require.NoError(t, checkBirthDate(timeutil.NewDate(2026, time.March, 11), late))
	})
}

func TestAdminUserDTO(t *testing.T) {
	created := time.Date(2026, time.March, 10, 6, 30, 0, 120_000_000, time.UTC)
	full := db.User{
		ID: "id-1", Email: "a@example.com", FullName: "A", SystemRole: db.UsersSystemRoleADMIN, Status: db.UsersStatusACTIVE,
		Username:    sql.NullString{String: "a_user", Valid: true},
		Phone:       sql.NullString{String: "0812", Valid: true},
		BirthDate:   sql.NullTime{Time: time.Date(1994, time.May, 10, 0, 0, 0, 0, time.UTC), Valid: true},
		LastLoginAt: sql.NullTime{Time: created, Valid: true},
		CreatedAt:   created,
		// The hash must never reach the DTO; there is no field for it, and this documents why.
		PasswordHash: "$argon2id$secret",
	}
	dto := newAdminUserDTO(full)
	require.Equal(t, "a_user", *dto.Username)
	require.Equal(t, "0812", *dto.Phone)
	require.Equal(t, "1994-05-10", dto.BirthDate.String())
	require.Equal(t, "2026-03-10T06:30:00.120Z", *dto.LastLoginAt)
	require.Equal(t, "2026-03-10T06:30:00.120Z", dto.CreatedAt)

	empty := newAdminUserDTO(db.User{ID: "id-2", CreatedAt: created})
	require.Nil(t, empty.Username)
	require.Nil(t, empty.Phone)
	require.Nil(t, empty.LastLoginAt)
	require.True(t, empty.BirthDate.IsZero())
}

func TestUpdateUserRequestIsEmpty(t *testing.T) {
	require.True(t, UpdateUserRequest{}.isEmpty())
	name := "x"
	require.False(t, UpdateUserRequest{FullName: &name}.isEmpty())
	empty := ""
	require.False(t, UpdateUserRequest{Phone: &empty}.isEmpty(), "an empty string is a request to clear")
}
