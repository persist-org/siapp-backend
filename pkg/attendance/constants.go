package attendance

const (
	// entityType is how the photos of an attendance are attached (media.AttachInput.EntityType).
	entityType = "ATTENDANCE"

	// selfiesPerCapture is how many photos a selfie check-in or check-out carries: exactly one.
	selfiesPerCapture = 1
)

// Event types of the Personel outbox (payload "type") that Phase 07.1 writes. Phase 07.6 adds
// ABSENT, ABSENT_CANCELLED, CORRECTED and GROUP_PRESENT.
const (
	eventCheckIn         = "CHECK_IN"
	eventCheckOut        = "CHECK_OUT"
	eventCheckOutUpdated = "CHECK_OUT_UPDATED"

	// eventAbsent is written whenever a record is freshly turned ABSENT: the DANRU's
	// POST /attendances/absent, or an admin correction setting status ABSENT.
	eventAbsent = "ABSENT"
	// eventAbsentCancelled is written by the DANRU's own DELETE /attendances/absent/{id}, undoing
	// a mistaken mangkir: the row is deleted, not just changed, so the event is built from it
	// before it goes.
	eventAbsentCancelled = "ABSENT_CANCELLED"
	// eventGroupPresent is written for a GROUP-method check-in (the DANRU's group check-in).
	eventGroupPresent = "GROUP_PRESENT"
	// eventCorrected is the one event an admin correction (PATCH /admin/attendances/{id}) ever
	// writes, whatever it changed: it says "this record was corrected", not which transition.
	eventCorrected = "CORRECTED"
)
