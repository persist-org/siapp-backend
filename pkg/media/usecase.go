package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Service is the media module's use cases: register an upload and confirm it. Phase 04.2 adds
// attaching uploaded photos to records and signed reads.
type Service struct {
	pool    *pgxpool.Pool
	storage Storage
	clock   clock.Clock
	logger  *slog.Logger
	repo    repo
}

// NewService returns the service. storage is the driver NewStorage chose at start-up.
func NewService(pool *pgxpool.Pool, storage Storage, clk clock.Clock, logger *slog.Logger) *Service {
	return &Service{pool: pool, storage: storage, clock: clk, logger: logger}
}

// normalizeType lowercases a content type and drops its parameters ("Image/JPEG; q=1" is
// "image/jpeg"), so the comparisons below are about the type and nothing else.
func normalizeType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// Register records a photo that is about to be uploaded and returns where to send it. It makes
// no call to the storage that can fail on its own account: presigning is local arithmetic, so
// it runs inside the transaction and a failure rolls the row back with it.
func (s *Service) Register(ctx context.Context, p auth.Principal, in RegisterInput) (Registration, error) {
	now := s.clock.Now()

	contentType := normalizeType(in.ContentType)
	if _, ok := allowedContentTypes[contentType]; !ok {
		return Registration{}, ErrTypeNotAllowed
	}
	if in.SizeBytes > maxSizeBytes {
		return Registration{}, ErrTooLarge
	}
	if (in.Latitude == nil) != (in.Longitude == nil) {
		return Registration{}, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "latitude", Code: "INVALID", Message: "Latitude dan longitude harus dikirim bersamaan.",
		})
	}
	// A phone's clock may be a little ahead, but a capture time well in the future is a wrong
	// clock or a forged one, and this field is evidence.
	if in.CapturedAt != nil && in.CapturedAt.After(now.Add(maxCapturedAtSkew)) {
		return Registration{}, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "captured_at", Code: "INVALID", Message: "Waktu pengambilan tidak boleh di masa depan.",
		})
	}

	var out Registration
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		id, err := s.repo.nextID(ctx, q)
		if err != nil {
			return err
		}
		key, err := NewKey(p.OrganizationID, in.Purpose, now, id)
		if err != nil {
			return err
		}
		m, err := s.repo.create(ctx, q, createParams{
			ID: id, Owner: p.PersonnelID, Purpose: Purpose(in.Purpose),
			ContentType: contentType, SizeBytes: in.SizeBytes, StorageKey: key,
			CapturedAt: in.CapturedAt, Latitude: in.Latitude, Longitude: in.Longitude,
			CaptureSource: CaptureSource(in.CaptureSource), Now: now,
		})
		if err != nil {
			return err
		}
		upload, err := s.storage.PresignPut(ctx, key, PutInput{ContentType: contentType, SizeBytes: in.SizeBytes, TTL: uploadURLTTL})
		if err != nil {
			return fmt.Errorf("presign upload: %w", err)
		}
		out = Registration{ID: m.ID, Upload: upload}
		return nil
	})
	if err != nil {
		return Registration{}, err
	}
	return out, nil
}

// ReissueUpload signs a new upload URL for a photo that is still PENDING.
//
// ⚠ Why this exists next to Register: POST /media is idempotent, and a replay returns the first
// response as it was stored, URL included. The offline queue retries with the same
// Idempotency-Key, so a retry after uploadURLTTL would get the same dead URL for ever. The id
// is what stays stable; the URL is short-lived and is asked for again here. Only the owner may
// ask, and only while the photo is PENDING: a confirmed photo has nothing left to upload. It is
// also refused (ErrExpired) once the photo is older than reissueWindow, so that the new URL
// always dies before the cleanup job can remove the row.
//
// The URL signs the type and size that were registered, never anything the caller sends. If the
// bytes already reached the storage and only complete was lost, the new URL answers 412 (the
// upload is write-once) and the client just calls complete.
func (s *Service) ReissueUpload(ctx context.Context, p auth.Principal, id int64) (Registration, error) {
	var m Media
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		m, err = s.repo.get(ctx, q, id)
		return err
	})
	if err != nil {
		return Registration{}, err
	}
	if m.OwnerPersonnelID != p.PersonnelID {
		return Registration{}, ErrNotFound
	}
	if m.Status != StatusPending {
		return Registration{}, ErrAlreadyUploaded
	}
	// ⚠ A photo this old is about to be removed by the cleanup job (it removes unattached rows
	// older than orphanAge, whatever their status). A URL signed now would let the bytes land
	// after the row and file are gone, and nothing lists objects without rows. The caller
	// registers again instead.
	if s.clock.Now().After(m.CreatedAt.Add(reissueWindow)) {
		return Registration{}, ErrExpired
	}
	upload, err := s.storage.PresignPut(ctx, m.StorageKey, PutInput{ContentType: m.ContentType, SizeBytes: m.SizeBytes, TTL: uploadURLTTL})
	if err != nil {
		return Registration{}, fmt.Errorf("presign upload: %w", err)
	}
	return Registration{ID: m.ID, Upload: upload}, nil
}

// Complete confirms that the owner's bytes are in storage and moves the photo to UPLOADED.
//
// The storage is asked OUTSIDE any transaction: a slow or unreachable object store must not
// hold a database connection and a row lock while it thinks. The state change at the end is
// one conditional UPDATE, so two confirmations racing both end well.
func (s *Service) Complete(ctx context.Context, p auth.Principal, id int64) (Media, error) {
	var m Media
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) (err error) {
		m, err = s.repo.get(ctx, q, id)
		return err
	})
	if err != nil {
		return Media{}, err
	}
	// Someone else's photo is "not found", the same as one that does not exist: an id must not
	// tell a stranger that it is taken.
	if m.OwnerPersonnelID != p.PersonnelID {
		return Media{}, ErrNotFound
	}
	// A retry of a confirmation that already worked (the answer was lost on the way) succeeds
	// again, without asking the storage a second time.
	if m.Status == StatusUploaded {
		return m, nil
	}

	obj, err := s.storage.Head(ctx, m.StorageKey)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		return Media{}, ErrNotUploaded
	case err != nil:
		return Media{}, fmt.Errorf("check uploaded object: %w", err)
	}
	// ⚠ The stored object is compared with what was registered, not just checked for
	// existence: the size must be the declared one, and the TYPE must be an image of the
	// declared type. That type comes from the bytes (every driver's Head sniffs them), so a text
	// file or an executable uploaded as image/jpeg is refused here instead of being attached to
	// a report as a "photo".
	if obj.SizeBytes != m.SizeBytes || normalizeType(obj.ContentType) != m.ContentType {
		s.logger.InfoContext(ctx, "media: stored object does not match the registration",
			slog.Int64("mediaId", m.ID),
			slog.Int64("declaredSize", m.SizeBytes), slog.Int64("storedSize", obj.SizeBytes),
			slog.String("declaredType", m.ContentType), slog.String("storedType", normalizeType(obj.ContentType)))
		return Media{}, ErrNotUploaded
	}

	var done Media
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		updated, ok, err := s.repo.markUploaded(ctx, q, m.ID, p.PersonnelID, s.clock.Now())
		if err != nil {
			return err
		}
		if ok {
			done = updated
			return nil
		}
		// Nothing to update: a concurrent confirmation got there first. Report what is there.
		done, err = s.repo.get(ctx, q, m.ID)
		return err
	})
	if err != nil {
		return Media{}, err
	}
	return done, nil
}
