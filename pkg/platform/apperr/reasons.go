package apperr

// Platform reasons (details.reason). Module reasons live with their module; a reason that
// has shipped is never renamed.
const (
	// ReasonIdempotencyKeyReused: the Idempotency-Key was already used for a different
	// request (another body, method or path). 409 CONFLICT.
	ReasonIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"
	// ReasonIdempotencyKeyInFlight: the first request with this key is still running.
	// 409 CONFLICT; the client retries a moment later.
	ReasonIdempotencyKeyInFlight = "IDEMPOTENCY_KEY_IN_FLIGHT"
	// ReasonOccurredAtInFuture: an offline item claims to have happened more than 5 minutes
	// from now. 422 UNPROCESSABLE.
	ReasonOccurredAtInFuture = "OCCURRED_AT_IN_FUTURE"
	// ReasonOccurredAtTooOld: an offline item is older than the 7-day offline window.
	// 422 UNPROCESSABLE.
	ReasonOccurredAtTooOld = "OCCURRED_AT_TOO_OLD"
)
