package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
)

// RefreshInput is a refresh request. The json tags carry the contract's field names.
type RefreshInput struct {
	RefreshToken string `json:"refresh_token" validate:"required,max=128"`

	IP        string `json:"-"`
	UserAgent string `json:"-"`
}

// resyncState is what asking Personalia about a person came to.
type resyncState int

const (
	// resyncSkipped: the cached copy is fresh enough, the person has no NIK to look up, or
	// Personalia could not answer. The cached status is kept.
	resyncSkipped resyncState = iota
	// resyncFresh: Personalia confirms the person is active in this organization.
	resyncFresh
	// resyncGone: Personalia says the person is inactive, gone, or employed elsewhere now.
	resyncGone
)

type resync struct {
	state    resyncState
	identity personel.Identity
}

// Refresh exchanges a refresh token for a new token pair and retires the old token.
//
// ⚠ A refresh token works exactly once: using it retires it and issues the next one, in
// the same login chain (family_id). The order of the checks under the lock is:
//  1. unknown token (or another organization's prefix, which RLS hides): AUTH_TOKEN_INVALID;
//  2. an already retired token: reuse, the whole chain ends (AUTH_REFRESH_REUSED);
//  3. an expired session: AUTH_TOKEN_INVALID;
//  4. the person is INACTIVE, or Personalia says so now: AUTH_ACCOUNT_DISABLED;
//  5. a MOBILE session whose phone is no longer the bound one: AUTH_DEVICE_CHANGED.
//
// ⚠ Reuse detection. The usual cause of a replayed token is theft: a thief and the real
// person each hold it, and whoever refreshes second is the replayer. SiAPP cannot tell which
// one is honest, so the whole chain is revoked and both must log in again. The trade-off is
// that an honest client that sends the same refresh twice, for instance a retry after a lost
// response, ends up logged out. For a security app that is the right side to err on.
//
// ⚠ Every refusal that ends a chain (reuse, disabled, device changed) COMMITS the revocation
// first and returns its error afterwards. Returning the error from inside the transaction
// would roll the revocation back, and the chain would stay alive.
//
// ⚠ The device check (MOBILE sessions, on every refresh) is a local read: the person's
// device_bindings row must exist and its hash must equal the session's device_token_hash.
// A reset binding, or another phone bound since, ends the chain. It never calls Personalia,
// so it still works while Personalia is down, and an admin's device reset (SiAPP's own data,
// Phase 01.3) takes effect at the next refresh.
//
// ⚠ Re-validation with Personalia. When the cached person is older than personelResyncAfter,
// Personalia is asked before new tokens are issued (and, so that no row lock is held during
// an HTTP call, before the transaction that rotates the session). If it says the person is
// inactive, gone, or employed by another company, the refresh is refused. If Personalia is
// unavailable the refresh is allowed and the cached status kept: an outage of Personalia
// must not log the whole field force out. The trade-off: a person deactivated in Personalia
// keeps access for at most 12 hours plus one access-token lifetime, unless the Personalia
// webhook (Phase 05.3) arrives sooner.
func (u *Usecase) Refresh(ctx context.Context, in RefreshInput) (LoginResult, error) {
	organizationID, err := auth.ParseRefreshToken(in.RefreshToken)
	if err != nil {
		return LoginResult{}, ErrRefreshTokenInvalid
	}
	hash := auth.HashToken(in.RefreshToken)
	now := u.clock.Now()

	sync, err := u.revalidate(ctx, organizationID, hash, now)
	if err != nil {
		return LoginResult{}, err
	}

	var (
		out     LoginResult
		cutOff  []int64 // sessions whose access tokens die because the chain ended
		refused error   // returned after the transaction has committed
	)
	endChain := func(q *db.Queries, family pgtype.UUID, why error) error {
		ids, err := u.repo.endFamily(ctx, q, family, now)
		if err != nil {
			return err
		}
		cutOff, refused = ids, why
		return nil
	}

	err = database.WithTenant(ctx, u.pool, organizationID, func(q *db.Queries) error {
		row, found, err := u.repo.sessionForRefresh(ctx, q, hash)
		if err != nil {
			return err
		}
		if !found {
			return ErrRefreshTokenInvalid
		}
		if row.RevokedAt.Valid {
			return endChain(q, row.FamilyID, ErrRefreshReused)
		}
		if !row.ExpiresAt.Time.After(now) {
			return ErrRefreshTokenInvalid
		}
		if row.PersonnelStatus == db.PersonnelStatusINACTIVE {
			return endChain(q, row.FamilyID, ErrAccountDisabled)
		}
		if sync.state == resyncGone {
			if err := u.repo.markInactive(ctx, q, row.PersonnelID, now); err != nil {
				return err
			}
			return endChain(q, row.FamilyID, ErrAccountDisabled)
		}
		if row.Client == db.SessionClientMOBILE {
			ok, err := u.deviceStillBound(ctx, q, row, now)
			if err != nil {
				return err
			}
			if !ok {
				return endChain(q, row.FamilyID, ErrDeviceChanged)
			}
		}

		var person db.Personnel
		if sync.state == resyncFresh {
			person, err = u.repo.upsertPersonnel(ctx, q, sync.identity, now)
		} else {
			person, err = u.repo.getPersonnel(ctx, q, row.PersonnelID)
		}
		if err != nil {
			return err
		}

		// The new session keeps the chain, the remember-me choice, the client and the phone,
		// and its lifetime restarts from now: a session stays alive as long as it is used.
		refreshToken, refreshHash, err := auth.NewRefreshToken(organizationID)
		if err != nil {
			return err
		}
		next, err := u.repo.createSession(ctx, q, newSession{
			personnelID:      row.PersonnelID,
			familyID:         uuid.UUID(row.FamilyID.Bytes),
			refreshTokenHash: refreshHash,
			deviceTokenHash:  row.DeviceTokenHash.String,
			rememberMe:       row.RememberMe,
			client:           row.Client,
			expiresAt:        now.Add(sessionTTL(row.RememberMe)),
			userAgent:        truncate(in.UserAgent, maxUserAgentLen),
			ip:               truncate(in.IP, maxIPLen),
			now:              now,
		})
		if err != nil {
			return err
		}
		if err := u.repo.rotateSession(ctx, q, row.ID, next.ID, now); err != nil {
			return err
		}
		accessToken, _, err := u.tokens.NewAccessToken(person.ID, organizationID, next.ID)
		if err != nil {
			return err
		}
		out = LoginResult{AccessToken: accessToken, RefreshToken: refreshToken, Person: personFrom(person)}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	if refused != nil {
		// Their access tokens are cut off too, not only their refresh tokens: a thief who
		// holds a still-valid access token must not get up to 15 more minutes.
		u.revocations.Revoke(ctx, cutOff...)
		return LoginResult{}, refused
	}
	return out, nil
}

// deviceStillBound reports whether the session's phone is the person's current binding, and
// notes the phone as seen when it is.
func (u *Usecase) deviceStillBound(ctx context.Context, q *db.Queries, row db.GetSessionForRefreshRow, now time.Time) (bool, error) {
	binding, found, err := u.repo.deviceBinding(ctx, q, row.PersonnelID)
	if err != nil {
		return false, err
	}
	if !found || !row.DeviceTokenHash.Valid ||
		subtle.ConstantTimeCompare([]byte(binding.DeviceTokenHash), []byte(row.DeviceTokenHash.String)) != 1 {
		return false, nil
	}
	return true, u.repo.touchDeviceBinding(ctx, q, binding.ID, now)
}

// revalidate asks Personalia about the person behind a refresh token when the cached copy
// is stale. It reads without a lock first (so no lock is held during the HTTP call) and
// decides only whether to ask; every fact it reads is read again under the lock.
func (u *Usecase) revalidate(ctx context.Context, organizationID int64, hash string, now time.Time) (resync, error) {
	var peek db.PeekSessionForRefreshRow
	var found bool
	err := database.WithTenant(ctx, u.pool, organizationID, func(q *db.Queries) (err error) {
		peek, found, err = u.repo.peekSession(ctx, q, hash)
		return err
	})
	if err != nil {
		return resync{}, err
	}
	if !found {
		return resync{}, ErrRefreshTokenInvalid
	}

	stale := now.Sub(peek.SyncedAt.Time) > personelResyncAfter
	live := !peek.RevokedAt.Valid && peek.ExpiresAt.Time.After(now) && peek.PersonnelStatus == db.PersonnelStatusACTIVE
	// A person without a NIK (D4) cannot be looked up, so the cached copy stays.
	if !stale || !live || !peek.Nik.Valid {
		return resync{state: resyncSkipped}, nil
	}

	people, err := u.personel.EmployeesByNIKs(ctx, organizationID, []string{peek.Nik.String})
	if err != nil {
		// Allowed on purpose, see Refresh. Neither the NIK nor the error text is logged: the
		// first is personal data and the second can quote the request.
		u.logger.WarnContext(ctx, "refresh: personalia could not re-validate the person, keeping the cached status",
			slog.Bool("unavailable", errors.Is(err, personel.ErrUnavailable)),
			slog.Int64("organization_id", organizationID),
		)
		return resync{state: resyncSkipped}, nil
	}
	for _, p := range people {
		if p.NIK != peek.Nik.String {
			continue
		}
		// Gone means inactive, employed elsewhere now, or a NIK that belongs to another
		// Personalia account than the one this person was cached from.
		if !p.Active || p.OrganizationID != organizationID || p.PersonelUserID != peek.PersonelUserID {
			return resync{state: resyncGone}, nil
		}
		return resync{state: resyncFresh, identity: p}, nil
	}
	return resync{state: resyncGone}, nil
}

// Logout ends the login chain of the caller's session: its refresh tokens stop working now,
// and so do the access tokens of the chain, through the revocation list.
//
// It does not unbind the phone: the binding is the admin's record and outlives a logout
// (Phase 01.3 deletes the session's push-token device here). Doing it twice with the same
// access token fails with 401, because the first call already cut that token off.
func (u *Usecase) Logout(ctx context.Context, p auth.Principal) error {
	now := u.clock.Now()
	var cutOff []int64
	err := database.WithTenant(ctx, u.pool, p.OrganizationID, func(q *db.Queries) error {
		family, found, err := u.repo.familyOf(ctx, q, p.SessionID, p.PersonnelID)
		if err != nil || !found {
			return err
		}
		cutOff, err = u.repo.endFamily(ctx, q, family, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	// The whole chain, not just this session: an access token from the refresh just before
	// this one is still valid for a few minutes and belongs to the same phone. The caller's
	// own session is always listed, even when the chain was not found, so the token that made
	// this request is cut off.
	u.revocations.Revoke(ctx, append(cutOff, p.SessionID)...)
	return nil
}
