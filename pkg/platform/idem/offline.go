package idem

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"siapp/pkg/platform/apperr"
)

// The window an offline item may claim for occurred_at: a small allowance for clock skew
// into the future, and the 7 days the app may stay offline into the past.
const (
	maxFuture = 5 * time.Minute
	maxAge    = 7 * 24 * time.Hour
)

// OfflineCreate is embedded in the request body of every offline-capable create. The app
// queues the item with a stable UUID (the Idempotency-Key header, repeated here as
// local_id) and the moment it really happened.
type OfflineCreate struct {
	LocalID    string    `json:"local_id" validate:"required,uuid"`
	OccurredAt time.Time `json:"occurred_at" validate:"required"`
}

// Check validates the offline fields of a request: local_id must equal the
// Idempotency-Key header (a client that mixes them up would defeat the replay protection),
// and occurred_at must be inside the allowed window. Call it after httpx.Decode.
func (o OfflineCreate) Check(r *http.Request, now time.Time) error {
	if key := r.Header.Get(HeaderKey); key != "" {
		// Compare as UUIDs, so letter case in the body does not matter.
		local, lerr := uuid.Parse(o.LocalID)
		header, herr := uuid.Parse(key)
		if lerr != nil || herr != nil || local != header {
			return apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "local_id", Code: "MISMATCH", Message: "local_id harus sama dengan header Idempotency-Key.",
			})
		}
	}
	return CheckOccurredAt(o.OccurredAt, now)
}

// CheckOccurredAt rejects an occurred_at more than 5 minutes in the future
// (OCCURRED_AT_IN_FUTURE) or more than 7 days in the past (OCCURRED_AT_TOO_OLD). Both are
// 422 UNPROCESSABLE; exactly on the boundary is still accepted.
func CheckOccurredAt(occurredAt, now time.Time) error {
	switch {
	case occurredAt.After(now.Add(maxFuture)):
		return apperr.ErrUnprocessable.
			WithReason(apperr.ReasonOccurredAtInFuture).
			WithMessage("Waktu kejadian tidak boleh di masa depan.")
	case occurredAt.Before(now.Add(-maxAge)):
		return apperr.ErrUnprocessable.
			WithReason(apperr.ReasonOccurredAtTooOld).
			WithMessage("Waktu kejadian terlalu lama (maksimum 7 hari).")
	}
	return nil
}
