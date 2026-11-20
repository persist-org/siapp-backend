package patrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/media"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/idem"
)

// SkipInput is the body of POST /zones/{id}/skip (Phase 10.17): the guard could not scan a checkpoint of the
// round and says why. local_id (also the Idempotency-Key) and occurred_at are the offline fields, as in
// ScanInput.
//
// There is no QR (the sticker may be what is broken) and no device condition (a skip has no answered
// action to keep it on). The position is optional, like a scan's.
type SkipInput struct {
	LocalID      string    `json:"local_id" validate:"required,uuid"`
	OccurredAt   time.Time `json:"occurred_at" validate:"required"`
	CheckpointID string    `json:"checkpoint_id" validate:"required,uuid"`
	// Reason is STICKER_DAMAGED, ACCESS_BLOCKED or OTHER; Note is required for OTHER.
	Reason         string   `json:"reason" validate:"required,oneof=STICKER_DAMAGED ACCESS_BLOCKED OTHER"`
	Note           *string  `json:"note" validate:"omitempty,max=2000"`
	MediaIDs       []int64  `json:"media_ids" validate:"omitempty,max=5,dive,gt=0"`
	Latitude       *float64 `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude      *float64 `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	AccuracyMeters *float64 `json:"accuracy_meters" validate:"omitempty,gte=0,lte=100000"`
	// SubmittedOffline is true when the phone queued it.
	SubmittedOffline bool `json:"submitted_offline"`
}

// Offline is the offline fields of the skip, for idem.OfflineCreate's checks.
func (in SkipInput) Offline() idem.OfflineCreate {
	return idem.OfflineCreate{LocalID: in.LocalID, OccurredAt: in.OccurredAt}
}

// checkSkip is what the tags cannot say: the note of an OTHER, the same photo twice, and the position.
func checkSkip(in SkipInput) error {
	var problems []apperr.FieldError
	if in.Reason == string(db.PatrolSkipReasonOTHER) && (in.Note == nil || strings.TrimSpace(*in.Note) == "") {
		problems = append(problems, apperr.FieldError{Field: "note", Code: "REQUIRED", Message: "Wajib diisi bila alasannya lainnya."})
	}
	seen := make(map[int64]bool, len(in.MediaIDs))
	for i, id := range in.MediaIDs {
		if seen[id] {
			problems = append(problems, apperr.FieldError{Field: fmt.Sprintf("media_ids[%d]", i), Code: "DUPLICATE", Message: "Foto yang sama tidak boleh dipakai dua kali."})
		}
		seen[id] = true
	}
	if len(problems) > 0 {
		return apperr.ErrValidation.WithFields(problems...)
	}
	return checkPosition(in.Latitude, in.Longitude, in.AccuracyMeters)
}

// Skip records that a checkpoint of the round could not be scanned.
//
// ⚠ A skip is a patrol_scans row (outcome SKIPPED), so the order rule counts it like a scan: the checkpoint
// after it can be scanned, which is the point of it (BE-CP1 of the mobile feedback: one checkpoint that cannot
// be scanned must not stop the rest of an offline round). It is judged like a scan in everything but the QR
// and the interval: the same permission, membership, work date, lock and order; the position is only
// flagged. It is not a visit, so the progress counters, the patrol's scan_count and the patrol interval
// leave it out.
//
// Idempotent by local_id, like Scan.
func (s *Service) Skip(ctx context.Context, p auth.Principal, zoneID int64, in SkipInput) (ScanResult, error) {
	res, err := s.skip(ctx, p, zoneID, in)
	if err != nil && database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_scans_local_id_uq" {
		return s.replay(ctx, p, zoneID, ScanInput{LocalID: in.LocalID})
	}
	return res, err
}

