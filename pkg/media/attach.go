package media

import (
	"context"
	"fmt"
	"time"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
)

// AttachInput says which photos a record claims, and for what.
type AttachInput struct {
	// MediaIDs are the photos to attach, each once. Empty attaches nothing.
	MediaIDs []int64
	// OwnerID is the person the photos must belong to: the sender of the record, taken from
	// the authenticated principal, never from the request body.
	OwnerID int64
	// Purpose is what the record is for. A photo registered for another purpose is refused.
	Purpose Purpose
	// EntityType and EntityID name the record ("REPORT", 4711).
	EntityType string
	EntityID   int64
	// Max is the most photos this call may attach. 0 means defaultAttachLimit (5). It counts
	// this call only: records attach their photos once, at create. A module that later lets a
	// record gain photos must count the ones it already has.
	Max int
}

// Attach claims uploaded photos for a record. It is how every feature module takes a photo
// into one of its records: reports, patrol scans, selfies, instructions, panic and asset
// reports, E-Memo.
//
// It runs inside the CALLER's tenant transaction (q comes from database.WithTenant), so the
// record and its photos commit or roll back together.
//
// ⚠ On any error the caller's transaction must roll back, which returning the error from the
// WithTenant callback does. Attach attaches every photo in one statement, and when that comes
// up short it reports why, but it cannot undo the rows the statement did reach: only the
// rollback does. A module that swallowed the error and committed would keep a half-attached
// set.
//
// Errors, all *apperr.Error with the photo's id in details.media_id:
//   - MEDIA_NOT_FOUND: no such photo, another organization's, or another person's;
//   - MEDIA_NOT_UPLOADED: registered but never confirmed;
//   - MEDIA_PURPOSE_MISMATCH: registered for another kind of record;
//   - MEDIA_ALREADY_ATTACHED: another record claimed it first;
//   - MEDIA_LIMIT_EXCEEDED: more photos than Max (checked first, before the database);
//   - VALIDATION_ERROR: the same id twice.
func (s *Service) Attach(ctx context.Context, q *db.Queries, in AttachInput) error {
	if in.OwnerID < 1 || in.EntityID < 1 || in.EntityType == "" || in.Purpose == "" {
		// A module's bug, not a client's mistake: it must name the owner, the record and the purpose.
		return fmt.Errorf("media: Attach needs an owner, a purpose and the record it is for (got owner %d, purpose %q, %q %d)",
			in.OwnerID, in.Purpose, in.EntityType, in.EntityID)
	}
	if len(in.MediaIDs) == 0 {
		return nil
	}
	limit := in.Max
	if limit <= 0 {
		limit = defaultAttachLimit
	}
	if len(in.MediaIDs) > limit {
		return ErrLimitExceeded.WithDetails(map[string]any{"max": limit})
	}
	if dup, found := firstDuplicate(in.MediaIDs); found {
		return apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "media_ids", Code: "DUPLICATE", Message: "Setiap foto hanya boleh dilampirkan sekali.",
		}).WithDetails(map[string]any{"media_id": dup})
	}

	// ⚠ One conditional UPDATE for every photo, then a comparison of the row count with the
	// number of ids. Why not read first and update after: two records attaching the same photo
	// at the same moment would both read "free" and both attach it. In one UPDATE the
	// conditions (owner, UPLOADED, purpose, not yet attached) are re-checked by Postgres on the
	// row the other transaction just changed, so exactly one of them gets it and the other
	// finds the count short. No lock, no race, one round trip in the normal case.
	n, err := s.repo.attach(ctx, q, in, s.clock.Now())
	if err != nil {
		return err
	}
	if n == int64(len(in.MediaIDs)) {
		return nil
	}
	return s.whyShort(ctx, q, in)
}

