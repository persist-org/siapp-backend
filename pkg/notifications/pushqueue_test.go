package notifications_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
	"siapp/pkg/notifications/push"
	"siapp/pkg/platform/testkit"
)

// fakeQueue records the tasks the dispatcher enqueues, so a test can run the handler on them.
type fakeQueue struct {
	mu    sync.Mutex
	tasks []*asynq.Task
	queue []string
}

func (q *fakeQueue) EnqueueContext(_ context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = append(q.tasks, task)
	name := ""
	for _, o := range opts {
		if o.Type() == asynq.QueueOpt {
			name, _ = o.Value().(string)
		}
	}
	q.queue = append(q.queue, name)
	return &asynq.TaskInfo{}, nil
}

// fakeProvider records every call and answers from a map of tokens.
type fakeProvider struct {
	mu      sync.Mutex
	calls   [][]push.Message
	invalid map[string]bool
	failing map[string]bool // a failure that may pass: worth a retry
	refused map[string]bool // a failure that will not: FCM rejects it for good
	err     error
}

func (p *fakeProvider) Send(_ context.Context, msgs []push.Message) ([]push.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, msgs)
	if p.err != nil {
		return nil, p.err
	}
	out := make([]push.Result, len(msgs))
	for i, m := range msgs {
		out[i] = push.Result{InvalidToken: p.invalid[m.Token]}
		if p.failing[m.Token] {
			out[i].Err = errors.New("unavailable")
		}
		if p.refused[m.Token] {
			out[i] = push.Result{Err: errors.New("sender id mismatch"), Permanent: true}
		}
	}
	return out, nil
}

func (p *fakeProvider) sent() (out []push.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.calls {
		out = append(out, c...)
	}
	return out
}

func (p *fakeProvider) tokens() (out []string) {
	for _, m := range p.sent() {
		out = append(out, m.Token)
	}
	return out
}

// pushFlow is the inbox flow with the push dispatcher in place of the fake one.
type pushFlow struct {
	*flow
	queue     *fakeQueue
	provider  *fakeProvider
	deliverer *notifications.PushDeliverer
}

func newPushFlow(t *testing.T) *pushFlow {
	t.Helper()
	f := newFlow(t)
	pf := &pushFlow{flow: f, queue: &fakeQueue{}, provider: &fakeProvider{}}
	f.service.UseDispatcher(notifications.NewPushDispatcher(pf.queue))
	pf.deliverer = notifications.NewPushDeliverer(testkit.Pool(t), pf.provider, slog.New(slog.NewJSONHandler(f.logs, nil)))
	pf.deliverer.UseSentLog(testkit.Redis(t), testkit.RandomPrefix(t))
	return pf
}

// device signs a phone in for a person: a session and the device that hangs on it.
func (f *pushFlow) device(org, personnelID int64, token string) {
	f.t.Helper()
	f.exec(org, `
		WITH s AS (
			INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, client, expires_at)
			VALUES ($1, gen_random_uuid(), md5(random()::text) || md5(random()::text), 'MOBILE', now() + interval '1 day')
			RETURNING id)
		INSERT INTO devices (personnel_id, session_id, platform, push_token)
		SELECT $1, id, 'ANDROID', $2 FROM s`, personnelID, token)
}

func (f *pushFlow) deviceTokens(org int64) (out []string) {
	f.t.Helper()
	f.inTenant(org, func(tx pgx.Tx) {
		rows, err := tx.Query(f.t.Context(), "SELECT push_token FROM devices ORDER BY id")
		require.NoError(f.t, err)
		defer rows.Close()
		for rows.Next() {
			var s string
			require.NoError(f.t, rows.Scan(&s))
			out = append(out, s)
		}
		require.NoError(f.t, rows.Err())
	})
	return out
}

// run executes the only task Notify queued, the way the worker would.
func (f *pushFlow) run() error {
	f.t.Helper()
	require.Len(f.t, f.queue.tasks, 1, "one push:deliver task per Notify call")
	return f.deliverer.ProcessTask(f.t.Context(), f.queue.tasks[0])
}

