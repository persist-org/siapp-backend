package apperr

import "net/http"

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
	// ReasonPersonelUnavailable: Personalia could not answer (outage, timeout, open circuit
	// breaker, rejected integration key). 503 INTERNAL_ERROR: the contract's ErrorCode enum
	// has no 503 code, and the client treats it as a server-side failure to retry later.
	ReasonPersonelUnavailable = "PERSONEL_UNAVAILABLE"
)

// ErrPersonelUnavailable is what a handler returns for personel.ErrUnavailable.
var ErrPersonelUnavailable = New(http.StatusServiceUnavailable, CodeInternal, ReasonPersonelUnavailable,
	"Layanan Personalia sedang tidak tersedia. Coba lagi sebentar lagi.")
