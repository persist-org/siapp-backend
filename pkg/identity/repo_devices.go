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
	"siapp/pkg/platform/personel"
)

// The persistence of push-token devices, the admin device reset, the people cache lookups and
// the resync (Phase 01.3). Like the rest of repo, every method takes the *db.Queries of a
// transaction the caller opened.

func (repo) upsertDevice(ctx context.Context, q *db.Queries, personnelID, sessionID int64, platform db.DevicePlatform, token, appVersion string, now time.Time) error {
	// ⚠ A push token belongs to one device: whoever else holds it loses it first (a phone that
	// changed hands, or an app that was reinstalled and now sits on a new session).
	if err := q.DeleteDevicesByPushTokenExceptSession(ctx, db.DeleteDevicesByPushTokenExceptSessionParams{
		PushToken: token, SessionID: sessionID,
	}); err != nil {
		return fmt.Errorf("remove push token from other devices: %w", err)
	}
	if _, err := q.UpsertDevice(ctx, db.UpsertDeviceParams{
		PersonnelID: personnelID, SessionID: sessionID, Platform: platform, PushToken: token,
		AppVersion: textOrNull(appVersion), Now: ts(now),
	}); err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

// liveSession returns the live session of the login chain that sessionID belongs to.
func (repo) liveSession(ctx context.Context, q *db.Queries, sessionID, personnelID int64) (int64, bool, error) {
	id, err := q.GetLiveSessionOfChain(ctx, db.GetLiveSessionOfChainParams{SessionID: sessionID, PersonnelID: personnelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("get live session of chain: %w", err)
	}
	return id, true, nil
}

func (repo) removeChainDevice(ctx context.Context, q *db.Queries, sessionID, personnelID int64, token string) error {
	if _, err := q.DeleteChainDeviceByPushToken(ctx, db.DeleteChainDeviceByPushTokenParams{
		PushToken: token, SessionID: sessionID, PersonnelID: personnelID,
	}); err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	return nil
}

func (repo) moveDevice(ctx context.Context, q *db.Queries, oldSessionID, newSessionID int64, now time.Time) error {
	if err := q.MoveDevice(ctx, db.MoveDeviceParams{OldSessionID: oldSessionID, NewSessionID: newSessionID, Now: ts(now)}); err != nil {
		return fmt.Errorf("move device: %w", err)
	}
	return nil
}

// resetBinding deletes the person's phone binding, revokes their MOBILE sessions and deletes
// those sessions' push-token devices. It returns how many bindings it deleted (0 or 1) and
// the ids of the revoked sessions whose access tokens can still be alive.
func (repo) resetBinding(ctx context.Context, q *db.Queries, personnelID int64, now time.Time) (deleted int64, cutOff []int64, err error) {
	deleted, err = q.DeleteDeviceBinding(ctx, personnelID)
	if err != nil {
		return 0, nil, fmt.Errorf("delete device binding: %w", err)
	}
	if deleted == 0 {
		return 0, nil, nil
	}
	cutOff, err = q.ListRecentMobileSessionIDs(ctx, db.ListRecentMobileSessionIDsParams{
		PersonnelID: personnelID, Since: ts(now.Add(-auth.RecentWindow)),
	})
	if err != nil {
		return 0, nil, fmt.Errorf("list recent mobile sessions: %w", err)
	}
	if err := q.RevokeMobileSessions(ctx, db.RevokeMobileSessionsParams{PersonnelID: personnelID, Now: ts(now)}); err != nil {
		return 0, nil, fmt.Errorf("revoke mobile sessions: %w", err)
	}
	if err := q.DeleteMobileDevices(ctx, personnelID); err != nil {
		return 0, nil, fmt.Errorf("delete mobile devices: %w", err)
	}
	return deleted, cutOff, nil
}

// endAllSessions revokes every live session of a person, whatever the chain, deletes their
// devices, and returns the ids whose access tokens can still be alive.
func (repo) endAllSessions(ctx context.Context, q *db.Queries, personnelID int64, now time.Time) ([]int64, error) {
	ids, err := q.ListRecentSessionIDsOfPersonnel(ctx, db.ListRecentSessionIDsOfPersonnelParams{
		PersonnelID: personnelID, Since: ts(now.Add(-auth.RecentWindow)),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent sessions of personnel: %w", err)
	}
	if err := q.RevokeSessionsOfPersonnel(ctx, db.RevokeSessionsOfPersonnelParams{PersonnelID: personnelID, Now: ts(now)}); err != nil {
		return nil, fmt.Errorf("revoke sessions of personnel: %w", err)
	}
	if err := q.DeleteDevicesOfPersonnel(ctx, personnelID); err != nil {
		return nil, fmt.Errorf("delete devices of personnel: %w", err)
	}
	return ids, nil
}

func (repo) personnelByNIK(ctx context.Context, q *db.Queries, nik string) (db.Personnel, bool, error) {
	p, err := q.GetPersonnelByNIK(ctx, pgtype.Text{String: nik, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Personnel{}, false, nil
	}
	if err != nil {
		return db.Personnel{}, false, fmt.Errorf("get personnel by nik: %w", err)
	}
	return p, true, nil
}

// refreshPersonnel overwrites the cached copy with what Personalia says, without counting
// it as the person's own activity.
func (repo) refreshPersonnel(ctx context.Context, q *db.Queries, id int64, p personel.Identity, now time.Time) error {
	if err := releaseNIK(ctx, q, p, now); err != nil {
		return err
	}
	if err := q.RefreshPersonnel(ctx, db.RefreshPersonnelParams{
		ID: id, Nik: textOrNull(p.NIK), ClientID: int8OrNull(p.ClientID), ClientName: textOrNull(p.ClientName),
		FullName: p.FullName, Email: textOrNull(p.Email), Phone: textOrNull(p.Phone), PhotoUrl: textOrNull(p.PhotoURL),
		Now: ts(now),
	}); err != nil {
		return fmt.Errorf("refresh personnel: %w", err)
	}
	return nil
}