func TestPushReachesTheDevicesOfEveryRecipient(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	_, b := f.person(f.orgA)
	_, noPhone := f.person(f.orgA)
	_, bystander := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a1")
	f.device(f.orgA, a, "tok-a2")
	f.device(f.orgA, b, "tok-b")
	f.device(f.orgA, bystander, "tok-bystander")

	f.notify(f.orgA, actor, notifications.Input{
		Type:         notifications.TypeInstructionReceived,
		RecipientIDs: []int64{a, b, noPhone},
		Vars:         notifications.Vars{Subject: "Siaga"},
		Entity:       &notifications.Entity{Type: "instruction", ID: 42},
	})

	require.Equal(t, []string{notifications.QueueDefault}, f.queue.queue)
	require.NoError(t, f.run())
	require.ElementsMatch(t, []string{"tok-a1", "tok-a2", "tok-b"}, f.provider.tokens(),
		"a recipient's every device gets it; a person who was not a recipient gets nothing")
	require.Len(t, f.provider.calls, 1)

	for _, m := range f.provider.sent() {
		require.NotEmpty(t, m.Title)
		require.NotEmpty(t, m.Body)
		require.False(t, m.High)
		require.Equal(t, push.PlatformAndroid, m.Platform)
		require.Equal(t, "INSTRUCTION_RECEIVED", m.Data["type"])
		require.Equal(t, "instruction", m.Data["entity_type"])
		require.Equal(t, "42", m.Data["entity_id"])
		require.NotEmpty(t, m.Data["notification_id"])
		require.Equal(t, "n"+m.Data["notification_id"], m.CollapseKey)
		require.Len(t, m.Data, 4, "ids and codes only")
	}
	require.Len(t, f.deviceTokens(f.orgA), 4, "no device was removed")
}

func TestPushWithoutEntityCarriesNoEntityKeys(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a")

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	require.NoError(t, f.run())

	m := f.provider.sent()[0]
	require.NotContains(t, m.Data, "entity_type")
	require.NotContains(t, m.Data, "entity_id")
}

func TestPanicPushGoesOnTheCriticalQueueAndIsHigh(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a")

	f.notify(f.orgA, actor, notifications.Input{
		Type:         notifications.TypePanicTriggered,
		RecipientIDs: []int64{a},
		Vars:         notifications.Vars{ActorName: "Budi"},
		Priority:     notifications.PriorityHigh,
	})

	require.Equal(t, []string{notifications.QueueCritical}, f.queue.queue)
	require.NoError(t, f.run())
	require.True(t, f.provider.sent()[0].High)
}

func TestPushDeletesDevicesWithInvalidTokens(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-live")
	f.device(f.orgA, a, "tok-dead")
	f.provider.invalid = map[string]bool{"tok-dead": true}

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})

	require.NoError(t, f.run(), "a dead token is cleaned up, not retried")
	require.Equal(t, []string{"tok-live"}, f.deviceTokens(f.orgA))
}

func TestPushRetriesWhenTheProviderFails(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-ok")
	f.device(f.orgA, a, "tok-flaky")
	f.device(f.orgA, a, "tok-dead")
	f.provider.failing = map[string]bool{"tok-flaky": true}
	f.provider.invalid = map[string]bool{"tok-dead": true}

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})

	err := f.run()
	require.Error(t, err, "asynq retries a task that returns an error")
	require.NotErrorIs(t, err, asynq.SkipRetry)
	require.ElementsMatch(t, []string{"tok-ok", "tok-flaky"}, f.deviceTokens(f.orgA),
		"the invalid token goes even when the task is retried; the flaky one stays")
}

func TestPushRetriesWhenTheProviderRefusesTheCall(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a")
	f.provider.err = errors.New("credentials refused")

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})

	require.ErrorContains(t, f.run(), "credentials refused")
	require.Equal(t, []string{"tok-a"}, f.deviceTokens(f.orgA))
}

func TestPushSendsInChunksOf500(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.exec(f.orgA, `
		WITH s AS (
			INSERT INTO sessions (personnel_id, family_id, refresh_token_hash, client, expires_at)
			SELECT $1, gen_random_uuid(), md5(g::text) || md5(g::text), 'MOBILE', now() + interval '1 day'
			FROM generate_series(1, 1100) g
			RETURNING id)
		INSERT INTO devices (personnel_id, session_id, platform, push_token)
		SELECT $1, id, 'ANDROID', 'tok-' || id FROM s`, a)

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	require.NoError(t, f.run())

	var sizes []int
	for _, c := range f.provider.calls {
		sizes = append(sizes, len(c))
	}
	require.Equal(t, []int{500, 500, 100}, sizes)
}

