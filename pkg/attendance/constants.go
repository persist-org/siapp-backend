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
)
