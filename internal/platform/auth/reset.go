package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/mail"
	"siapp/internal/platform/audit"
	"siapp/internal/platform/database"
)

// RequestPasswordReset emails a reset link to the user with this address, if there is an
// ACTIVE one. It returns nil either way: the caller answers 202 in both cases, so the
// response must not reveal whether an account exists.
//
// Only the newest link works: asking again invalidates the earlier one. The link carries a
// random token that only its SHA-256 hash is stored for, and it is sent by email only, never
// returned or logged.
//
// A failed delivery is logged and not returned, for the same reason. In production the mailer
// is mail.Async, which also makes a known and an unknown address take the same time.
func (s *Service) RequestPasswordReset(ctx context.Context, email string) error {
	user, err := s.q.GetActiveUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get user by email: %w", err)
	}

	token, hash, err := NewRefreshToken() // 32 random bytes and their hash: same scheme
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate reset token id: %w", err)
	}
	now := s.clock.Now()

	err = database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		if err := q.InvalidateUnusedResetTokens(ctx, db.InvalidateUnusedResetTokensParams{
			UserID: user.ID, Now: sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("invalidate earlier reset tokens: %w", err)
		}
		if err := q.CreateResetToken(ctx, db.CreateResetTokenParams{
			ID: id.String(), UserID: user.ID, TokenHash: hash, ExpiresAt: now.Add(passwordResetTTL),
		}); err != nil {
			return fmt.Errorf("create reset token: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Side effects run after the commit, so the link is never sent for a token that was not stored.
	link, err := s.resetLink(token)
	if err != nil {
		return err
	}
	msg, err := mail.NewPasswordResetMessage(user.Email, user.FullName, link, passwordResetTTL)
	if err != nil {
		return err
	}
	if err := s.mailer.Send(ctx, msg); err != nil {
		s.logger.ErrorContext(ctx, "sending password reset email failed", "userId", user.ID, "error", err)
	}
	return nil
}

// resetLink builds the URL the email points to: the client app's reset page with the token.
func (s *Service) resetLink(token string) (string, error) {
	u, err := url.Parse(s.resetURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("APP_RESET_URL %q is not an absolute URL", s.resetURL)
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ResetPassword sets a new password with a reset link's token, and ends every session.
//
// ⚠ The token row is locked (FOR UPDATE) before anything is decided, so two requests with
// the same link cannot both succeed: the second waits, then finds it used.
//
// The order of the checks matters. The token is judged first, so a bad link gets
// AUTH_RESET_INVALID whatever the new password was. Only then is the password checked; a weak
// one is refused inside the same transaction, which rolls back, so the link is not used up
// and the user can try again with a better password.
//
// On success the password is replaced, the lockout is cleared, the link and any other unused
// links are retired, every session is ended and its access tokens are cut off. Whoever knew
// the old password, or held a session, is locked out.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	now := s.clock.Now()
	var recent []string

	err := database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		row, err := q.GetResetTokenForUpdate(ctx, HashRefreshToken(token))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrResetInvalid
		}
		if err != nil {
			return fmt.Errorf("get reset token: %w", err)
		}
		// Unknown, used, expired, and a user who is disabled or deleted all look the same
		// to the caller.
		if row.UsedAt.Valid || !row.ExpiresAt.After(now) || row.UserDeletedAt.Valid || row.UserStatus != db.UsersStatusACTIVE {
			return ErrResetInvalid
		}

		if err := ValidatePassword(newPassword, PasswordSubject{Email: row.Email, Username: row.Username.String}); err != nil {
			return err
		}
		hash, err := s.passwords.Hash(newPassword)
		if err != nil {
			return err
		}

		if err := q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{ID: row.UserID, PasswordHash: hash}); err != nil {
			return fmt.Errorf("update password: %w", err)
		}
		nowValue := sql.NullTime{Time: now, Valid: true}
		if err := q.MarkResetTokenUsed(ctx, db.MarkResetTokenUsedParams{ID: row.ID, Now: nowValue}); err != nil {
			return fmt.Errorf("mark reset token used: %w", err)
		}
		if err := q.InvalidateUnusedResetTokens(ctx, db.InvalidateUnusedResetTokensParams{UserID: row.UserID, Now: nowValue}); err != nil {
			return fmt.Errorf("invalidate other reset tokens: %w", err)
		}
		if recent, _, err = revokeAllSessions(ctx, q, row.UserID, now); err != nil {
			return err
		}
		// No password, hash or token in the audit row. The actor is the user themselves.
		return audit.Record(ctx, q, audit.Entry{
			ActorID: row.UserID, Action: "user.password.reset", EntityType: "user", EntityID: row.UserID, At: now,
		})
	})
	if err != nil {
		return err
	}
	s.revocations.Revoke(ctx, recent...)
	return nil
}
