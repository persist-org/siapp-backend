package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"siapp/internal/db"
	"siapp/internal/platform/clock"
)

// ErrInvalidToken means an access token is malformed, expired, wrongly signed or lacks
// the claims we issue. Callers do not need to tell these apart.
var ErrInvalidToken = errors.New("invalid access token")

// Claims are the facts an access token carries.
type Claims struct {
	UserID     string
	SystemRole db.UsersSystemRole
	SessionID  string
}

// Tokens issues and verifies access JWTs.
type Tokens struct {
	secret []byte
	clock  clock.Clock
}

// NewTokens returns a Tokens signing with secret (HS256). The clock decides both the
// issue time and whether a token has expired.
func NewTokens(secret []byte, c clock.Clock) *Tokens {
	return &Tokens{secret: secret, clock: c}
}

type accessClaims struct {
	Role      string `json:"role"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

// NewAccessToken signs a token for the user's session and returns it with its expiry.
func (t *Tokens) NewAccessToken(userID string, role db.UsersSystemRole, sessionID string) (string, time.Time, error) {
	now := t.clock.Now()
	expiresAt := now.Add(accessTokenTTL)
	claims := accessClaims{
		Role:      string(role),
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
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

// ParseAccessToken verifies the signature and expiry of token and returns its claims.
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
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}

	role := db.UsersSystemRole(parsed.Role)
	if parsed.Subject == "" || parsed.SessionID == "" || !validRole(role) {
		return Claims{}, fmt.Errorf("%w: missing or unknown claims", ErrInvalidToken)
	}
	return Claims{UserID: parsed.Subject, SystemRole: role, SessionID: parsed.SessionID}, nil
}

func validRole(r db.UsersSystemRole) bool {
	return r == db.UsersSystemRoleADMIN || r == db.UsersSystemRoleMEMBER
}

// NewRefreshToken returns a random opaque refresh token (32 bytes, base64url) and the hash
// to store. Only the hash goes into the database, so a leaked database cannot be used to
// refresh sessions.
func NewRefreshToken() (token, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token), nil
}

// HashRefreshToken returns the hex SHA-256 of a refresh token, the form stored in
// sessions.refresh_token_hash. A fast hash is right here: the token is 256 bits of
// randomness, so there is nothing to brute-force, unlike a human-chosen password.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
