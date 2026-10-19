// Package auth holds SiAPP's own tokens and the default-deny authentication middleware.
//
// SiAPP never verifies a Personalia token: a person proves their Personalia password once,
// at login, and from then on carries a token that SiAPP itself signed. Who may log in is
// Personalia's decision (module 01); what a token means is decided here.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"siapp/pkg/platform/clock"
)

const (
	// AccessTokenTTL is how long an access token lives. It is short because a signed token
	// cannot be taken back: ending a session cuts it off earlier through the revocation
	// list (Phase 01.2), but only this bounds the damage if that list is unavailable.
	AccessTokenTTL = 15 * time.Minute

	refreshTokenBytes = 32
	// refreshRandomLen is the length of 32 bytes in unpadded base64url.
	refreshRandomLen = 43

	// MinSecretBytes is the shortest JWT secret NewTokens accepts: HS256 is only as strong
	// as its key, and the key is a deployment setting that is easy to leave at an example.
	MinSecretBytes = 32
)

// ErrInvalidToken means an access token is malformed, wrongly signed, or lacks the claims
// SiAPP issues. Callers do not need to tell these apart.
var ErrInvalidToken = errors.New("auth: invalid access token")

// ErrExpiredToken means an access token is genuine but past its expiry. It is kept apart
// from ErrInvalidToken because the client reacts differently: it refreshes instead of
// sending the person back to the login screen.
var ErrExpiredToken = errors.New("auth: access token expired")

// ErrInvalidRefreshToken means a refresh token does not have the shape SiAPP issues.
var ErrInvalidRefreshToken = errors.New("auth: invalid refresh token")

// Claims are the facts an access token carries. Nothing from Personalia is in a token: no
// NIK, no name, no role.
type Claims struct {
	PersonnelID    int64
	OrganizationID int64
	SessionID      int64
}

// Tokens issues and verifies access tokens.
type Tokens struct {
	secret []byte
	clock  clock.Clock
}

// NewTokens returns a Tokens signing with secret (HS256). The clock decides both the issue
// time and whether a token has expired.
func NewTokens(secret []byte, c clock.Clock) (*Tokens, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("auth: JWT secret must be at least %d bytes", MinSecretBytes)
	}
	return &Tokens{secret: secret, clock: c}, nil
}

type accessClaims struct {
	// OrganizationID uses the contract's claim name (docs/api-response.md §8).
	OrganizationID int64  `json:"organization_id"`
	SessionID      string `json:"sid"`
	jwt.RegisteredClaims
}

// NewAccessToken signs a token for one session and returns it with its expiry. The subject
// is the local personnels.id, not anything Personalia owns.
func (t *Tokens) NewAccessToken(personnelID, organizationID, sessionID int64) (string, time.Time, error) {
	now := t.clock.Now()
	expiresAt := now.Add(AccessTokenTTL)
	claims := accessClaims{
		OrganizationID: organizationID,
		SessionID:      strconv.FormatInt(sessionID, 10),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   strconv.FormatInt(personnelID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken verifies the signature and expiry of token and returns its claims. A
// token past its expiry returns ErrExpiredToken; every other failure ErrInvalidToken.
//
// ⚠ Only HS256 is accepted. Without WithValidMethods a token could pick its own algorithm,
// including "none" (no signature at all), which is the classic JWT forgery.
func (t *Tokens) ParseAccessToken(token string) (Claims, error) {
	var parsed accessClaims
	_, err := jwt.ParseWithClaims(token, &parsed,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(t.clock.Now),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Claims{}, fmt.Errorf("%w: %w", ErrExpiredToken, err)
		}
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	personnelID, err1 := strconv.ParseInt(parsed.Subject, 10, 64)
	sessionID, err2 := strconv.ParseInt(parsed.SessionID, 10, 64)
	if err1 != nil || err2 != nil || personnelID <= 0 || sessionID <= 0 || parsed.OrganizationID <= 0 {
		return Claims{}, fmt.Errorf("%w: missing or malformed claims", ErrInvalidToken)
	}
	return Claims{PersonnelID: personnelID, OrganizationID: parsed.OrganizationID, SessionID: sessionID}, nil
}

// NewRefreshToken returns an opaque refresh token for a person of organizationID and the
// hash to store: "{orgID}.{32 random bytes, base64url}".
//
// ⚠ Why the organization id is in the token: refresh runs before any tenant is known, and
// the prefix lets the lookup run inside WithTenant for that organization instead of
// scanning across tenants. The prefix is not trusted on its own: the hash must match a row
// inside that tenant, so a forged prefix finds nothing.
func NewRefreshToken(organizationID int64) (token, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	token = strconv.FormatInt(organizationID, 10) + "." + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// ParseRefreshToken returns the organization id a refresh token names, or
// ErrInvalidRefreshToken if the token is not shaped like one SiAPP issues. It checks the
// shape only; whether the token exists is the database's answer.
func ParseRefreshToken(token string) (organizationID int64, err error) {
	prefix, random, found := strings.Cut(token, ".")
	if !found || len(random) != refreshRandomLen {
		return 0, ErrInvalidRefreshToken
	}
	organizationID, perr := strconv.ParseInt(prefix, 10, 64)
	if perr != nil || organizationID <= 0 || strconv.FormatInt(organizationID, 10) != prefix {
		return 0, ErrInvalidRefreshToken
	}
	if raw, derr := base64.RawURLEncoding.DecodeString(random); derr != nil || len(raw) != refreshTokenBytes {
		return 0, ErrInvalidRefreshToken
	}
	return organizationID, nil
}

// HashToken returns the hex SHA-256 of a refresh token or device token, the form stored in
// the database. A fast hash is right here: refresh tokens are 256 bits of randomness and
// device tokens are machine-generated identifiers, so there is nothing to brute-force,
// unlike a human-chosen password.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
