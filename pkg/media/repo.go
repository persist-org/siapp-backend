package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
)

// repo wraps the media queries. Every method receives the *db.Queries of a tenant transaction
// (database.WithTenant); nothing here opens one.
type repo struct{}

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func toMedia(m db.Medium) Media {
	out := Media{
		ID:               m.ID,
		OwnerPersonnelID: m.OwnerPersonnelID,
		Purpose:          m.Purpose,
		ContentType:      m.ContentType,
		SizeBytes:        m.SizeBytes,
		StorageKey:       m.StorageKey,
		Status:           m.Status,
		Latitude:         m.CaptureLatitude,
		Longitude:        m.CaptureLongitude,
		CaptureSource:    m.CaptureSource,
		CreatedAt:        m.CreatedAt.Time,
		UpdatedAt:        m.UpdatedAt.Time,
	}
	if m.CapturedAt.Valid {
		t := m.CapturedAt.Time
		out.CapturedAt = &t
	}
	return out
}

// nextID reserves the id of a photo that is about to be inserted. The storage key contains
// it, so it has to be known first.
func (repo) nextID(ctx context.Context, q *db.Queries) (int64, error) {
	id, err := q.NextMediaID(ctx)
	if err != nil {
		return 0, fmt.Errorf("next media id: %w", err)
	}
	return id, nil
}

// createParams is what a new row needs; the organization comes from the tenant setting.
type createParams struct {
	ID            int64
	Owner         int64
	Purpose       Purpose
	ContentType   string
	SizeBytes     int64
	StorageKey    string
	CapturedAt    *time.Time
	Latitude      *float64
	Longitude     *float64
	CaptureSource CaptureSource
	Now           time.Time
}

func (repo) create(ctx context.Context, q *db.Queries, p createParams) (Media, error) {
	arg := db.CreateMediaParams{
		ID:               p.ID,
		OwnerPersonnelID: p.Owner,
		Purpose:          p.Purpose,
		ContentType:      p.ContentType,
		SizeBytes:        p.SizeBytes,
		StorageKey:       p.StorageKey,
		CaptureLatitude:  p.Latitude,
		CaptureLongitude: p.Longitude,
		CaptureSource:    p.CaptureSource,
		Now:              timestamptz(p.Now),
	}
	if p.CapturedAt != nil {
		arg.CapturedAt = timestamptz(*p.CapturedAt)
	}
	row, err := q.CreateMedia(ctx, arg)
	if err != nil {
		return Media{}, fmt.Errorf("create media: %w", err)
	}
	return toMedia(row), nil
}

// get returns ErrNotFound for an id with no visible row: it does not exist, or row-level
// security hides it because it belongs to another organization.
func (repo) get(ctx context.Context, q *db.Queries, id int64) (Media, error) {
	row, err := q.GetMedia(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, ErrNotFound
	}
	if err != nil {
		return Media{}, fmt.Errorf("get media: %w", err)
	}
	return toMedia(row), nil
}

// markUploaded moves a PENDING photo of this owner to UPLOADED. updated is false when there
// was nothing to move: the photo is not this person's, or it is no longer PENDING.
func (repo) markUploaded(ctx context.Context, q *db.Queries, id, owner int64, now time.Time) (m Media, updated bool, err error) {
	row, err := q.MarkMediaUploaded(ctx, db.MarkMediaUploadedParams{ID: id, OwnerPersonnelID: owner, Now: timestamptz(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return Media{}, false, nil
	}
	if err != nil {
		return Media{}, false, fmt.Errorf("mark media uploaded: %w", err)
	}
	return toMedia(row), true, nil
}
