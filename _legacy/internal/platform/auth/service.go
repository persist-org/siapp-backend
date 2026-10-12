package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/mail"
	"siapp/internal/platform/clock"
	"siapp/internal/platform/database"
)

// Service holds the authentication rules.
type Service struct {
	pool      *sql.DB
	q         *db.Queries
	passwords *Passwords
	tokens    *Tokens
	clock     clock.Clock
	// revocations cuts off the access tokens of sessions that are ended.
	revocations *Revocations
	mailer      mail.Sender
	resetURL    string
	logger      *slog.Logger

	dummyOnce sync.Once
	dummyHash string
}

// ServiceConfig is what the Service is built from.
type ServiceConfig struct {
	Pool        *sql.DB
	Passwords   *Passwords
	Tokens      *Tokens
	Clock       clock.Clock
	Revocations *Revocations
	// Mailer sends the password reset email. In production it is an mail.Async around the
	// SMTP sender, see mail.Async for why.
	Mailer mail.Sender
	// ResetURL is the page of the client app that takes a reset token, e.g.
	// "https://app.example.com/reset-password". The token is added as ?token=...
	ResetURL string
	Logger   *slog.Logger
}

// NewService returns the auth Service.
func NewService(cfg ServiceConfig) *Service {
	return &Service{
		pool: cfg.Pool, q: db.New(cfg.Pool), passwords: cfg.Passwords, tokens: cfg.Tokens,
		clock: cfg.Clock, revocations: cfg.Revocations, mailer: cfg.Mailer,
		resetURL: cfg.ResetURL, logger: cfg.Logger,
	}
}

// recentSince is the earliest creation time of a session whose access token can still be
// alive at now. See ListRecentSessionIDsByUser.
func recentSince(now time.Time) sql.NullTime {
	return sql.NullTime{Time: now.Add(-revocationTTL), Valid: true}
}

// LoginInput is a login attempt with the request metadata stored on the session.
type LoginInput struct {
	Identifier string
	Password   string
	RememberMe bool
	UserAgent  string
	IP         string
}

// LoginResult is what a successful login hands back.
type LoginResult struct {
	AccessToken          string
	AccessTokenExpiresAt time.Time
	RefreshToken         string
	User                 db.User
}

// Login checks the credentials and opens a session.
//
// ⚠ Order of the checks, and why:
//  1. Unknown user and wrong password return the same ErrInvalidCredentials, so a caller
//     cannot learn which emails or usernames have accounts. For an unknown user a password
//     hash is still computed against a dummy hash, so the response time does not give the
//     answer away either.
//  2. A locked account is refused before the password is checked, so guessing during the
//     lock is pointless and does not extend it.
//  3. DISABLED is reported only after the correct password. Otherwise anyone could probe
//     which accounts exist and are disabled without knowing a password.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	user, err := s.q.GetUserByIdentifier(ctx, db.GetUserByIdentifierParams{
		Email:    normalizeIdentifier(in.Identifier),
		Username: sql.NullString{String: strings.TrimSpace(in.Identifier), Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		s.spendPasswordTime(in.Password)
		return LoginResult{}, ErrInvalidCredentials
	}
	if err != nil {
		return LoginResult{}, fmt.Errorf("get user by identifier: %w", err)
	}

	now := s.clock.Now()
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(now) {
		return LoginResult{}, ErrAccountLocked
	}

	ok, err := s.passwords.Verify(in.Password, user.PasswordHash)
	if err != nil {
		return LoginResult{}, fmt.Errorf("verify password of user %s: %w", user.ID, err)
	}
	if !ok {
		// The counter is not reset when the lock triggers, so the first wrong guess after
		// a lock expires locks the account again straight away.
		err := s.q.RecordFailedLogin(ctx, db.RecordFailedLoginParams{
			ID:          user.ID,
			MaxFailures: maxFailedLogins,
			LockedUntil: now.Add(lockoutDuration),
		})
		if err != nil {
			return LoginResult{}, fmt.Errorf("record failed login: %w", err)
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if user.Status == db.UsersStatusDISABLED {
		return LoginResult{}, ErrAccountDisabled
	}

	return s.openSession(ctx, user, in, now)
}

func (s *Service) openSession(ctx context.Context, user db.User, in LoginInput, now time.Time) (LoginResult, error) {
	sessionID, err := uuid.NewV7()
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate session id: %w", err)
	}
	familyID, err := uuid.NewV7()
	if err != nil {
		return LoginResult{}, fmt.Errorf("generate session family id: %w", err)
	}
	refreshToken, refreshHash, err := NewRefreshToken()
	if err != nil {
		return LoginResult{}, err
	}

	ttl := sessionTTL(in.RememberMe)

	err = database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		if err := q.RecordSuccessfulLogin(ctx, db.RecordSuccessfulLoginParams{
			ID:  user.ID,
			Now: sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("record successful login: %w", err)
		}
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			ID:               sessionID.String(),
			UserID:           user.ID,
			FamilyID:         familyID.String(),
			RefreshTokenHash: refreshHash,
			RememberMe:       in.RememberMe,
			ExpiresAt:        now.Add(ttl),
			UserAgent:        nullString(truncate(in.UserAgent, maxUserAgentLen)),
			Ip:               nullString(in.IP),
			LastUsedAt:       sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}

	accessToken, expiresAt, err := s.tokens.NewAccessToken(user.ID, user.SystemRole, sessionID.String())
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		AccessToken:          accessToken,
		AccessTokenExpiresAt: expiresAt,
		RefreshToken:         refreshToken,
		User:                 user,
	}, nil
}

// spendPasswordTime runs one password verification against a dummy hash of the same cost
// as real ones, so an unknown user takes as long to reject as a wrong password.
func (s *Service) spendPasswordTime(password string) {
	s.dummyOnce.Do(func() {
		// Hashing can only fail if the system random source does; an empty hash then
		// makes Verify return quickly, which is harmless.
		s.dummyHash, _ = s.passwords.Hash(dummyPassword)
	})
	_, _ = s.passwords.Verify(password, s.dummyHash)
}

// normalizeIdentifier lowercases emails, which are stored lowercase. A username has no '@'
// and keeps its case (the column collation compares case-insensitively anyway).
func normalizeIdentifier(identifier string) string {
	identifier = strings.TrimSpace(identifier)
	if strings.Contains(identifier, "@") {
		return strings.ToLower(identifier)
	}
	return identifier
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Cut on a rune boundary so a multi-byte character is never split.
	for maxBytes > 0 && !isRuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
