package realtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"siapp/pkg/notifications"
)

func newMsgID() string { return uuid.NewString() }

// NotificationDispatcher is the realtime half of Phase 05.3's dispatcher: it publishes
// notification.created to each recipient's personal room, so an open app updates its inbox and
// badge without waiting for a push. Compose it with the push dispatcher (notifications.Dispatchers).
type NotificationDispatcher struct {
	publisher Publisher
}

// NewNotificationDispatcher returns the dispatcher.
func NewNotificationDispatcher(p Publisher) *NotificationDispatcher {
	return &NotificationDispatcher{publisher: p}
}

// Dispatch implements notifications.Dispatcher.
//
// The frame names the notification and its deep link and carries no text: the title and body
// can hold a person's name, and no name goes through a socket. The app fetches the row (or the
// delta) over REST, which is where masking and audit live.
func (d *NotificationDispatcher) Dispatch(ctx context.Context, organizationID int64, sent []notifications.Notification) error {
	var errs []error
	for _, n := range sent {
		data := map[string]any{
			"notification_id": n.ID,
			"type":            string(n.Type),
			"priority":        string(n.Priority),
			"created_at":      n.CreatedAt.UTC(),
		}
		if n.EntityType != nil && n.EntityID != nil {
			data["entity_type"] = *n.EntityType
			data["entity_id"] = *n.EntityID
		}
		if err := d.publisher.Publish(ctx, organizationID, PersonalRoom(n.RecipientID), "notification.created", data); err != nil {
			errs = append(errs, fmt.Errorf("notification %s: %w", strconv.FormatInt(n.ID, 10), err))
		}
	}
	return errors.Join(errs...)
}

var _ notifications.Dispatcher = (*NotificationDispatcher)(nil)
