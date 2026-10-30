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
	if m.AttachedEntityType.Valid {
		out.AttachedEntityType = &m.AttachedEntityType.String
	}
	if m.AttachedEntityID.Valid {
		out.AttachedEntityID = &m.AttachedEntityID.Int64
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

// getByIDs returns the visible photos among ids, in no particular order.
func (repo) getByIDs(ctx context.Context, q *db.Queries, ids []int64) ([]Media, error) {
	rows, err := q.GetMediaByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("get media by ids: %w", err)
	}
	out := make([]Media, len(rows))
	for i, row := range rows {
		out[i] = toMedia(row)
	}
	return out, nil
}

// attach runs the one conditional UPDATE and returns how many rows it attached.
func (repo) attach(ctx context.Context, q *db.Queries, in AttachInput, now time.Time) (int64, error) {
	n, err := q.AttachMedia(ctx, db.AttachMediaParams{
		EntityType:       pgtype.Text{String: in.EntityType, Valid: true},
		EntityID:         pgtype.Int8{Int64: in.EntityID, Valid: true},
		Now:              timestamptz(now),
		Ids:              in.MediaIDs,
		OwnerPersonnelID: in.OwnerID,
		Purpose:          in.Purpose,
	})
	if err != nil {
		return 0, fmt.Errorf("attach media: %w", err)
	}
	return n, nil
}

// staleMedia is a photo the cleanup job is about to remove.
type staleMedia struct {
	ID        int64
	Key       string
	CreatedAt time.Time
}

// staleCursor is where a cleanup batch starts: after the photo with this (created_at, id). The
// zero value starts at the oldest photo.
type staleCursor struct {
	CreatedAt time.Time
	ID        int64
}

// lockStale returns, and locks, up to batch unattached photos registered before cutoff, oldest
// first, starting after the cursor.
func (repo) lockStale(ctx context.Context, q *db.Queries, cutoff time.Time, after staleCursor, batch int32) ([]staleMedia, error) {
	rows, err := q.ListStaleMediaForUpdate(ctx, db.ListStaleMediaForUpdateParams{
		Cutoff:         timestamptz(cutoff),
		AfterCreatedAt: timestamptz(after.CreatedAt),
		AfterID:        after.ID,
		Batch:          batch,
	})
	if err != nil {
		return nil, fmt.Errorf("list stale media: %w", err)
	}
	out := make([]staleMedia, len(rows))
	for i, row := range rows {
		out[i] = staleMedia{ID: row.ID, Key: row.StorageKey, CreatedAt: row.CreatedAt.Time}
	}
	return out, nil
}

// deleteRows removes the rows of ids that are still unattached.
func (repo) deleteRows(ctx context.Context, q *db.Queries, ids []int64) (int64, error) {
	n, err := q.DeleteMediaByIDs(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("delete media: %w", err)
	}
	return n, nil
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
