package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/notifications/push"
	"siapp/pkg/platform/database"
)

// TaskPushDeliver is the asynq task that sends the pushes of one Notify call.
const TaskPushDeliver = "push:deliver"

// The asynq queues. Panic goes on the critical one so that a backlog of ordinary pushes (a
// report that notified a large regu) never stands between a siren and the phones.
const (
	QueueCritical = "critical"
	QueueDefault  = "default"
)

const (
	pushMaxRetry = 3
	// pushChunk is how many messages one provider call carries.
	pushChunk = 500
)

// Queues is the worker's queue weights: critical gets twice the share of default.
func Queues() map[string]int { return map[string]int{QueueCritical: 6, QueueDefault: 3} }

// pushPayload is the task payload. It names rows, not texts: the worker reads the notifications
// again, so a row deleted in between is not pushed, and the payload stays small for a big regu.
type pushPayload struct {
	OrganizationID  int64   `json:"organization_id"`
	NotificationIDs []int64 `json:"notification_ids"`
}

// Enqueuer is the part of *asynq.Client the dispatcher uses.
type Enqueuer interface {
	EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// PushDispatcher is the Dispatcher of Phase 05.2: it queues one push:deliver task per Notify
// call and leaves the sending to the worker, so the request that caused the notification never
// waits for FCM.
type PushDispatcher struct {
	enqueuer Enqueuer
}

// NewPushDispatcher returns a dispatcher that enqueues on e.
func NewPushDispatcher(e Enqueuer) *PushDispatcher { return &PushDispatcher{enqueuer: e} }

// Dispatch implements Dispatcher.
func (d *PushDispatcher) Dispatch(ctx context.Context, organizationID int64, sent []Notification) error {
	if len(sent) == 0 {
		return nil
	}
	payload := pushPayload{OrganizationID: organizationID, NotificationIDs: make([]int64, len(sent))}
	queue := QueueDefault
	for i, n := range sent {
		payload.NotificationIDs[i] = n.ID
		if n.Priority == PriorityHigh {
			queue = QueueCritical
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode push task: %w", err)
	}
	if _, err := d.enqueuer.EnqueueContext(ctx, asynq.NewTask(TaskPushDeliver, raw),
		asynq.Queue(queue), asynq.MaxRetry(pushMaxRetry)); err != nil {
		return fmt.Errorf("enqueue push task: %w", err)
	}
	return nil
}

// PushDeliverer is the worker's handler of push:deliver.
type PushDeliverer struct {
	pool     *pgxpool.Pool
	provider push.Provider
	logger   *slog.Logger
}

// NewPushDeliverer returns the handler.
func NewPushDeliverer(pool *pgxpool.Pool, provider push.Provider, logger *slog.Logger) *PushDeliverer {
	return &PushDeliverer{pool: pool, provider: provider, logger: logger}
}

// RegisterPushJob adds push:deliver to the worker's mux.
func RegisterPushJob(mux *asynq.ServeMux, d *PushDeliverer) { mux.Handle(TaskPushDeliver, d) }

// ProcessTask implements asynq.Handler.
func (d *PushDeliverer) ProcessTask(ctx context.Context, task *asynq.Task) error {
	var p pushPayload
	if err := json.Unmarshal(task.Payload(), &p); err != nil || p.OrganizationID < 1 {
		// Retrying cannot fix a payload that does not parse.
		return fmt.Errorf("push: bad payload: %w", asynq.SkipRetry)
	}
	return d.Deliver(ctx, p.OrganizationID, p.NotificationIDs)
}

// Deliver pushes the notifications to every device of their recipients.
//
// The database work is two short tenant transactions, one before and one after the sending: a
// transaction held open across 500 HTTP calls would pin a pooled connection for as long as FCM
// is slow.
func (d *PushDeliverer) Deliver(ctx context.Context, organizationID int64, ids []int64) error {
	var (
		items   []Notification
		devices []db.ListPushDevicesRow
	)
	err := database.WithTenant(ctx, d.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListNotificationsByIDs(ctx, ids)
		if err != nil {
			return err
		}
		if items, err = toNotifications(rows); err != nil {
			return err
		}
		recipients := make([]int64, 0, len(items))
		for _, n := range items {
			recipients = append(recipients, n.RecipientID)
		}
		devices, err = q.ListPushDevices(ctx, recipients)
		return err
	})
	if err != nil {
		return fmt.Errorf("push: load: %w", err)
	}

	byRecipient := make(map[int64][]db.ListPushDevicesRow)
	for _, dev := range devices {
		byRecipient[dev.PersonnelID] = append(byRecipient[dev.PersonnelID], dev)
	}
	var (
		msgs      []push.Message
		deviceIDs []int64 // deviceIDs[i] is the device msgs[i] goes to
	)
	for _, n := range items {
		for _, dev := range byRecipient[n.RecipientID] {
			msgs = append(msgs, pushMessage(n, dev))
			deviceIDs = append(deviceIDs, dev.ID)
		}
	}
	if len(msgs) == 0 {
		return nil
	}

	var (
		invalid []int64
		failed  int
		sendErr error
	)
	for start := 0; start < len(msgs); start += pushChunk {
		end := min(start+pushChunk, len(msgs))
		results, err := d.provider.Send(ctx, msgs[start:end])
		if err != nil {
			// The provider refused the whole call (credentials, outage). The chunks after this one
			// would fail the same way.
			sendErr = err
			failed += len(msgs) - start
			break
		}
		for i, r := range results {
			switch {
			case r.InvalidToken:
				invalid = append(invalid, deviceIDs[start+i])
			case r.Err != nil:
				failed++
			}
		}
	}

	if len(invalid) > 0 {
		if err := database.WithTenant(ctx, d.pool, organizationID, func(q *db.Queries) error {
			return q.DeleteDevicesByIDs(ctx, invalid)
		}); err != nil {
			return fmt.Errorf("push: delete invalid tokens: %w", err)
		}
		d.logger.InfoContext(ctx, "push: removed devices with invalid tokens", slog.Int("devices", len(invalid)))
	}
	if failed > 0 {
		// ⚠ Returning the error makes asynq retry the whole task, which sends again to the phones
		// that already got it. That is harmless: every message carries the notification's
		// collapse key, so the phone replaces the first one with the second.
		if sendErr == nil {
			sendErr = errors.New("some pushes were not accepted")
		}
		return fmt.Errorf("push: %d of %d not delivered: %w", failed, len(msgs), sendErr)
	}
	return nil
}

// pushMessage builds the push of one notification for one device. Data carries ids and codes
// for the deep link and nothing about the person: no NIK, and no name beyond the rendered text.
func pushMessage(n Notification, dev db.ListPushDevicesRow) push.Message {
	data := map[string]string{
		"notification_id": strconv.FormatInt(n.ID, 10),
		"type":            string(n.Type),
	}
	if n.EntityType != nil && n.EntityID != nil {
		data["entity_type"] = *n.EntityType
		data["entity_id"] = strconv.FormatInt(*n.EntityID, 10)
	}
	return push.Message{
		Token:       dev.PushToken,
		Platform:    push.Platform(dev.Platform),
		Title:       n.Title,
		Body:        n.Body,
		Data:        data,
		CollapseKey: "n" + strconv.FormatInt(n.ID, 10),
		High:        n.Priority == PriorityHigh,
	}
}
