// Package idem makes offline-capable creates safe to replay.
//
// A mobile client that was offline sends each record with a client-generated UUIDv7 `id`
// and the time it happened, `occurredAt`. If the network drops after the server stored the
// record but before the client saw the response, the client sends the same request again.
// Create makes that second request return the stored record instead of creating a duplicate.
package idem

import (
	"context"
	"errors"
	"fmt"
	"time"

	"siapp/internal/platform/apperr"
	"siapp/internal/platform/database"
)

// errStaleRead means a duplicate PRIMARY key was reported but the row could not be read
// back: find is running against an old transaction snapshot.
var errStaleRead = errors.New("record missing after duplicate primary key: find must not read from a stale snapshot")

// OfflineCreate is embedded by the request DTO of every offline-capable create. The
// `uuid7` tag is registered by httpx and checks for a canonical, lowercase UUIDv7.
type OfflineCreate struct {
	ID         string    `json:"id" validate:"required,uuid7"`
	OccurredAt time.Time `json:"occurredAt" validate:"required"`
}

// CheckOccurredAt rejects a time that cannot be real: more than 5 minutes ahead of now
// (a wrong device clock) or more than 7 days old (a record held offline too long).
// Exactly on the limit is accepted.
func CheckOccurredAt(occurredAt, now time.Time) error {
	switch {
	case occurredAt.After(now.Add(maxFutureSkew)):
		return apperr.ErrOccurredAtInFuture
	case occurredAt.Before(now.Add(-maxAge)):
		return apperr.ErrOccurredAtTooOld
	}
	return nil
}

// Create runs an idempotent create and reports whether it was a replay.
//
// find loads the record stored under the client's ID: the record, the user who owns it,
// and whether it exists. insert stores a new record and returns it.
//
// ⚠ The replay contract (README, "Offline-capable creates"):
//   - The ID is new: insert runs, replayed is false.
//   - The ID exists and belongs to userID: the stored record is returned with replayed
//     true. The caller answers 200 and must skip side effects (notifications, push,
//     realtime), because they already ran the first time.
//   - The ID exists and belongs to someone else: ErrIDConflict (409). The other user's
//     record is never returned.
//
// ⚠ The race in step 3. Two identical requests can both pass find before either inserts.
// One insert wins; the other fails with a duplicate PRIMARY key. That is not an error but
// a replay that arrived concurrently, so Create runs find again and applies the same
// rules. Only a duplicate of the PRIMARY key means this; a duplicate on any other unique
// key is a real conflict and is returned to the caller unchanged.
//
// find must see rows committed by other transactions. Run it and insert outside a
// transaction, or make find a locking read (FOR SHARE); a plain SELECT inside a
// REPEATABLE READ transaction reads an older snapshot and would miss the winner's row.
func Create[T any](
	ctx context.Context,
	find func() (rec T, ownerID string, found bool, err error),
	insert func() (T, error),
	userID string,
) (rec T, replayed bool, err error) {
	if err := ctx.Err(); err != nil {
		return rec, false, fmt.Errorf("idempotent create: %w", err)
	}

	// 1. Look for an existing record.
	existing, ownerID, found, err := find()
	if err != nil {
		return rec, false, fmt.Errorf("find existing record: %w", err)
	}
	if found {
		return resolve(existing, ownerID, userID)
	}

	// 2. Insert a new one.
	created, err := insert()
	if err == nil {
		return created, false, nil
	}
	if !database.IsDuplicateKey(err) || database.DuplicateKeyName(err) != "PRIMARY" {
		return rec, false, err
	}

	// 3. A concurrent request with the same ID won the insert. Treat this one as a replay.
	existing, ownerID, found, err = find()
	if err != nil {
		return rec, false, fmt.Errorf("find record after duplicate key: %w", err)
	}
	if !found {
		// The duplicate key said the row exists, yet it is invisible: the caller ran find
		// in a stale snapshot (see above). Fail loudly rather than guess.
		return rec, false, errStaleRead
	}
	return resolve(existing, ownerID, userID)
}

func resolve[T any](rec T, ownerID, userID string) (T, bool, error) {
	if ownerID != userID {
		var zero T
		return zero, false, apperr.ErrIDConflict
	}
	return rec, true, nil
}
