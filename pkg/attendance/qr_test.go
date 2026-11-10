package attendance

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/clock"
)

const qrTestSecret = "unit-test-attendance-qr-secret-0123456789"

var qrNow = time.Date(2026, 3, 10, 1, 30, 0, 0, time.UTC)

func qrTokensAt(t *testing.T, secret string, at time.Time) *QRTokens {
	t.Helper()
	tokens, err := NewQRTokens(secret, clock.Fixed{T: at})
	require.NoError(t, err)
	return tokens
}

// signed builds a token for an arbitrary payload, signed with the real key: for the payloads Issue
// never produces.
func signed(t *testing.T, tokens *QRTokens, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(raw) + "." + enc.EncodeToString(tokens.mac(raw))
}

// spareBits returns another base64url character that decodes to the same bytes as c when it is the
// last of a 43-character (32-byte) string: the final character carries only 4 payload bits, so the
// low 2 bits are spare.
func spareBits(c byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, c)
	return string(alphabet[i^1])
}

func TestNewQRTokensRefusesAShortSecret(t *testing.T) {
	_, err := NewQRTokens(strings.Repeat("k", minQRSecretBytes-1), clock.Fixed{T: qrNow})
	require.Error(t, err)
	_, err = NewQRTokens("", clock.Fixed{T: qrNow})
	require.Error(t, err)
	_, err = NewQRTokens(strings.Repeat("k", minQRSecretBytes), clock.Fixed{T: qrNow})
	require.NoError(t, err)
}

func TestQRTokenRoundTrips(t *testing.T) {
	tokens := qrTokensAt(t, qrTestSecret, qrNow)
	token, issued := tokens.Issue(7, "1K-IBMI-S1", 42, DirectionIn)

	got, err := tokens.Verify(token)
	require.NoError(t, err)
	require.Equal(t, issued, got)
	require.Equal(t, QRClaims{OrganizationID: 7, TeamCode: "1K-IBMI-S1", DanruID: 42, Direction: DirectionIn, IssuedAt: qrNow}, got)
	require.Equal(t, qrNow.Add(qrTokenTTL), got.ExpiresAt())
}