// whyShort reads the photos again to name the first one that could not be attached, in the
// order the module sent them. The order of the checks is the order a client can act on: a
// photo that is not theirs is not found; one not yet confirmed needs its upload finished; one
// for the wrong record kind cannot be fixed by retrying; one already claimed is taken.
func (s *Service) whyShort(ctx context.Context, q *db.Queries, in AttachInput) error {
	found, err := s.repo.getByIDs(ctx, q, in.MediaIDs)
	if err != nil {
		return err
	}
	byID := make(map[int64]Media, len(found))
	for _, m := range found {
		byID[m.ID] = m
	}
	for _, id := range in.MediaIDs {
		m, ok := byID[id]
		var reason *apperr.Error
		switch {
		case !ok || m.OwnerPersonnelID != in.OwnerID:
			reason = ErrNotFound
		case m.Status != StatusUploaded:
			reason = ErrNotUploaded
		case m.Purpose != in.Purpose:
			reason = ErrPurposeMismatch
		case m.AttachedEntityID != nil && !attachedTo(m, in):
			reason = ErrAlreadyAttached
		default:
			continue
		}
		return reason.WithDetails(map[string]any{"media_id": id})
	}
	// Every photo looks attachable or is attached to THIS record, so nothing else explains the
	// short count: either another transaction attached or removed one between the UPDATE and
	// this read, or this record's photo was attached by an earlier Attach of the same record.
	// The attach did not complete, and "taken" is what it amounts to.
	return ErrAlreadyAttached
}

// attachedTo reports whether m is attached to the very record in names.
//
// ⚠ The re-read in whyShort runs in the same transaction as the UPDATE, so it sees the rows
// that statement DID attach, which are attached to this record. Counting those as "already
// attached" would blame a photo that was fine and hide the one that was not.
func attachedTo(m Media, in AttachInput) bool {
	return m.AttachedEntityType != nil && *m.AttachedEntityType == in.EntityType &&
		m.AttachedEntityID != nil && *m.AttachedEntityID == in.EntityID
}

func firstDuplicate(ids []int64) (int64, bool) {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return id, true
		}
		seen[id] = struct{}{}
	}
	return 0, false
}

// View is a photo as a record's response shows it. These fill the contract's photos arrays.
type View struct {
	ID int64
	// URL is a short-lived signed URL (one hour): a presigned S3 URL or a local blob URL,
	// depending on the driver. It is not stored anywhere; a client that needs it later loads the
	// record again.
	URL           string
	ContentType   string
	CapturedAt    *time.Time
	CaptureSource CaptureSource
}

// Views returns the viewable form of photos, in the order of ids. Only photos that a record has
// attached are returned: a record's photos always are, and a photo nobody claimed is still its
// owner's private draft, which a module that passed ids straight from a request must not be
// able to hand to someone else. A photo that is not visible (another organization's,
// nonexistent), not confirmed or not attached is left out rather than failing the whole record.
//
// ⚠ There is deliberately NO endpoint that serves a photo by media id, and Views does not ask
// who is looking. The module that owns the record decides whether the caller may see the
// record (its own scope rules, inside WithTenant), and only then embeds these views in its
// response. A by-id endpoint would need its own copy of every module's visibility rules, and
// the first one to drift from them would leak photos. The signed URL is the capability, and it
// expires.
//
// q is the caller's tenant transaction, like Attach's: the read is scoped to the caller's
// organization by row-level security.
func (s *Service) Views(ctx context.Context, q *db.Queries, ids []int64) ([]View, error) {
	if len(ids) == 0 {
		return []View{}, nil
	}
	found, err := s.repo.getByIDs(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]Media, len(found))
	for _, m := range found {
		byID[m.ID] = m
	}

	out := make([]View, 0, len(ids))
	for _, id := range ids {
		m, ok := byID[id]
		if !ok || m.Status != StatusUploaded || m.AttachedEntityID == nil {
			continue
		}
		url, err := s.storage.PresignGet(ctx, m.StorageKey, m.ContentType, viewURLTTL)
		if err != nil {
			return nil, fmt.Errorf("presign photo %d: %w", m.ID, err)
		}
		out = append(out, View{ID: m.ID, URL: url, ContentType: m.ContentType, CapturedAt: m.CapturedAt, CaptureSource: m.CaptureSource})
	}
	return out, nil
}
