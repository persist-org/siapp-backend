// Package patrol is module 10: QR-tagged zones, the checklist each zone asks of the guard, scans
// and the patrol route. Phase 10.1 is the zones and their checklists; 10.2 the scans and the
// check-in/out of a zone; 10.3 progress and approval; 10.4 the patrol route, the line a guard walks.
package patrol

import (
	"fmt"
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Module reasons (details.reason). A reason that has shipped is never renamed. The plan's
// catalog for the module is in docs/plan/10-patrol.md; a phase adds the ones it uses.
const (
	ReasonZoneNotFound = "ZONE_NOT_FOUND"

	ReasonNotInProject         = "ZONE_NOT_IN_PROJECT"
	ReasonIntervalNotElapsed   = "ZONE_INTERVAL_NOT_ELAPSED"
	ReasonChecklistItemInvalid = "CHECKLIST_ITEM_INVALID"
	ReasonAnswerRequired       = "CHECKLIST_ANSWER_REQUIRED"
	ReasonAnswerInvalid        = "CHECKLIST_ANSWER_INVALID"
	ReasonAttachmentRequired   = "CHECKLIST_ATTACHMENT_REQUIRED"
	ReasonNotCheckedIn         = "ZONE_NOT_CHECKED_IN"
	ReasonAlreadyCheckedIn     = "ZONE_ALREADY_CHECKED_IN" // not in the plan's list: "check in twice fails" needs a reason
	ReasonLocalIDTaken         = "SCAN_LOCAL_ID_TAKEN"
	ReasonZoneNameTaken        = "ZONE_NAME_TAKEN"
)

// ErrZoneNotFound is returned for a zone that does not exist, is deleted or switched off, or
// belongs to another organization (row-level security makes that look the same as the first).
var ErrZoneNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonZoneNotFound, "Zona tidak ditemukan.")

// ErrNotInProject is returned to a person who does not serve in the zone's project (or regu).
var ErrNotInProject = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonNotInProject, "Anda tidak bertugas di proyek atau regu zona ini.")

// ErrIntervalNotElapsed is returned for a second scan of a checkpoint inside the project's patrol
// interval (DL-48). The reason keeps its zone name: a reason that has shipped is never renamed.
var ErrIntervalNotElapsed = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonIntervalNotElapsed, "Zona ini baru saja di-scan. Tunggu sampai interval patroli terlewati.")

// ErrNotCheckedIn is returned for a check-out of a zone the person is not checked in to.
var ErrNotCheckedIn = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonNotCheckedIn, "Anda belum check-in di zona ini.")

// ErrAlreadyCheckedIn is returned for a check-in while the person is still checked in, here or in
// another zone: a guard is at one post at a time.
var ErrAlreadyCheckedIn = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonAlreadyCheckedIn, "Anda masih check-in di sebuah zona. Check-out lebih dulu.")

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

// Reasons of Phase 10.7 (checkpoints; docs/plan/10.1-patrol-update.md).
const (
	ReasonCheckpointNotFound      = "CHECKPOINT_NOT_FOUND"
	ReasonCheckpointNameTaken     = "CHECKPOINT_NAME_TAKEN"
	ReasonCheckpointActionInvalid = "CHECKPOINT_ACTION_INVALID"
)

// ErrCheckpointNotFound is returned for a checkpoint that does not exist, is deleted, belongs to
// another organization (row-level security makes that look the same), or whose id is not a uuid.
var ErrCheckpointNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonCheckpointNotFound, "Checkpoint tidak ditemukan.")

// ErrCheckpointNameTaken is returned when another live checkpoint of the same project has the name.
var ErrCheckpointNameTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonCheckpointNameTaken, "Sudah ada checkpoint dengan nama ini di proyek yang sama.")

// ReasonCheckpointNotInProject is returned by Phase 10.8 for a checkpoint that cannot go into a zone.
const ReasonCheckpointNotInProject = "CHECKPOINT_NOT_IN_PROJECT"

