package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
)

const testSecret = "unit-test-secret-unit-test-secret-1234"

var t0 = time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)

func newTokens(t *testing.T, at time.Time) *auth.Tokens {
	t.Helper()
	tokens, err := auth.NewTokens([]byte(testSecret), clock.Fixed{T: at})
	require.NoError(t, err)
	return tokens
}

func TestNewTokensRejectsAShortSecret(t *testing.T) {
	_, err := auth.NewTokens([]byte("too-short"), clock.System{})
	require.Error(t, err)
}

func TestAccessTokenRoundTrip(t *testing.T) {
	tokens := newTokens(t, t0)

	token, expiresAt, err := tokens.NewAccessToken(15, 7, 99)
	require.NoError(t, err)
	require.Equal(t, t0.Add(auth.AccessTokenTTL), expiresAt)

	claims, err := tokens.ParseAccessToken(token)
	require.NoError(t, err)
	require.Equal(t, auth.Claims{PersonnelID: 15, OrganizationID: 7, SessionID: 99}, claims)
}

func TestAccessTokenClaimNames(t *testing.T) {
	// docs/api-response.md §8: sub, organization_id, sid, iat, exp and nothing else.
	token, _, err := newTokens(t, t0).NewAccessToken(15, 7, 99)
	require.NoError(t, err)

	var got jwt.MapClaims
	_, _, err = jwt.NewParser().ParseUnverified(token, &got)
	require.NoError(t, err)
	require.Len(t, got, 5)
	require.Equal(t, "15", got["sub"])
	require.EqualValues(t, 7, got["organization_id"])
	require.Equal(t, "99", got["sid"])
	require.Contains(t, got, "iat")
	require.Contains(t, got, "exp")
}

func TestAccessTokenFailures(t *testing.T) {
	good, _, err := newTokens(t, t0).NewAccessToken(15, 7, 99)
	require.NoError(t, err)
	parts := strings.Split(good, ".")

	sign := func(method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(key)
		require.NoError(t, err)
		return s
	}
	valid := func() jwt.MapClaims {
		return jwt.MapClaims{
			"sub": "15", "organization_id": 7, "sid": "99",
			"iat": t0.Unix(), "exp": t0.Add(time.Minute).Unix(),
		}
	}
	without := func(key string) jwt.MapClaims {
		c := valid()
		delete(c, key)
		return c
	}
	override := func(key string, v any) jwt.MapClaims {
		c := valid()
		c[key] = v
		return c
	}
	secret := []byte(testSecret)

	tests := []struct {
		name    string
		token   string
		expired bool
	}{
		{name: "empty", token: ""},
		{name: "not a jwt", token: "garbage"},
		{name: "truncated", token: parts[0] + "." + parts[1]},
		{name: "tampered payload", token: parts[0] + "." + parts[1] + "x." + parts[2]},
		{name: "signed with another secret", token: sign(jwt.SigningMethodHS256, []byte("another-secret-another-secret-123456"), valid())},
		// ⚠ The classic forgery: no signature at all, with the algorithm set to "none".
		{name: "alg none", token: sign(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid())},
		{name: "another HMAC algorithm", token: sign(jwt.SigningMethodHS512, secret, valid())},
		{name: "no exp", token: sign(jwt.SigningMethodHS256, secret, without("exp"))},
		{name: "no sub", token: sign(jwt.SigningMethodHS256, secret, without("sub"))},
		{name: "no sid", token: sign(jwt.SigningMethodHS256, secret, without("sid"))},
		{name: "no organization_id", token: sign(jwt.SigningMethodHS256, secret, without("organization_id"))},
		{name: "non-numeric sub", token: sign(jwt.SigningMethodHS256, secret, override("sub", "budi"))},
		{name: "zero organization_id", token: sign(jwt.SigningMethodHS256, secret, override("organization_id", 0))},
		{name: "negative sid", token: sign(jwt.SigningMethodHS256, secret, override("sid", "-4"))},
		{name: "expired", token: sign(jwt.SigningMethodHS256, secret, override("exp", t0.Add(-time.Minute).Unix())), expired: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newTokens(t, t0).ParseAccessToken(tc.token)
			if tc.expired {
				require.ErrorIs(t, err, auth.ErrExpiredToken)
				require.NotErrorIs(t, err, auth.ErrInvalidToken)
				return
			}
			require.ErrorIs(t, err, auth.ErrInvalidToken)
			require.NotErrorIs(t, err, auth.ErrExpiredToken)
		})
	}
}

func TestAccessTokenExpiresOnTime(t *testing.T) {
	token, _, err := newTokens(t, t0).NewAccessToken(15, 7, 99)
	require.NoError(t, err)

	_, err = newTokens(t, t0.Add(auth.AccessTokenTTL-time.Second)).ParseAccessToken(token)
	require.NoError(t, err, "still valid one second before the expiry")

	_, err = newTokens(t, t0.Add(auth.AccessTokenTTL+time.Second)).ParseAccessToken(token)
	require.ErrorIs(t, err, auth.ErrExpiredToken)
}

func TestRefreshTokenRoundTrip(t *testing.T) {
	token, hash, err := auth.NewRefreshToken(7)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "7."), "the organization id is the prefix")
	require.Equal(t, auth.HashToken(token), hash)
	require.Len(t, hash, 64)
	require.NotContains(t, hash, token, "only the hash is stored")

	orgID, err := auth.ParseRefreshToken(token)
	require.NoError(t, err)
	require.EqualValues(t, 7, orgID)

	again, _, err := auth.NewRefreshToken(7)
	require.NoError(t, err)
	require.NotEqual(t, token, again, "every token is fresh randomness")
}

func TestParseRefreshTokenRejectsMalformedValues(t *testing.T) {
	good, _, err := auth.NewRefreshToken(7)
	require.NoError(t, err)
	random := strings.TrimPrefix(good, "7.")

	tests := map[string]string{
		"empty":                     "",
		"no separator":              "7" + random,
		"no prefix":                 "." + random,
		"no random part":            "7.",
		"zero organization":         "0." + random,
		"negative organization":     "-7." + random,
		"organization not a number": "abc." + random,
		"leading zero":              "07." + random,
		"plus sign":                 "+7." + random,
		"random part too short":     "7." + random[:42],
		"random part too long":      "7." + random + "A",
		"not base64url":             "7." + strings.Repeat("!", 43),
		"padding":                   "7." + random[:42] + "=",
		"extra separator":           "7." + random[:20] + "." + random[21:],
		"organization overflow":     "99999999999999999999." + random,
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := auth.ParseRefreshToken(token)
			require.ErrorIs(t, err, auth.ErrInvalidRefreshToken)
		})
	}
}