func TestQRTokenVerification(t *testing.T) {
	issuer := qrTokensAt(t, qrTestSecret, qrNow)
	token, _ := issuer.Issue(7, "1K-IBMI-S1", 42, DirectionOut)
	body, sig, _ := strings.Cut(token, ".")

	flip := func(s string) string { // a different, still well-formed, base64url string
		b := []byte(s)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		return string(b)
	}
	enc := base64.RawURLEncoding
	otherOrg, _ := json.Marshal(qrPayload{V: qrVersion, Org: 8, Team: "1K-IBMI-S1", Danru: 42, Dir: DirectionOut, Iat: qrNow.Unix()})

	for _, tc := range []struct {
		name  string
		token string
		at    time.Time
		with  string // the verifier's secret, qrTestSecret when empty
		want  error
	}{
		{"valid", token, qrNow, "", nil},
		{"valid at the last second", token, qrNow.Add(qrTokenTTL), "", nil},
		{"valid inside the tolerance after expiry", token, qrNow.Add(qrTokenTTL + qrClockTolerance), "", nil},
		{"expired just past the tolerance", token, qrNow.Add(qrTokenTTL + qrClockTolerance + time.Second), "", errQRExpired},
		{"expired long ago", token, qrNow.Add(24 * time.Hour), "", errQRExpired},
		{"issued a few seconds ahead of this clock is fine", token, qrNow.Add(-qrClockTolerance), "", nil},
		{"issued far ahead of this clock is not", token, qrNow.Add(-qrClockTolerance - time.Second), "", errQRExpired},
		{"tampered payload keeps its MAC", enc.EncodeToString(otherOrg) + "." + sig, qrNow, "", errQRMalformed},
		{"tampered payload text", flip(body) + "." + sig, qrNow, "", errQRMalformed},
		{"tampered MAC", body + "." + flip(sig), qrNow, "", errQRMalformed},
		{"truncated MAC", body + "." + sig[:len(sig)-2], qrNow, "", errQRMalformed},
		{"wrong secret", token, qrNow, "another-secret-another-secret-another-secret", errQRMalformed},
		{"empty", "", qrNow, "", errQRMalformed},
		{"no dot", body, qrNow, "", errQRMalformed},
		{"two dots", token + ".x", qrNow, "", errQRMalformed},
		{"payload not base64", "!!!." + sig, qrNow, "", errQRMalformed},
		{"MAC not base64", body + ".!!!", qrNow, "", errQRMalformed},
		{"padded base64", base64.URLEncoding.EncodeToString([]byte("x")) + "." + sig, qrNow, "", errQRMalformed},
		{"MAC with a newline inside", body + "." + sig[:10] + "\n" + sig[10:], qrNow, "", errQRMalformed},
		{"payload with a newline inside", body[:10] + "\r\n" + body[10:] + "." + sig, qrNow, "", errQRMalformed},
		{"MAC with spare bits set in its last character", body + "." + sig[:len(sig)-1] + spareBits(sig[len(sig)-1]), qrNow, "", errQRMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := tc.with
			if secret == "" {
				secret = qrTestSecret
			}
			_, err := qrTokensAt(t, secret, tc.at).Verify(tc.token)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// Payloads signed with the real key that are still not tokens: the signature proves who wrote them,
// not that they are well-formed, so every field is checked after it.
func TestQRTokenRefusesWellSignedNonsense(t *testing.T) {
	tokens := qrTokensAt(t, qrTestSecret, qrNow)
	base := qrPayload{V: qrVersion, Org: 7, Team: "1K-IBMI-S1", Danru: 42, Dir: DirectionIn, Iat: qrNow.Unix()}

	for name, mutate := range map[string]func(*qrPayload){
		"another version": func(p *qrPayload) { p.V = 2 },
		"version zero":    func(p *qrPayload) { p.V = 0 },
		"no team":         func(p *qrPayload) { p.Team = "" },
		"no direction":    func(p *qrPayload) { p.Dir = "" },
		"unknown dir":     func(p *qrPayload) { p.Dir = "SIDEWAYS" },
		"lowercase dir":   func(p *qrPayload) { p.Dir = "in" },
	} {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			_, err := tokens.Verify(signed(t, tokens, p))
			require.ErrorIs(t, err, errQRMalformed)
		})
	}

	t.Run("not JSON", func(t *testing.T) {
		enc := base64.RawURLEncoding
		raw := []byte("not json")
		_, err := tokens.Verify(enc.EncodeToString(raw) + "." + enc.EncodeToString(tokens.mac(raw)))
		require.ErrorIs(t, err, errQRMalformed)
	})
	t.Run("the well-formed one passes, so the mutations above are what failed", func(t *testing.T) {
		_, err := tokens.Verify(signed(t, tokens, base))
		require.NoError(t, err)
	})
}

func TestADirectionIsInOrOut(t *testing.T) {
	require.True(t, DirectionIn.Valid())
	require.True(t, DirectionOut.Valid())
	for _, d := range []Direction{"", "in", "out", "BOTH"} {
		require.False(t, d.Valid(), "%q", d)
	}
}

// An expired token still says what it was: the service compares the organization and the direction
// before it answers "expired".
func TestAnExpiredTokenStillCarriesItsClaims(t *testing.T) {
	issued, _ := qrTokensAt(t, qrTestSecret, qrNow).Issue(7, "1K-IBMI-S1", 42, DirectionOut)
	claims, err := qrTokensAt(t, qrTestSecret, qrNow.Add(time.Hour)).Verify(issued)
	require.ErrorIs(t, err, errQRExpired)
	require.Equal(t, QRClaims{OrganizationID: 7, TeamCode: "1K-IBMI-S1", DanruID: 42, Direction: DirectionOut, IssuedAt: qrNow}, claims)

	_, err = qrTokensAt(t, qrTestSecret, qrNow.Add(time.Hour)).Verify("garbage")
	require.ErrorIs(t, err, errQRMalformed)
}
