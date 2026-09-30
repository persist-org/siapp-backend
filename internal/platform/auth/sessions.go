package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/platform/database"
)

// TokenPair is a fresh access token and refresh token.
type TokenPair struct {
	AccessToken          string
	AccessTokenExpiresAt time.Time
	RefreshToken         string
}

// RefreshInput is a refresh request with the metadata stored on the new session.
type RefreshInput struct {
	RefreshToken string
	UserAgent    string
	IP           string
}

// sessionTTL is how long a session lives from login or from each refresh.
func sessionTTL(rememberMe bool) time.Duration {
	if rememberMe {
		return sessionTTLRememberMe
	}
	return sessionTTLDefault
}

// Refresh exchanges a refresh token for a new token pair and retires the old token.
//
// The whole operation is one transaction, and it starts by locking the session row.
// ⚠ Why the lock: two requests carrying the same refresh token must not both succeed. With
// a plain read both could see "not revoked", and both would create a new session, leaving
// two live chains from one token. With SELECT ... FOR UPDATE the second request waits for
// the first to commit, then reads the row as revoked.
//
// ⚠ Reuse detection. A refresh token works exactly once: using it retires it and issues the
// next one. If a token that is already revoked is presented again, someone is replaying it.
// The usual cause is theft: a thief and the real user each hold the token, and whoever
// refreshes second is the replayer. We cannot tell which one is honest, so every session in
// the token's family is revoked and both must log in again. The client sees
// AUTH_REFRESH_REUSED.
//
// The trade-off is that an honest client that sends the same refresh twice, for instance a
// retry after a lost response, ends up logged out. For a security app that is the right side
// to err on.
func (s *Service) Refresh(ctx context.Context, in RefreshInput) (TokenPair, error) {
	now := s.clock.Now()
	var pair TokenPair
	reused := false
	var cutOff []string // sessions whose access tokens must die because of a reuse

	err := database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		old, err := q.GetSessionForRefresh(ctx, HashRefreshToken(in.RefreshToken))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRefreshTokenInvalid
		}
		if err != nil {
			return fmt.Errorf("get session for refresh: %w", err)
		}

		if old.RevokedAt.Valid {
			// ⚠ The family is revoked here and the error is returned only after the
			// transaction commits. Returning an error from this callback would roll the
			// revocation back, and the stolen chain would stay alive.
			//
			// Its access tokens are cut off too, not only its refresh tokens: a thief who
			// holds a still-valid access token must not get up to 15 more minutes.
			if cutOff, err = q.ListRecentSessionIDsByFamily(ctx, db.ListRecentSessionIDsByFamilyParams{
				FamilyID: old.FamilyID, Since: recentSince(now),
			}); err != nil {
				return fmt.Errorf("list recent sessions of family: %w", err)
			}
			if err := q.RevokeSessionFamily(ctx, db.RevokeSessionFamilyParams{
				FamilyID: old.FamilyID,
				Now:      sql.NullTime{Time: now, Valid: true},
			}); err != nil {
				return fmt.Errorf("revoke session family: %w", err)
			}
			reused = true
			return nil
		}
		if !old.ExpiresAt.After(now) || old.UserDeletedAt.Valid {
			return ErrRefreshTokenInvalid
		}
		if old.UserStatus == db.UsersStatusDISABLED {
			return ErrAccountDisabled
		}

		newID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate session id: %w", err)
		}
		refreshToken, refreshHash, err := NewRefreshToken()
		if err != nil {
			return err
		}

		// The new session keeps the family, so a later replay of any token in the chain can
		// revoke all of it, and keeps remember_me, so its lifetime rule does not change.
		// The lifetime restarts from now: a session stays alive as long as it is used.
		if err := q.CreateSession(ctx, db.CreateSessionParams{
			ID:               newID.String(),
			UserID:           old.UserID,
			FamilyID:         old.FamilyID,
			RefreshTokenHash: refreshHash,
			RememberMe:       old.RememberMe,
			ExpiresAt:        now.Add(sessionTTL(old.RememberMe)),
			UserAgent:        nullString(truncate(in.UserAgent, maxUserAgentLen)),
			Ip:               nullString(in.IP),
			LastUsedAt:       sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		if err := q.RotateSession(ctx, db.RotateSessionParams{
			ID:           old.ID,
			Now:          sql.NullTime{Time: now, Valid: true},
			ReplacedByID: sql.NullString{String: newID.String(), Valid: true},
		}); err != nil {
			return fmt.Errorf("rotate session: %w", err)
		}

		// The role comes from the database, so a role change reaches the user at the next
		// refresh instead of waiting for a new login.
		accessToken, expiresAt, err := s.tokens.NewAccessToken(old.UserID, old.SystemRole, newID.String())
		if err != nil {
			return err
		}
		pair = TokenPair{AccessToken: accessToken, AccessTokenExpiresAt: expiresAt, RefreshToken: refreshToken}
		return nil
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reused {
		s.revocations.Revoke(ctx, cutOff...)
		return TokenPair{}, ErrRefreshReused
	}
	return pair, nil
}

// Logout ends one session: its refresh token stops working now, and so do the access tokens
// of its login chain, through Revocations. The user ID condition in the queries means a
// token can only end its own user's sessions.
//
// Doing it twice with the same access token fails with 401, because the first call already
// cut that token off. The database part is still safe to repeat.
func (s *Service) Logout(ctx context.Context, userID, sessionID string) error {
	now := s.clock.Now()
	err := s.q.RevokeSession(ctx, db.RevokeSessionParams{
		ID:     sessionID,
		UserID: userID,
		Now:    sql.NullTime{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}

	// The whole chain, not just this session: an access token from the refresh just before
	// this one is still valid for a few minutes and belongs to the same device.
	chain, err := s.q.ListRecentSessionIDsOfSessionFamily(ctx, db.ListRecentSessionIDsOfSessionFamilyParams{
		SessionID: sessionID, UserID: userID, Since: recentSince(now),
	})
	if err != nil {
		return fmt.Errorf("list recent sessions of the login chain: %w", err)
	}
	// The caller's own session is always listed, even if the query found nothing, so the
	// token that made this request is cut off.
	s.revocations.Revoke(ctx, append(chain, sessionID)...)
	return nil
}

// LogoutAll ends every session of the user, on every device, and cuts off their access
// tokens. It returns how many sessions were still active; sessions that were already ended
// are not counted. Anything that has to end all of a user's sessions, such as disabling
// the account, should call this.
func (s *Service) LogoutAll(ctx context.Context, userID string) (int64, error) {
	now := s.clock.Now()
	var recent []string
	var ended int64

	err := database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		var err error
		recent, ended, err = revokeAllSessions(ctx, q, userID, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.revocations.Revoke(ctx, recent...)
	return ended, nil
}

// revokeAllSessions ends every session of the user inside the caller's transaction. It
// returns the IDs whose access tokens can still be alive, so that the caller can put them on
// the revocation list once the transaction has committed, and how many sessions were active.
// Account disabling and password reset call it in the same transaction as their own change.
//
// The read locks its range, so a login that races with this call cannot insert a session
// between the read and the revoke and escape the list.
func revokeAllSessions(ctx context.Context, q *db.Queries, userID string, now time.Time) (recent []string, ended int64, err error) {
	if recent, err = q.ListRecentSessionIDsByUser(ctx, db.ListRecentSessionIDsByUserParams{
		UserID: userID, Since: recentSince(now),
	}); err != nil {
		return nil, 0, fmt.Errorf("list recent sessions: %w", err)
	}
	if ended, err = q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{
		UserID: userID,
		Now:    sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		return nil, 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return recent, ended, nil
}
