// Package auth handles credentials, tokens and login: password hashing, JWT access tokens,
// refresh tokens, the default-deny authentication middleware and the login endpoint.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// PasswordParams are the argon2id cost parameters.
type PasswordParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultPasswordParams follow the RFC 9106 "second recommended" profile family: 64 MiB of
// memory, 3 passes, 2 lanes. A hash takes on the order of 100 ms.
var DefaultPasswordParams = PasswordParams{MemoryKiB: 64 * 1024, Time: 3, Threads: 2}

const (
	argonKeyLen  = 32
	argonSaltLen = 16

	// Upper bounds accepted when reading parameters back from a stored hash, so a corrupt
	// or hostile row cannot make Verify allocate gigabytes.
	maxStoredMemoryKiB = 256 * 1024
	maxStoredTime      = 10
	maxStoredThreads   = 16
)

// ErrInvalidHash means a stored password hash is not a valid argon2id PHC string.
var ErrInvalidHash = errors.New("invalid argon2id hash")

// Passwords hashes and verifies passwords with argon2id.
type Passwords struct{ params PasswordParams }

// NewPasswords returns a Passwords that hashes with the given cost. Production uses
// DefaultPasswordParams; tests use a cheap setting to stay fast.
func NewPasswords(p PasswordParams) *Passwords { return &Passwords{params: p} }

// Hash returns the PHC string "$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>" for password,
// with a fresh random salt. The parameters travel inside the string, so raising the cost
// later does not invalidate existing hashes.
func (p *Passwords) Hash(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.params.Time, p.params.MemoryKiB, p.params.Threads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.params.MemoryKiB, p.params.Time, p.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify reports whether password matches the encoded hash. It uses the parameters stored
// in the hash, not p's, so old hashes keep verifying after the cost is changed.
//
// ⚠ The final comparison is constant-time (subtle.ConstantTimeCompare): a plain == or
// bytes.Equal returns at the first differing byte, and that timing difference can leak the
// hash byte by byte to an attacker who can measure many responses.
func (p *Passwords) Verify(password, encoded string) (bool, error) {
	params, salt, want, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Threads, uint32(len(want))) //nolint:gosec // length is bounded by the stored hash
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func parseHash(encoded string) (params PasswordParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return params, nil, nil, ErrInvalidHash
	}
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.MemoryKiB, &params.Time, &threads); err != nil {
		return params, nil, nil, ErrInvalidHash
	}
	params.Threads = threads
	if params.MemoryKiB == 0 || params.MemoryKiB > maxStoredMemoryKiB ||
		params.Time == 0 || params.Time > maxStoredTime ||
		params.Threads == 0 || params.Threads > maxStoredThreads {
		return params, nil, nil, ErrInvalidHash
	}

	if salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return params, nil, nil, ErrInvalidHash
	}
	if key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5]); err != nil || len(key) == 0 {
		return params, nil, nil, ErrInvalidHash
	}
	return params, salt, key, nil
}
