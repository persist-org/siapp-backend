package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/auth"
)

// The persistence of refresh, logout and the session chain (Phase 01.2). Like the rest of
// repo, every method takes the *db.Queries of a transaction the caller opened.

func (repo) peekSession(ctx context.Context, q *db.Queries, hash string) (db.PeekSessionForRefreshRow, bool, error) {
	row, err := q.PeekSessionForRefresh(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.PeekSessionForRefreshRow{}, false, nil
	}
	if err != nil {
		return db.PeekSessionForRefreshRow{}, false, fmt.Errorf("peek session: %w", err)
	}
	return row, true, nil
}

// sessionForRefresh returns the session with its row locked until the transaction ends.
func (repo) sessionForRefresh(ctx context.Context, q *db.Queries, hash string) (db.GetSessionForRefreshRow, bool, error) {
	row, err := q.GetSessionForRefresh(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetSessionForRefreshRow{}, false, nil
	}
	if err != nil {
		return db.GetSessionForRefreshRow{}, false, fmt.Errorf("get session for refresh: %w", err)
	}
	return row, true, nil
}

func (repo) getPersonnel(ctx context.Context, q *db.Queries, id int64) (db.Personnel, error) {
	p, err := q.GetPersonnel(ctx, id)
	if err != nil {
		return db.Personnel{}, fmt.Errorf("get personnel: %w", err)
	}
	return p, nil
}

func (repo) markInactive(ctx context.Context, q *db.Queries, id int64, now time.Time) error {
	if err := q.MarkPersonnelInactive(ctx, db.MarkPersonnelInactiveParams{ID: id, Now: ts(now)}); err != nil {
		return fmt.Errorf("mark personnel inactive: %w", err)
	}
	return nil
}

func (repo) rotateSession(ctx context.Context, q *db.Queries, oldID, newID int64, now time.Time) error {
	if err := q.RotateSession(ctx, db.RotateSessionParams{
		ID: oldID, ReplacedByID: pgtype.Int8{Int64: newID, Valid: true}, Now: ts(now),
	}); err != nil {
		return fmt.Errorf("rotate session: %w", err)
	}
	return nil
}

// endFamily revokes every live session of a login chain, deletes its devices, and returns the ids of those whose
// access tokens can still be alive, for the caller to put on the revocation list once the
// transaction has committed.
//
// reason is recorded on every session it ends, so that a later refresh with one of their
// tokens is answered with the real cause (see refusalFor).
func (repo) endFamily(ctx context.Context, q *db.Queries, family pgtype.UUID, now time.Time, reason db.SessionRevokeReason) ([]int64, error) {
	ids, err := q.ListRecentSessionIDsByFamily(ctx, db.ListRecentSessionIDsByFamilyParams{
		FamilyID: family, Since: ts(now.Add(-auth.RecentWindow)),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent sessions of family: %w", err)
	}
	if err := q.RevokeSessionFamily(ctx, db.RevokeSessionFamilyParams{
		FamilyID: family, Now: ts(now), Reason: db.NullSessionRevokeReason{SessionRevokeReason: reason, Valid: true},
	}); err != nil {
		return nil, fmt.Errorf("revoke session family: %w", err)
	}
	// A device never outlives its login: a revoked session is not a deleted one, so the push
	// token of the ended chain is removed explicitly, and a stolen chain stops receiving
	// notifications.
	if err := q.DeleteDevicesOfFamily(ctx, family); err != nil {
		return nil, fmt.Errorf("delete devices of family: %w", err)
	}
	return ids, nil
}

// familyOf returns the chain of a session if it belongs to the person, ok=false otherwise.
func (repo) familyOf(ctx context.Context, q *db.Queries, sessionID, personnelID int64) (pgtype.UUID, bool, error) {
	family, err := q.GetSessionFamily(ctx, db.GetSessionFamilyParams{ID: sessionID, PersonnelID: personnelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, fmt.Errorf("get session family: %w", err)
	}
	return family, true, nil
}