func TestPushNeverCrossesOrganizations(t *testing.T) {
	f := newPushFlow(t)
	_, actorA := f.person(f.orgA)
	_, a := f.person(f.orgA)
	_, b := f.person(f.orgB)
	f.device(f.orgA, a, "tok-a")
	f.device(f.orgB, b, "tok-b")

	f.notify(f.orgA, actorA, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	require.Len(t, f.queue.tasks, 1)

	// The same notification ids, under the other organization's tenant: row-level security hides
	// the rows, so nothing is sent.
	other := asynq.NewTask(notifications.TaskPushDeliver, retarget(t, f.queue.tasks[0].Payload(), f.orgB))
	require.NoError(t, f.deliverer.ProcessTask(t.Context(), other))
	require.Empty(t, f.provider.sent())
}

func TestPushSkipsDeletedNotifications(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a")

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	f.exec(f.orgA, "UPDATE notifications SET deleted_at = now()")

	require.NoError(t, f.run())
	require.Empty(t, f.provider.sent())
}

func TestPushBadPayloadIsNotRetried(t *testing.T) {
	f := newPushFlow(t)
	for _, payload := range []string{"not json", `{"organization_id":0,"notification_ids":[1]}`} {
		err := f.deliverer.ProcessTask(t.Context(), asynq.NewTask(notifications.TaskPushDeliver, []byte(payload)))
		require.ErrorIs(t, err, asynq.SkipRetry, payload)
	}
}

func TestPushQueueFailureDoesNotFailNotify(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.service.UseDispatcher(notifications.NewPushDispatcher(failingQueue{}))

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})

	require.Equal(t, 1, f.count(f.orgA), "the inbox row is saved whatever the queue did")
	require.Contains(t, f.logs.String(), "enqueue push task")
}

type failingQueue struct{}

func (failingQueue) EnqueueContext(context.Context, *asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error) {
	return nil, errors.New("redis is down")
}

// retarget returns the payload of a push task with another organization in it.
func retarget(t *testing.T, payload []byte, org int64) []byte {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal(payload, &p))
	p["organization_id"] = org
	out, err := json.Marshal(p)
	require.NoError(t, err)
	return out
}

// ⚠ A retry must not send again to the phones that already have the push: for a panic that would
// sound the siren twice (a collapse key replaces the banner, not the sound).
func TestPushRetryGoesOnlyToTheDevicesThatAreMissing(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-ok1")
	f.device(f.orgA, a, "tok-flaky")
	f.device(f.orgA, a, "tok-ok2")
	f.provider.failing = map[string]bool{"tok-flaky": true}

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	require.Error(t, f.run(), "the first attempt fails for one phone")
	require.ElementsMatch(t, []string{"tok-ok1", "tok-flaky", "tok-ok2"}, f.provider.tokens())

	f.provider.mu.Lock()
	f.provider.failing, f.provider.calls = nil, nil // FCM recovers
	f.provider.mu.Unlock()
	require.NoError(t, f.run(), "asynq runs the same task again")
	require.Equal(t, []string{"tok-flaky"}, f.provider.tokens(), "only the phone that is still missing is sent to")

	f.provider.mu.Lock()
	f.provider.calls = nil
	f.provider.mu.Unlock()
	require.NoError(t, f.run())
	require.Empty(t, f.provider.tokens(), "everyone has it now: a third run sends nothing")
}

func TestPushDoesNotRetryAFailureThatCannotPass(t *testing.T) {
	f := newPushFlow(t)
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-ok")
	f.device(f.orgA, a, "tok-refused")
	f.provider.refused = map[string]bool{"tok-refused": true}

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypePanicTriggered, RecipientIDs: []int64{a}, Vars: notifications.Vars{ActorName: "Budi"},
		Priority: notifications.PriorityHigh,
	})

	require.NoError(t, f.run(), "a permanent failure does not fail the task, so asynq does not retry it")
	require.Contains(t, f.logs.String(), "rejected for good")
	require.ElementsMatch(t, []string{"tok-ok", "tok-refused"}, f.deviceTokens(f.orgA), "and no device is deleted for it")
	require.Len(t, f.provider.sent(), 2, "each phone was sent to exactly once")
}

func TestPushWorksWithoutASentLog(t *testing.T) {
	f := newPushFlow(t)
	f.deliverer = notifications.NewPushDeliverer(testkit.Pool(t), f.provider, slog.New(slog.NewJSONHandler(f.logs, nil)))
	_, actor := f.person(f.orgA)
	_, a := f.person(f.orgA)
	f.device(f.orgA, a, "tok-a")

	f.notify(f.orgA, actor, notifications.Input{
		Type: notifications.TypeTeamChanged, RecipientIDs: []int64{a}, Vars: notifications.Vars{TeamName: "R1"},
	})
	require.NoError(t, f.run())
	require.NoError(t, f.run())
	require.Equal(t, []string{"tok-a", "tok-a"}, f.provider.tokens(), "without the log a rerun sends again")
}
