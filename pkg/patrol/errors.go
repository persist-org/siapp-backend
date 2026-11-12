// Package patrol is module 10: QR-tagged zones, the checklist each zone asks of the guard, scans
// and the patrol route. Phase 10.1 is the zones and their checklists; scans, approval and the
// route arrive in the phases after it.
package patrol

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A reason that has shipped is never renamed. The plan's
// catalog for the module is in docs/plan/10-patrol.md; a phase adds the ones it uses.
const (
	ReasonZoneNotFound = "ZONE_NOT_FOUND"
)

// ErrZoneNotFound is returned for a zone that does not exist, is deleted, or belongs to another
// organization (row-level security makes the last two look the same as the first).
var ErrZoneNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonZoneNotFound, "Zona tidak ditemukan.")
