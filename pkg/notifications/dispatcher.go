package notifications

import "context"

// Dispatcher delivers notifications that Notify has just written: Phase 05.2 sends them as push
// notifications and Phase 05.3 publishes them to the recipients' realtime rooms. Until a phase
// plugs one in (Service.UseDispatcher), the default does nothing and the inbox is all there is.
//
// sent carries the stored rows, not only their ids, because the two deliveries need more than an
// id: the push queue is chosen by Priority, and the realtime room by the recipient.
//
// An error is logged by Notify and goes no further: the rows are already saved, and the person
// finds them in the inbox whatever the delivery did.
type Dispatcher interface {
	Dispatch(ctx context.Context, organizationID int64, sent []Notification) error
}

type noopDispatcher struct{}

func (noopDispatcher) Dispatch(context.Context, int64, []Notification) error { return nil }
