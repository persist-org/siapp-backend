package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// cheap keeps the unit tests fast; the production cost is checked once below.
var cheap = NewPasswords(PasswordParams{MemoryKiB: 8, Time: 1, Threads: 1})

func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := cheap.Hash("correct horse battery staple")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=8,t=1,p=1$"), hash)

	ok, err := cheap.Verify("correct horse battery staple", hash)
	require.NoError(t, err)
	require.True(t, ok)

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", " correct horse battery staple"} {
		ok, err := cheap.Verify(wrong, hash)
		require.NoError(t, err)
		require.False(t, ok, "%q must not verify", wrong)
	}
}

func TestHashUsesFreshSalt(t *testing.T) {
	a, err := cheap.Hash("same password")
	require.NoError(t, err)
	b, err := cheap.Hash("same password")
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestHashHandlesUnusualPasswords(t *testing.T) {
	for _, pw := range []string{"", "日本語のパスワード", strings.Repeat("x", 1000), "with\x00nul"} {
		hash, err := cheap.Hash(pw)
		require.NoError(t, err)
		ok, err := cheap.Verify(pw, hash)
		require.NoError(t, err)
		require.True(t, ok)
	}
}

func TestDefaultParamsAreTheDocumentedOnes(t *testing.T) {
	hash, err := NewPasswords(DefaultPasswordParams).Hash("x")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$"), hash) // 64 MiB, 3 passes, 2 lanes
}

func TestVerifyReadsParametersFromTheHash(t *testing.T) {
	old := NewPasswords(PasswordParams{MemoryKiB: 8, Time: 1, Threads: 1})
	hash, err := old.Hash("pw")
	require.NoError(t, err)

	// A hasher configured with other parameters still verifies the old hash.
	newer := NewPasswords(PasswordParams{MemoryKiB: 16, Time: 2, Threads: 2})
	ok, err := newer.Verify("pw", hash)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	valid, err := cheap.Hash("pw")
	require.NoError(t, err)
	parts := strings.Split(valid, "$")

	tests := map[string]string{
		"empty":             "",
		"plain text":        "password",
		"wrong algorithm":   strings.Replace(valid, "argon2id", "argon2i", 1),
		"wrong version":     strings.Replace(valid, "v=19", "v=16", 1),
		"too few parts":     strings.Join(parts[:5], "$"),
		"too many parts":    valid + "$extra",
		"bad params":        strings.Replace(valid, "m=8,t=1,p=1", "m=x,t=y,p=z", 1),
		"zero memory":       strings.Replace(valid, "m=8,", "m=0,", 1),
		"huge memory":       strings.Replace(valid, "m=8,", "m=999999999,", 1),
		"huge time":         strings.Replace(valid, "t=1,", "t=999,", 1),
		"zero threads":      strings.Replace(valid, "p=1", "p=0", 1),
		"salt not base64":   strings.Join([]string{"", "argon2id", "v=19", "m=8,t=1,p=1", "!!!", parts[5]}, "$"),
		"hash not base64":   strings.Join([]string{"", "argon2id", "v=19", "m=8,t=1,p=1", parts[4], "!!!"}, "$"),
		"empty hash":        strings.Join([]string{"", "argon2id", "v=19", "m=8,t=1,p=1", parts[4], ""}, "$"),
		"missing leading $": strings.TrimPrefix(valid, "$"),
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			ok, err := cheap.Verify("pw", encoded)
			require.ErrorIs(t, err, ErrInvalidHash)
			require.False(t, ok)
		})
	}
}
