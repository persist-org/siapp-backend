// Package patrol is module 10: QR-tagged zones, the checklist each zone asks of the guard, scans
// and the patrol route. Phase 10.1 is the zones and their checklists; 10.2 the scans and the
// check-in/out of a zone; 10.3 progress and approval; 10.4 the patrol route, the line a guard walks.
package patrol

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A reason that has shipped is never renamed. The plan's
// catalog for the module is in docs/plan/10-patrol.md; a phase adds the ones it uses.
const (
	ReasonZoneNotFound = "ZONE_NOT_FOUND"

	ReasonQRInvalid            = "ZONE_QR_INVALID"
	ReasonNotInProject         = "ZONE_NOT_IN_PROJECT"
	ReasonOutsideRadius        = "ZONE_OUTSIDE_RADIUS"
	ReasonIntervalNotElapsed   = "ZONE_INTERVAL_NOT_ELAPSED"
	ReasonChecklistItemInvalid = "CHECKLIST_ITEM_INVALID"
	ReasonAnswerRequired       = "CHECKLIST_ANSWER_REQUIRED"
	ReasonAnswerInvalid        = "CHECKLIST_ANSWER_INVALID"
	ReasonAttachmentRequired   = "CHECKLIST_ATTACHMENT_REQUIRED"
	ReasonPhotoRequired        = "SCAN_PHOTO_REQUIRED"
	ReasonNotCheckedIn         = "ZONE_NOT_CHECKED_IN"
	ReasonAlreadyCheckedIn     = "ZONE_ALREADY_CHECKED_IN" // not in the plan's list: "check in twice fails" needs a reason
	ReasonLocalIDTaken         = "SCAN_LOCAL_ID_TAKEN"
	ReasonZoneNameTaken        = "ZONE_NAME_TAKEN"
)

// ErrZoneNotFound is returned for a zone that does not exist, is deleted or switched off, or
// belongs to another organization (row-level security makes that look the same as the first).
var ErrZoneNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonZoneNotFound, "Zona tidak ditemukan.")

// ErrQRInvalid is returned for a QR code that is not this zone's: not a zone code at all, of
// another zone, of another organization, or with an old secret (the sticker was replaced).
var ErrQRInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonQRInvalid, "QR code tidak valid untuk zona ini.")

// ErrNotInProject is returned to a person who does not serve in the zone's project (or regu).
var ErrNotInProject = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonNotInProject, "Anda tidak bertugas di proyek atau regu zona ini.")

// ErrOutsideRadius is returned for a scan or check-in made away from the zone.
var ErrOutsideRadius = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonOutsideRadius, "Anda berada diluar zona patroli.")

// ErrIntervalNotElapsed is returned for a second scan of a zone inside the project's patrol
// interval (DL-48).
var ErrIntervalNotElapsed = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonIntervalNotElapsed, "Zona ini baru saja di-scan. Tunggu sampai interval patroli terlewati.")

// ErrNotCheckedIn is returned for a check-out of a zone the person is not checked in to.
var ErrNotCheckedIn = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonNotCheckedIn, "Anda belum check-in di zona ini.")

// ErrAlreadyCheckedIn is returned for a check-in while the person is still checked in, here or in
// another zone: a guard is at one post at a time.
var ErrAlreadyCheckedIn = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonAlreadyCheckedIn, "Anda masih check-in di sebuah zona. Check-out lebih dulu.")

// ErrPhotoRequired is returned for a scan with no photo in a zone that demands one.
var ErrPhotoRequired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPhotoRequired, "Zona ini mewajibkan minimal satu foto.")

// Reasons of Phase 10.3.
const (
	ReasonScanNotFound       = "SCAN_NOT_FOUND"
	ReasonScanAlreadyDecided = "SCAN_ALREADY_DECIDED"
)

// ErrScanNotFound is returned for a scan that does not exist, belongs to another organization, or
// is not one the caller may read (a scan of another regu is answered like one that is not there).
var ErrScanNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonScanNotFound, "Scan tidak ditemukan.")

// ErrScanAlreadyDecided is returned for an approve or decline of a scan that is no longer SUBMITTED.
var ErrScanAlreadyDecided = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonScanAlreadyDecided, "Scan ini sudah diputuskan.")

// Reasons of Phase 10.4.
const (
	ReasonPatrolNotFound      = "PATROL_NOT_FOUND"
	ReasonPatrolAlreadyActive = "PATROL_ALREADY_ACTIVE"
	ReasonPatrolNotActive     = "PATROL_NOT_ACTIVE"
	ReasonPointsTooLarge      = "PATROL_POINTS_TOO_LARGE"
	ReasonPatrolLocalIDTaken  = "PATROL_LOCAL_ID_TAKEN" // not in the plan's list, like SCAN_LOCAL_ID_TAKEN
)

// ErrPatrolNotFound is returned for a patrol that does not exist, belongs to another organization,
// or is not one the caller may read: another person's patrol is answered like one that is not there.
var ErrPatrolNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonPatrolNotFound, "Patroli tidak ditemukan.")

// ErrPatrolAlreadyActive is returned for a start while the person's other patrol still runs.
var ErrPatrolAlreadyActive = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonPatrolAlreadyActive, "Anda masih punya patroli yang berjalan. Akhiri lebih dulu.")

// ErrPatrolNotActive is returned for points sent to a patrol that has ended.
var ErrPatrolNotActive = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonPatrolNotActive, "Patroli ini sudah berakhir.")

// ErrPointsTooLarge is returned for a batch of more than MaxRoutePoints points.
var ErrPointsTooLarge = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonPointsTooLarge, "Terlalu banyak titik dalam satu permintaan.")

// ErrZoneNameTaken is returned when another live zone of the same project and type has the name.
var ErrZoneNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonZoneNameTaken, "Sudah ada zona dengan nama ini di proyek dan tipe yang sama.")

// ErrPatrolLocalIDTaken is returned when a patrol's local_id was already used by another person: it
// is the app's own id, so this is a bug or a forgery, never a replay.
var ErrPatrolLocalIDTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonPatrolLocalIDTaken, "local_id sudah dipakai untuk patroli lain.")

// ErrLocalIDTaken is returned when a scan's local_id was already used by another person or for
// another zone: it is the app's own id, so this is a bug or a forgery, never a replay.
var ErrLocalIDTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonLocalIDTaken, "local_id sudah dipakai untuk scan lain.")

// itemError builds the error of one checklist item: the reason says what is wrong, the details name
// the item, and the message names it for a human.
func itemError(base *apperr.Error, itemID int64, label, why string) *apperr.Error {
	msg := base.Message
	if why != "" {
		msg = why
	}
	if label != "" {
		msg = "Butir \"" + label + "\": " + msg
	}
	return base.WithMessage(msg).WithDetails(map[string]any{"item_id": itemID})
}

var (
	errChecklistItemInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonChecklistItemInvalid, "Butir checklist tidak valid untuk zona ini.")
	errAnswerRequired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAnswerRequired, "Wajib dijawab.")
	errAnswerInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAnswerInvalid, "Jawaban tidak valid.")
	errAttachmentRequired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAttachmentRequired, "Foto atau catatan wajib dilampirkan.")
)