func (s *Service) skip(ctx context.Context, p auth.Principal, zoneID int64, in SkipInput) (ScanResult, error) {
	localID, err := uuid.Parse(in.LocalID)
	if err != nil {
		return ScanResult{}, apperr.ErrValidation.WithFields(apperr.FieldError{Field: "local_id", Code: "INVALID", Message: "Harus berupa UUID yang valid."})
	}
	if err := checkSkip(in); err != nil {
		return ScanResult{}, err
	}
	now := s.clock.Now()
	at := in.OccurredAt.UTC()
	photoIDs := in.MediaIDs
	if photoIDs == nil {
		photoIDs = []int64{}
	}

	var (
		out     ScanResult
		notices []Notice
	)
	err = database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		if got, found, err := s.stored(ctx, q, p, zoneID, in.LocalID); err != nil {
			return err
		} else if found {
			out = ScanResult{Scan: got, Replayed: true}
			return nil
		}

		zone, err := q.GetPatrolZone(ctx, zoneID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !zone.Active) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get zone: %w", err)
		}
		project, err := q.GetProject(ctx, zone.ProjectCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrZoneNotFound
		}
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}

		// The checkpoint must be the zone's; no QR (see Skip).
		v, err := s.reach(ctx, q, p, zone, project, in.CheckpointID, nil, time.Time{}, false)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, q, p, v, at); err != nil {
			return err
		}
		v, flag := flagLocation(v, in.Latitude, in.Longitude, in.AccuracyMeters)
		pl, err := s.place(ctx, q, p, v, at)
		if err != nil {
			return err
		}
		// The order of the round, under the lock a scan takes (see lockScanning). Not the patrol interval (a skip is
		// not a visit), but the scanMinGap: a double tap or a retry with a new local_id must not make two rows
		// of one skip, which the order rule would count as two walks past the checkpoint.
		if err := lockScanning(ctx, q, p, zoneID); err != nil {
			return err
		}
		if err := checkSkipGap(ctx, q, p, zoneID, v.checkpoint.ID, at); err != nil {
			return err
		}
		if err := checkOrder(ctx, q, p, zoneID, v.checkpoint.ID, pl.workDate, at); err != nil {
			return err
		}
		patrolID, err := s.joinPatrol(ctx, q, p, zone.ProjectCode, at)
		if err != nil {
			return err
		}

		row, err := q.InsertPatrolScan(ctx, db.InsertPatrolScanParams{
			LocalID: pgtype.UUID{Bytes: localID, Valid: true}, ZoneID: zoneID, ZoneType: zone.Type,
			PersonnelID: p.PersonnelID, TeamCode: pl.team, ProjectCode: zone.ProjectCode,
			OccurredAt: ts(at), ReceivedAt: ts(now), WorkDate: pgtype.Date{Time: pl.workDate.Time(), Valid: true},
			Latitude: in.Latitude, Longitude: in.Longitude, AccuracyMeters: in.AccuracyMeters,
			DistanceMeters: intParam(v.distance), Subject: "Dilewati: " + v.checkpoint.Name, Notes: textOrNull(in.Note),
			PatrolID: patrolID, CheckpointID: v.checkpoint.ID, SubmittedOffline: in.SubmittedOffline,
			LocationFlag: flag, Outcome: db.PatrolScanOutcomeSKIPPED,
			SkipReason:   db.NullPatrolSkipReason{PatrolSkipReason: db.PatrolSkipReason(in.Reason), Valid: true},
			SkipMediaIds: photoIDs,
		})
		if err != nil {
			return fmt.Errorf("insert skip: %w", err)
		}
		// ⚠ Not AddScanToRoute: a skip is not a visit, so the patrol's scan_count leaves it out (finish counts
		// the same way).

		if len(photoIDs) > 0 {
			if s.attach == nil {
				return errors.New("patrol: photo attachments are not plugged in")
			}
			if err := s.attach.Attach(ctx, q, mediaAttach(p, row.ID, photoIDs)); err != nil {
				return err
			}
		}

		scan, err := scanOf(row)
		if err != nil {
			return err
		}
		scan.Checkpoint = ScanCheckpoint{ID: uuidString(v.checkpoint.ID), Name: v.checkpoint.Name, SortOrder: &v.position}
		out = ScanResult{Scan: scan}
		notices, err = s.noticesFor(ctx, q, p, zone, project, v.checkpoint, scan)
		return err
	})
	if err != nil {
		return ScanResult{}, err
	}
	s.notify(ctx, notices...)
	return out, nil
}

// checkSkipGap refuses a skip of the checkpoint within scanMinGap of any other row of it by the person (a scan, a
// declined one or another skip), on either side of its own moment, with the same ZONE_INTERVAL_NOT_ELAPSED a
// scan gets. Only the minimum gap, not the project's patrol interval: that one is about visits. The caller holds
// lockScanning.
func checkSkipGap(ctx context.Context, q *db.Queries, p auth.Principal, zoneID int64, checkpointID pgtype.UUID, at time.Time) error {
	last, err := q.LastRowBetween(ctx, db.LastRowBetweenParams{
		PersonnelID: p.PersonnelID, ZoneID: zoneID, CheckpointID: checkpointID, After: ts(at.Add(-scanMinGap)), Before: ts(at.Add(scanMinGap)),
	})
	if err != nil {
		return fmt.Errorf("check skip gap: %w", err)
	}
	t := lastScan(last)
	if t == nil {
		return nil
	}
	return ErrIntervalNotElapsed.WithDetails(map[string]any{
		"interval_seconds": int(scanMinGap.Seconds()),
		"next_allowed_at":  nextAllowedAt(*t, scanMinGap).UTC().Format(time.RFC3339Nano),
	})
}

// mediaAttach is the claim of a skip's photos for the scan row, as a scan's actions make for theirs.
func mediaAttach(p auth.Principal, scanID int64, ids []int64) media.AttachInput {
	return media.AttachInput{
		MediaIDs: ids, OwnerID: p.PersonnelID, Purpose: media.PurposePatrolScan,
		EntityType: EntityScan, EntityID: scanID, Max: len(ids),
	}
}
