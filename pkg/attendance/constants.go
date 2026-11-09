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
	// POST /attendances/absent, a group-present entry marked ABSENT, or an admin correction
	// turning a PRESENT/no record into ABSENT. The reason is actor-independent: Personel only
	// needs to know the day became ABSENT, not who decided it.
	eventAbsent = "ABSENT"
	// eventAbsentCancelled is written only by an admin correction, undoing a mangkir (ABSENT back
	// to PRESENT). There is no field-side "undo" for this.
	eventAbsentCancelled = "ABSENT_CANCELLED"
	// eventGroupPresent is written for a GROUP-method check-in (a DANRU's group-present entry
	// marked PRESENT).
	eventGroupPresent = "GROUP_PRESENT"
	// eventCorrected is written by an admin correction that edits check_in/check_out on an
	// already-PRESENT record without touching absence status.
	eventCorrected = "CORRECTED"
)
