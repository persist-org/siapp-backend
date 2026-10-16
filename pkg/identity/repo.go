package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/personel"
)

// repo is the persistence of the identity domain. Its methods take the *db.Queries of a
// transaction the caller opened: tenant data only goes through database.WithTenant.
type repo struct{}

// upsertOrganization registers the tenant on first sight and refreshes its name. It runs in
// database.WithoutTenant: the organizations registry is the one table read outside a tenant
// (it has no RLS). It is the platform's registry query (db/queries/platform.sql), used here
// because login is where a tenant is first seen.
func (repo) upsertOrganization(ctx context.Context, q *db.Queries, id int64, name string) (db.Organization, error) {
	org, err := q.UpsertOrganizationSeen(ctx, db.UpsertOrganizationSeenParams{ID: id, Name: name})
	if err != nil {
		return db.Organization{}, fmt.Errorf("upsert organization: %w", err)
	}
	return org, nil
}

// upsertPersonnel creates or refreshes the person from what Personalia just said.
func (repo) upsertPersonnel(ctx context.Context, q *db.Queries, id personel.Identity, now time.Time) (db.Personnel, error) {
	status := db.PersonnelStatusACTIVE
	if !id.Active {
		status = db.PersonnelStatusINACTIVE
	}
	p, err := q.UpsertPersonnel(ctx, db.UpsertPersonnelParams{
		PersonelUserID: id.PersonelUserID,
		Nik:            textOrNull(id.NIK),
		ClientID:       int8OrNull(id.ClientID),
		ClientName:     textOrNull(id.ClientName),
		FullName:       id.FullName,
		Email:          textOrNull(id.Email),
		Phone:          textOrNull(id.Phone),
		PhotoUrl:       textOrNull(id.PhotoURL),
		Status:         status,
		Now:            ts(now),
	})
	if err != nil {
		return db.Personnel{}, fmt.Errorf("upsert personnel: %w", err)
	}
	return p, nil
}

// deviceBinding returns the person's binding, locked until the transaction ends, or ok=false
// when they have none.
func (repo) deviceBinding(ctx context.Context, q *db.Queries, personnelID int64) (b db.DeviceBinding, ok bool, err error) {
	b, err = q.GetDeviceBindingForUpdate(ctx, personnelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.DeviceBinding{}, false, nil
	}
	if err != nil {
		return db.DeviceBinding{}, false, fmt.Errorf("get device binding: %w", err)
	}
	return b, true, nil
}

func (repo) createDeviceBinding(ctx context.Context, q *db.Queries, personnelID int64, hash string, now time.Time) error {
	if _, err := q.CreateDeviceBinding(ctx, db.CreateDeviceBindingParams{
		PersonnelID: personnelID, DeviceTokenHash: hash, Now: ts(now),
	}); err != nil {
		return fmt.Errorf("create device binding: %w", err)
	}
	return nil
}

func (repo) touchDeviceBinding(ctx context.Context, q *db.Queries, id int64, now time.Time) error {
	if err := q.TouchDeviceBinding(ctx, db.TouchDeviceBindingParams{ID: id, Now: ts(now)}); err != nil {
		return fmt.Errorf("touch device binding: %w", err)
	}
	return nil
}

type newSession struct {
	personnelID int64
	// familyID groups the sessions of one login chain; a login starts a new family.
	familyID         uuid.UUID
	refreshTokenHash string
	deviceTokenHash  string // empty for WEB
	rememberMe       bool
	client           db.SessionClient
	expiresAt        time.Time
	userAgent, ip    string
	now              time.Time
}

func (repo) createSession(ctx context.Context, q *db.Queries, s newSession) (db.Session, error) {
	sess, err := q.CreateSession(ctx, db.CreateSessionParams{
		PersonnelID:      s.personnelID,
		FamilyID:         pgtype.UUID{Bytes: s.familyID, Valid: true},
		RefreshTokenHash: s.refreshTokenHash,
		DeviceTokenHash:  textOrNull(s.deviceTokenHash),
		RememberMe:       s.rememberMe,
		Client:           s.client,
		ExpiresAt:        ts(s.expiresAt),
		UserAgent:        textOrNull(s.userAgent),
		Ip:               textOrNull(s.ip),
		Now:              ts(s.now),
	})
	if err != nil {
		return db.Session{}, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// textOrNull is NULL for an empty string: Personalia sends null for what it does not know, and
// an empty value is never worth storing as if it were data.
func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func int8OrNull(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: v != 0} }

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
