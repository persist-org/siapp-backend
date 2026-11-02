package notifications

import "time"

const (
	// deltaOverlap is how far back the server pulls the lower bound of a delta request.
	//
	// ⚠ A row's updated_at is the time its statement ran, but the row becomes visible only when its
	// transaction commits, a few milliseconds later (more under load, and the API instances'
	// clocks differ by a little). A pull that arrives in that gap gets a server_time later than
	// the row's updated_at and never asks for it again: the app would silently miss, say, a panic.
	// Starting each pull this much before `since` closes the gap. The price is that a change from
	// the last few seconds can arrive twice, which is harmless: the app upserts by id.
	deltaOverlap = 5 * time.Second

	// notifyTimeout bounds one Notify. It runs on a context that outlives the request (see
	// Service.Notify), so it needs a limit of its own, or a stuck database would hold the
	// goroutine of every caller for ever.
	notifyTimeout = 10 * time.Second

	// maxCursorLength caps what decodeCursor will even look at. A real cursor is under 40 bytes.
	maxCursorLength = 128
)