// checkpointNotInProject is a 422 for the i-th id of checkpoint_ids that cannot be put in the zone: it
// does not exist, is deleted, belongs to another organization (the same answer, so an id says nothing
// about other tenants), is in another project than the zone, or is inactive and not already in the zone.
func checkpointNotInProject(i int, id string) *apperr.Error {
	return apperr.ErrValidation.WithReason(ReasonCheckpointNotInProject).
		WithFields(apperr.FieldError{
			Field: fmt.Sprintf("checkpoint_ids[%d]", i), Code: "INVALID",
			Message: "Bukan checkpoint aktif di proyek zona ini.",
		}).
		WithDetails(map[string]any{"checkpoint_id": id})
}

// actionInvalid is a 422 for a bad checkpoint action: a validation error that also carries the
// reason, so a client can tell a bad checklist from any other bad field.
func actionInvalid(fields ...apperr.FieldError) *apperr.Error {
	return apperr.ErrValidation.WithReason(ReasonCheckpointActionInvalid).WithFields(fields...)
}

// Reasons of Phase 10.9 (the scan at a checkpoint).
const (
	ReasonCheckpointNotInZone     = "CHECKPOINT_NOT_IN_ZONE"
	ReasonCheckpointQRInvalid     = "CHECKPOINT_QR_INVALID"
	ReasonCheckpointOutsideRadius = "CHECKPOINT_OUTSIDE_RADIUS"
	ReasonCheckpointOutOfOrder    = "CHECKPOINT_OUT_OF_ORDER"
)

// ErrCheckpointNotInZone is returned for a checkpoint that a guard cannot scan in this zone: it is not one
// of the zone's (any more), it is switched off, or it does not exist. One answer for all three, so an id
// says nothing about checkpoints of other zones.
var ErrCheckpointNotInZone = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonCheckpointNotInZone, "Checkpoint ini bukan bagian dari zona ini.")

// ErrCheckpointQRInvalid is returned for a QR code that is not this checkpoint's: not a checkpoint code at
// all, of another checkpoint, of another organization, or with an old secret (the sticker was replaced).
var ErrCheckpointQRInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonCheckpointQRInvalid, "QR code tidak valid untuk checkpoint ini.")

// ErrCheckpointOutsideRadius is returned for a scan or check-in made away from the checkpoint.
var ErrCheckpointOutsideRadius = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
	ReasonCheckpointOutsideRadius, "Anda berada di luar area checkpoint.")

// ErrCheckpointOutOfOrder is returned for a checkpoint scanned before the one the zone's order puts first
// (docs/plan/10.1-patrol-update.md, U3). details.expected_checkpoint_id names the one to scan.
//
// ⚠ It is 409, not 422 like the other refusals of a scan, on purpose. The idempotency middleware keeps the
// answer of a 4xx under the request's key (the client would get the same answer again) but not of a 409,
// which it takes for transient. This refusal is transient: an offline queue is sent in order, and the same
// request, with the same local_id and key, is accepted once the earlier scans have arrived. A 422 would
// hand the app its own stored refusal back for good.
var ErrCheckpointOutOfOrder = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonCheckpointOutOfOrder, "Scan checkpoint sebelumnya lebih dulu.")

// actionError builds the error of one checkpoint action: the reason says what is wrong, the details name the
// action, and the message names it for a human.
func actionError(base *apperr.Error, actionID, name, why string) *apperr.Error {
	msg := base.Message
	if why != "" {
		msg = why
	}
	if name != "" {
		msg = "Aksi \"" + name + "\": " + msg
	}
	return base.WithMessage(msg).WithDetails(map[string]any{"action_id": actionID})
}

var (
	errChecklistItemInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonChecklistItemInvalid, "Aksi tidak valid untuk checkpoint ini.")
	errAnswerRequired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAnswerRequired, "Wajib dijawab.")
	errAnswerInvalid = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAnswerInvalid, "Jawaban tidak valid.")
	errAttachmentRequired = apperr.New(http.StatusUnprocessableEntity, apperr.CodeUnprocessable,
		ReasonAttachmentRequired, "Foto atau catatan wajib dilampirkan.")
)
