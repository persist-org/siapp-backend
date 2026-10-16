// Package identity signs people in with their Persist Personalia account and keeps the
// credential-free cache of the people SiAPP works with (module 01).
package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
)

// personelAuth is the part of the Personel client login needs (seam S1).
type personelAuth interface {
	Login(ctx context.Context, in personel.LoginInput) (personel.Identity, error)
}

// Usecase holds the identity rules.
type Usecase struct {
	pool     *pgxpool.Pool
	personel personelAuth
	tokens   *auth.Tokens
	clock    clock.Clock
	repo     repo
}

// NewUsecase returns the identity Usecase.
func NewUsecase(pool *pgxpool.Pool, p personelAuth, tokens *auth.Tokens, c clock.Clock) *Usecase {
	return &Usecase{pool: pool, personel: p, tokens: tokens, clock: c}
}

// Login is the single path that starts a session.
//
// ⚠ The order of the steps is the design:
//  1. Personalia checks the password. Nothing is written before it has answered, so a
//     wrong password costs nothing and changes nothing.
//  2. The organization row is upserted before any tenant insert, because every tenant
//     table has a foreign key to it. A suspended organization stops here.
//  3. In ONE tenant transaction: upsert the person, apply the phone rule, create the
//     session. If the phone is bound elsewhere the whole transaction rolls back, so a
//     refused login leaves no session, and no half-refreshed person, behind.
//
// ⚠ SiAPP never stores, logs or hashes the password: it is passed to Personalia over TLS
// and goes nowhere else. People sign in to SiAPP's own login screen with their Personalia
// account (manual §2.1, D3); the only secret the two systems share is the integration key.
//
// ⚠ The device rule. A person may have only one bound phone, and device_bindings is the
// single record of it. A lost or replaced phone is freed by an admin through SiAPP's own
// reset (Phase 01.3). The binding is created only after Personalia has confirmed the
// password and an active employee, so a guesser cannot claim a phone slot. WEB logins bind
// nothing (a browser has no stable device id); Phase 01.3 restricts web login to people
// with a web role, so field staff cannot use the web to get around the one-phone rule.
func (u *Usecase) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	id, err := u.personel.Login(ctx, personel.LoginInput{
		Email: in.Email, Password: in.Password, ClientIP: in.IP, UserAgent: in.UserAgent,
	})
	if err != nil {
		return LoginResult{}, mapPersonelError(err)
	}
	// Personalia refuses inactive people itself; this is the second lock on the same door.
	if !id.Active {
		return LoginResult{}, ErrAccountDisabled
	}
	// Both domains ('personalia' and 'siapp') may sign in to SiAPP, so id.Domain is not
	// checked. The organization is the person's employing company: it comes from this
	// answer and never from the request (D11).
	if id.OrganizationID <= 0 {
		return LoginResult{}, fmt.Errorf("personel login: answer without an organization for user %d", id.PersonelUserID)
	}

	now := u.clock.Now()
	var org db.Organization
	if err := database.WithoutTenant(ctx, u.pool, func(q *db.Queries) error {
		var err error
		org, err = u.repo.upsertOrganization(ctx, q, id.OrganizationID, id.OrganizationName)
		return err
	}); err != nil {
		return LoginResult{}, err
	}
	if org.Status == db.OrganizationStatusSUSPENDED {
		return LoginResult{}, ErrOrganizationSuspended
	}

	var out LoginResult
	err = database.WithTenant(ctx, u.pool, id.OrganizationID, func(q *db.Queries) error {
		person, err := u.repo.upsertPersonnel(ctx, q, id, now)
		if err != nil {
			return err
		}

		client := db.SessionClientWEB
		var deviceHash string
		if in.Client == ClientMobile {
			client = db.SessionClientMOBILE
			deviceHash = auth.HashToken(in.DeviceToken)
			// ⚠ Two logins of one person must not both bind a phone. The upsert above holds
			// the person's row lock until this transaction ends, so a second login of the
			// same person waits there and then sees the first one's binding.
			if err := u.bindDevice(ctx, q, person.ID, deviceHash, now); err != nil {
				return err
			}
		}

		family, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate session family: %w", err)
		}
		refreshToken, refreshHash, err := auth.NewRefreshToken(id.OrganizationID)
		if err != nil {
			return err
		}
		sess, err := u.repo.createSession(ctx, q, newSession{
			personnelID:      person.ID,
			familyID:         family,
			refreshTokenHash: refreshHash,
			deviceTokenHash:  deviceHash,
			rememberMe:       in.RememberMe,
			client:           client,
			expiresAt:        now.Add(sessionTTL(in.RememberMe)),
			userAgent:        truncate(in.UserAgent, maxUserAgentLen),
			ip:               truncate(in.IP, maxIPLen),
			now:              now,
		})
		if err != nil {
			return err
		}
		accessToken, _, err := u.tokens.NewAccessToken(person.ID, person.OrganizationID, sess.ID)
		if err != nil {
			return err
		}
		out = LoginResult{AccessToken: accessToken, RefreshToken: refreshToken, Person: personFrom(person)}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	return out, nil
}

// bindDevice applies the one-phone rule inside the login transaction: no binding yet binds
// this phone, the same phone is noted as seen, and any other phone is refused.
func (u *Usecase) bindDevice(ctx context.Context, q *db.Queries, personnelID int64, deviceHash string, now time.Time) error {
	b, found, err := u.repo.deviceBinding(ctx, q, personnelID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return u.repo.createDeviceBinding(ctx, q, personnelID, deviceHash, now)
	case subtle.ConstantTimeCompare([]byte(b.DeviceTokenHash), []byte(deviceHash)) == 1:
		return u.repo.touchDeviceBinding(ctx, q, b.ID, now)
	default:
		return ErrDeviceBound
	}
}

// mapPersonelError turns what the Personel client says into what the API answers.
func mapPersonelError(err error) error {
	switch {
	case errors.Is(err, personel.ErrInvalidCredentials):
		return ErrInvalidCredentials
	case errors.Is(err, personel.ErrAccountDisabled):
		return ErrAccountDisabled
	case errors.Is(err, personel.ErrRateLimited):
		return apperr.ErrRateLimited
	case errors.Is(err, personel.ErrUnavailable):
		return apperr.ErrPersonelUnavailable
	default:
		return fmt.Errorf("personel login: %w", err)
	}
}
