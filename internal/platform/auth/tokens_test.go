package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/internal/platform/clock"
)

var testSecret = []byte("unit-test-secret-unit-test-secret-0123")

// stepClock is a clock the test can move.
type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func TestAccessTokenRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	tokens := NewTokens(testSecret, clock.Fixed{T: now})

	token, expiresAt, err := tokens.NewAccessToken("user-1", db.UsersSystemRoleADMIN, "session-1")
	require.NoError(t, err)
	require.Equal(t, now.Add(15*time.Minute), expiresAt)

	claims, err := tokens.ParseAccessToken(token)
	require.NoError(t, err)
	require.Equal(t, Claims{UserID: "user-1", SystemRole: db.UsersSystemRoleADMIN, SessionID: "session-1"}, claims)
}

func TestAccessTokenExpiry(t *testing.T) {
	c := &stepClock{now: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
	tokens := NewTokens(testSecret, c)
	token, _, err := tokens.NewAccessToken("user-1", db.UsersSystemRoleMEMBER, "session-1")
	require.NoError(t, err)

	c.now = c.now.Add(14*time.Minute + 59*time.Second)
	_, err = tokens.ParseAccessToken(token)
	require.NoError(t, err, "still valid just before 15 minutes")

	c.now = c.now.Add(2 * time.Second)
	_, err = tokens.ParseAccessToken(token)
	require.ErrorIs(t, err, ErrInvalidToken, "expired after 15 minutes")
}

func TestParseAccessTokenRejectsForgeries(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	tokens := NewTokens(testSecret, clock.Fixed{T: now})

	claims := func(mod func(*accessClaims)) accessClaims {
		c := accessClaims{
			Role:      "MEMBER",
			SessionID: "session-1",
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   "user-1",
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		}
		if mod != nil {
			mod(&c)
		}
		return c
	}
	sign := func(method jwt.SigningMethod, key any, c accessClaims) string {
		s, err := jwt.NewWithClaims(method, c).SignedString(key)
		require.NoError(t, err)
		return s
	}

	// Sanity check: the helper's baseline token is accepted, so each rejection below is
	// caused by the one thing that differs.
	_, err := tokens.ParseAccessToken(sign(jwt.SigningMethodHS256, testSecret, claims(nil)))
	require.NoError(t, err)

	tests := map[string]string{
		"empty":            "",
		"garbage":          "not.a.jwt",
		"wrong secret":     sign(jwt.SigningMethodHS256, []byte("another-secret-another-secret-another"), claims(nil)),
		"alg none":         sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claims(nil)),
		"other HMAC alg":   sign(jwt.SigningMethodHS512, testSecret, claims(nil)),
		"expired":          sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Second)) })),
		"no expiry":        sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.ExpiresAt = nil })),
		"no subject":       sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.Subject = "" })),
		"no session id":    sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.SessionID = "" })),
		"unknown role":     sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.Role = "ROOT" })),
		"no role":          sign(jwt.SigningMethodHS256, testSecret, claims(func(c *accessClaims) { c.Role = "" })),
		"tampered payload": tamper(t, sign(jwt.SigningMethodHS256, testSecret, claims(nil))),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.ParseAccessToken(token)
			require.ErrorIs(t, err, ErrInvalidToken)
		})
	}
}

// tamper flips a character in the payload segment, leaving the signature untouched.
func tamper(t *testing.T, token string) string {
	t.Helper()
	b := []byte(token)
	first := 0
	for b[first] != '.' {
		first++
	}
	b[first+5] ^= 0x01
	return string(b)
}

func TestRefreshToken(t *testing.T) {
	token, hash, err := NewRefreshToken()
	require.NoError(t, err)
	require.Len(t, token, 43, "32 bytes in unpadded base64url")
	require.Regexp(t, `^[A-Za-z0-9_-]+$`, token)
	require.Len(t, hash, 64)
	require.Equal(t, hash, HashRefreshToken(token))
	require.NotContains(t, hash, token)

	other, otherHash, err := NewRefreshToken()
	require.NoError(t, err)
	require.NotEqual(t, token, other)
	require.NotEqual(t, hash, otherHash)
}

func TestHashRefreshTokenKnownVector(t *testing.T) {
	// SHA-256("abc"), from FIPS 180-2.
	require.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", HashRefreshToken("abc"))
}

func TestNormalizeIdentifier(t *testing.T) {
	require.Equal(t, "budi@example.com", normalizeIdentifier("  Budi@Example.COM "))
	require.Equal(t, "Budi_S", normalizeIdentifier(" Budi_S "), "usernames keep their case")
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "abc", truncate("abc", 10))
	require.Equal(t, "ab", truncate("abcdef", 2))
	// "é" is two bytes; cutting between them must back off to a rune boundary.
	require.Equal(t, "a", truncate("aé", 2))
	require.Equal(t, "aé", truncate("aé", 3))
}
