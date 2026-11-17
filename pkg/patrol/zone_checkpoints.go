package patrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
)

// MaxZoneCheckpoints is how many checkpoints one zone may hold: a round a guard walks in one go.
const MaxZoneCheckpoints = 100

// CheckpointsReplace is the body of PUT /admin/zones/{id}/checkpoints. An empty list is allowed: it
// takes every checkpoint out of the zone.
type CheckpointsReplace struct {
	CheckpointIDs []string `json:"checkpoint_ids" validate:"required"`
}

// parseCheckpointIDs reads checkpoint_ids: uuids, once each, at most MaxZoneCheckpoints. It reports
// every bad one at once, named by its place (checkpoint_ids[2]).
func parseCheckpointIDs(in []string) ([]pgtype.UUID, error) {
	if len(in) > MaxZoneCheckpoints {
		return nil, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "checkpoint_ids", Code: "INVALID", Message: fmt.Sprintf("Maksimal %d checkpoint per zona.", MaxZoneCheckpoints),
		})
	}
	var problems []apperr.FieldError
	fail := func(i int, msg string) {
		problems = append(problems, apperr.FieldError{Field: fmt.Sprintf("checkpoint_ids[%d]", i), Code: "INVALID", Message: msg})
	}
	out := make([]pgtype.UUID, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, raw := range in {
		id, ok := parseUUID(raw)
		switch {
		case !ok:
			fail(i, "Harus berupa uuid.")
		case seen[uuidString(id)]:
			fail(i, "Checkpoint yang sama dikirim lebih dari sekali.")
		default:
			seen[uuidString(id)] = true
			out = append(out, id)
		}
	}
	if len(problems) > 0 {
		return nil, apperr.ErrValidation.WithFields(problems...)
	}
	return out, nil
}

// lockCheckpoints locks the checkpoints a zone is about to link, in id order, and returns them by id.
// An id it does not return does not exist, is deleted, or belongs to another organization (row-level
// security hides it): linkCheckpoints answers all three the same.
//
// ⚠ Lock order is checkpoint first, then zone, for every writer of the links (this, and deleting a
// checkpoint). A link insert takes a share lock on its checkpoint row, so a writer that held the zone
// and then asked for the checkpoint could deadlock against a deletion that holds the checkpoint and
// asks for the zone. Callers therefore call this before they lock the zone.
func lockCheckpoints(ctx context.Context, q *db.Queries, ids []pgtype.UUID) (map[string]db.PatrolCheckpoint, error) {
	found := make(map[string]db.PatrolCheckpoint, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	rows, err := q.LockCheckpointsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("lock checkpoints: %w", err)
	}
	for _, r := range rows {
		found[uuidString(r.ID)] = r
	}
	return found, nil
}

// lockZones locks the live zones among ids, in id order, and returns the ones it locked. A deleted zone is
// skipped: it has no order to keep and no guard to release.
//
// ⚠ The order of locks is checkpoint first, then zones in id order, for every writer of a zone's links
// (see lockCheckpoints), so two requests that share zones cannot deadlock. The caller already holds the
// lock of the checkpoint it is changing.
func lockZones(ctx context.Context, q *db.Queries, ids []int64) ([]int64, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	locked := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, err := q.GetPatrolZoneForUpdate(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("lock zone: %w", err)
		}
		locked = append(locked, id)
	}
	return locked, nil
}

// releaseIfEmpty closes the open check-ins of a zone that has no active checkpoint left, at the given moment.
//
// ⚠ A guard checks in and out by scanning a checkpoint's QR (Phase 10.9), so a zone with no checkpoint to
// scan would hold its guards for ever, and one open check-in blocks a check-in in every zone. This is what
// deleting a zone does too (Delete). A zone that still has an active checkpoint needs nothing: the guard
// checks out at any of them. Callers hold the zone's lock.
func releaseIfEmpty(ctx context.Context, q *db.Queries, zoneID int64, at time.Time) error {
	counts, err := q.CountCheckpointsOfZones(ctx, []int64{zoneID})
	if err != nil {
		return fmt.Errorf("count checkpoints of zone: %w", err)
	}
	if len(counts) > 0 && counts[0].Checkpoints > 0 {
		return nil
	}
	if _, err := q.CloseZoneCheckinsOfZone(ctx, db.CloseZoneCheckinsOfZoneParams{ZoneID: zoneID, At: ts(at)}); err != nil {
		return fmt.Errorf("close check-ins of an empty zone: %w", err)
	}
	return nil
}

// linkCheckpoints makes the zone's checkpoints exactly ids, in that order: positions 1..n. The caller
// holds the zone's row lock and the checkpoints' (lockCheckpoints), and passes what that returned.
//
// A checkpoint must be of the zone's project (the foreign keys refuse anything else, this names the
// id first) and active. An inactive one is accepted only when the zone already holds it: switching a
// checkpoint off must not make its zone impossible to reorder, and it keeps its place until the admin
// leaves it out. All the links are rewritten in one go (delete, then insert), because the unique
// position is checked row by row and shifting positions in place can collide with itself.
func linkCheckpoints(ctx context.Context, q *db.Queries, zone db.PatrolZone, ids []pgtype.UUID, found map[string]db.PatrolCheckpoint) error {
	links, err := q.ListZoneLinks(ctx, zone.ID)
	if err != nil {
		return fmt.Errorf("list zone links: %w", err)
	}
	linked := make(map[string]bool, len(links))
	for _, l := range links {
		linked[uuidString(l.CheckpointID)] = true
	}
	for i, id := range ids {
		key := uuidString(id)
		cp, ok := found[key]
		if !ok || cp.ProjectCode != zone.ProjectCode || (!cp.Active && !linked[key]) {
			return checkpointNotInProject(i, key)
		}
	}
	if err := q.DeleteZoneCheckpoints(ctx, zone.ID); err != nil {
		return fmt.Errorf("clear zone links: %w", err)
	}
	for i, id := range ids {
		if err := q.InsertZoneCheckpoint(ctx, db.InsertZoneCheckpointParams{
			ProjectCode: zone.ProjectCode, ZoneID: zone.ID, CheckpointID: id, SortOrder: int32(i + 1), //nolint:gosec // at most MaxZoneCheckpoints
		}); err != nil {
			return fmt.Errorf("link checkpoint: %w", err)
		}
	}
	return nil
}
