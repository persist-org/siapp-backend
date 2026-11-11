// Package location is the GPS side of module 08 (Phase 08.2): devices send points, the live
// position reaches the DANRU and the web monitor, and the maps can be queried.
package location

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason), the plan's fixed catalog for the phase.
const (
	ReasonBatchTooLarge    = "LOCATION_BATCH_TOO_LARGE"
	ReasonTrackingDisabled = "TRACKING_DISABLED"
)

// ErrBatchTooLarge is returned for a batch of more than MaxBatch points.
var ErrBatchTooLarge = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonBatchTooLarge, "Maksimum 100 titik lokasi per permintaan. Kirim dalam beberapa bagian.")

// ErrTrackingDisabled is returned when the person is not on a shift at a project that tracks GPS.
// It is the privacy contract in one error: nobody's position is stored outside their working
// hours, or at a project the organization has not switched tracking on for.
var ErrTrackingDisabled = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonTrackingDisabled, "Pelacakan lokasi tidak aktif: Anda tidak sedang bertugas di proyek yang mengaktifkan pelacakan GPS.")
