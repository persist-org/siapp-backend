package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

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
	// sent remembers which (notification, device) pairs already got their push, so a retry skips
	// them. Nil: nothing is remembered and a retry sends to everyone again.
	sent *sentLog
}

// NewPushDeliverer returns the handler.
func NewPushDeliverer(pool *pgxpool.Pool, provider push.Provider, logger *slog.Logger) *PushDeliverer {
	return &PushDeliverer{pool: pool, provider: provider, logger: logger}
}

// UseSentLog makes retries idempotent: the pairs (notification, device) that were accepted are
// kept in Redis for an hour, and a retried task sends only to the devices that are missing.
// Without it a task that fails for ONE phone sends again to all of them, and for a panic that
// means the siren sounds twice on every phone that already had it (a collapse key replaces the
// banner, not the sound). Call it while wiring, before the worker starts.
func (d *PushDeliverer) UseSentLog(rdb redis.UniversalClient, prefix string) {
	d.sent = &sentLog{rdb: rdb, prefix: prefix, logger: d.logger}
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
		notifIDs  []int64 // notifIDs[i] is the notification msgs[i] carries
	)
	for _, n := range items {
		for _, dev := range byRecipient[n.RecipientID] {
			msgs = append(msgs, pushMessage(n, dev))
			deviceIDs = append(deviceIDs, dev.ID)
			notifIDs = append(notifIDs, n.ID)
		}
	}
	// A retry: leave out the pairs that already got their push.
	if done := d.sent.alreadySent(ctx, notifIDs, deviceIDs); len(done) > 0 {
		var m []push.Message
		var dv, nv []int64
		for i := range msgs {
			if !done[i] {
				m, dv, nv = append(m, msgs[i]), append(dv, deviceIDs[i]), append(nv, notifIDs[i])
			}
		}
		msgs, deviceIDs, notifIDs = m, dv, nv
	}
	if len(msgs) == 0 {
		return nil
	}

	var (
		invalid   []int64
		transient int
		permanent int
		sendErr   error
		firstPerm error
	)
	for start := 0; start < len(msgs); start += pushChunk {
		end := min(start+pushChunk, len(msgs))
		results, err := d.provider.Send(ctx, msgs[start:end])
		if err != nil {
			// The provider refused the whole call (credentials, outage). The chunks after this one
			// would fail the same way.
			sendErr = err
			transient += len(msgs) - start
			break
		}
		var accepted []int
		for i, r := range results {
			switch {
			case r.InvalidToken:
				invalid = append(invalid, deviceIDs[start+i])
			case r.Err != nil && r.Permanent:
				permanent++
				if firstPerm == nil {
					firstPerm = r.Err
				}
			case r.Err != nil:
				transient++
				if sendErr == nil {
					sendErr = r.Err
				}
			default:
				accepted = append(accepted, start+i)
			}
		}
		d.sent.markSent(ctx, notifIDs, deviceIDs, accepted)
	}

	if len(invalid) > 0 {
		if err := database.WithTenant(ctx, d.pool, organizationID, func(q *db.Queries) error {
			return q.DeleteDevicesByIDs(ctx, invalid)
		}); err != nil {
			return fmt.Errorf("push: delete invalid tokens: %w", err)
		}
		d.logger.InfoContext(ctx, "push: removed devices with invalid tokens", slog.Int("devices", len(invalid)))
	}
	if permanent > 0 {
		// Retrying a push that FCM rejects for good would only repeat the rejection, so it is
		// logged (loudly: it usually means a misconfiguration) and does not fail the task.
		d.logger.ErrorContext(ctx, "push: some pushes were rejected for good and will not be retried",
			slog.Int("pushes", permanent), slog.Any("error", firstPerm))
	}
	if transient > 0 {
		// The task is retried by asynq. The pairs that were accepted are in the sent log, so the
		// retry goes only to the devices that are still missing.
		return fmt.Errorf("push: %d of %d not delivered, will retry: %w", transient, len(msgs), sendErr)
	}
	return nil
}

// sentLog is the Redis record of pushes already accepted. Every method is safe on a nil
// receiver and fails open: if Redis cannot answer, the worst case is a duplicate push, which is
// better than a push that is never sent.
type sentLog struct {
	rdb    redis.UniversalClient
	prefix string
	logger *slog.Logger
}

const sentLogTTL = time.Hour

func (l *sentLog) key(notificationID, deviceID int64) string {
	return l.prefix + "push-sent:" + strconv.FormatInt(notificationID, 10) + ":" + strconv.FormatInt(deviceID, 10)
}

// alreadySent returns, for each pair, whether it was accepted before. It returns nil when it
// cannot tell.
func (l *sentLog) alreadySent(ctx context.Context, notificationIDs, deviceIDs []int64) map[int]bool {
	if l == nil || len(notificationIDs) == 0 {
		return nil
	}
	keys := make([]string, len(notificationIDs))
	for i := range keys {
		keys[i] = l.key(notificationIDs[i], deviceIDs[i])
	}
	vals, err := l.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		l.logger.WarnContext(ctx, "push: cannot read the sent log, sending to everyone", slog.Any("error", err))
		return nil
	}
	done := map[int]bool{}
	for i, v := range vals {
		if v != nil {
			done[i] = true
		}
	}
	return done
}

// markSent records the pairs at the given indexes as accepted.
func (l *sentLog) markSent(ctx context.Context, notificationIDs, deviceIDs []int64, idx []int) {
	if l == nil || len(idx) == 0 {
		return
	}
	pipe := l.rdb.Pipeline()
	for _, i := range idx {
		pipe.Set(ctx, l.key(notificationIDs[i], deviceIDs[i]), "1", sentLogTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		l.logger.WarnContext(ctx, "push: cannot write the sent log, a retry may send twice", slog.Any("error", err))
	}
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
