package notifications_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
	"siapp/pkg/platform/testkit"
)

func TestNotifyWritesOneRowPerRecipient(t *testing.T) {
	f := newFlow(t)
	tokenA, a := f.person(f.orgA)
	tokenB, b := f.person(f.orgA)
	tokenC, c := f.person(f.orgA)

	// c caused the event and is also in the list; duplicates and ids that cannot be a person
	// are in it too. What is left is a and b, once each.
	f.notify(f.orgA, c, notifications.Input{
		Type:               notifications.TypeTeamChanged,
		RecipientIDs:       []int64{a, b, b, c, 0, -7, a},
		Vars:               notifications.Vars{TeamName: "1K-IBMI-S1"},
		ExcludePersonnelID: c,
	})

	require.Equal(t, 2, f.count(f.orgA))
	for token, person := range map[string]int64{tokenA: a, tokenB: b} {
		inbox := f.list(token, "")
		require.Len(t, inbox.Data, 1)
		n := inbox.Data[0]
		require.Equal(t, f.orgA, n.OrganizationID)
		require.Equal(t, person, n.PersonnelID)
		require.Equal(t, "TEAM_CHANGED", n.Type)
		require.Equal(t, "Perpindahan regu", n.Title)
		require.Equal(t, "Anda dipindahkan ke regu 1K-IBMI-S1.", n.Body)
		require.Equal(t, "NORMAL", n.Priority, "the priority defaults to NORMAL")
		require.Nil(t, n.EntityType)
		require.Nil(t, n.EntityID)
		require.Empty(t, n.Data)
		require.False(t, n.IsRead)
		require.Nil(t, n.ReadAt)
		require.True(t, f.clock.Now().Equal(n.CreatedAt), "created at the clock's time, got %s", n.CreatedAt)
		require.True(t, n.CreatedAt.Equal(n.UpdatedAt))
	}
	require.Empty(t, f.list(tokenC, "").Data, "the person who caused the event is not told about it")

	calls := f.dispatcher.got()
	require.Len(t, calls, 1, "one Dispatch per Notify")
	require.Equal(t, f.orgA, calls[0].OrganizationID)
	var recipients []int64
	for _, n := range calls[0].Sent {
		recipients = append(recipients, n.RecipientID)
	}
	require.ElementsMatch(t, []int64{a, b}, recipients)
}

func TestNotifyStoresEntityDataPriorityAndTheBusinessLinesWords(t *testing.T) {
	f := newFlow(t)
	token, member := f.person(f.orgA)
	_, actor := f.person(f.orgA)

	f.notify(f.orgA, actor, notifications.Input{
		Type:         notifications.TypePanicTriggered,
		RecipientIDs: []int64{member},
		Vars:         notifications.Vars{BusinessLine: notifications.LineCleaning, ActorName: "Sari", TeamName: "1K-CLN-C1"},
		Entity:       &notifications.Entity{Type: "panic", ID: 12},
		Data:         map[string]any{"team_code": "1K-CLN-C1", "panic_id": 12},
		Priority:     notifications.PriorityHigh,
	})

	n := f.list(token, "").Data[0]
	require.Equal(t, "HIGH", n.Priority)
	require.Equal(t, "panic", *n.EntityType)
	require.EqualValues(t, 12, *n.EntityID)
	require.Equal(t, map[string]any{"team_code": "1K-CLN-C1", "panic_id": 12.0}, n.Data)
	// The cleaning line says "Sinyal Panik" and "tim", not "Sinyal Tombol Panik" and "regu".
	require.Equal(t, "Sinyal Panik", n.Title)
	require.Equal(t, "Sari mengirim sinyal panik di tim 1K-CLN-C1. Segera beri bantuan.", n.Body)
}

// ⚠ The rule every module relies on: a notification failure never reaches the caller. Notify has
// no error to return, so each case is checked by what it leaves behind: the inbox, and the log.
func TestNotifyNeverFailsTheCaller(t *testing.T) {
	input := func(recipients ...int64) notifications.Input {
		return notifications.Input{Type: notifications.TypeTeamChanged, RecipientIDs: recipients, Vars: notifications.Vars{TeamName: "R1"}}
	}

	t.Run("a caller with no organization behind it is logged, and nothing is written", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		require.NotPanics(t, func() { f.service.Notify(t.Context(), input(a)) })
		require.Zero(t, f.count(f.orgA))
		require.Contains(t, f.logs.String(), "could not notify")
		require.Empty(t, f.dispatcher.got())
	})

	t.Run("a failing dispatcher is logged and the rows stay", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)
		f.dispatcher.err = errors.New("redis is down")

		f.notify(f.orgA, actor, input(a))

		require.Equal(t, 1, f.count(f.orgA), "the inbox row is saved before delivery is tried")
		require.Contains(t, f.logs.String(), "redis is down")
	})

	t.Run("a panicking dispatcher is recovered, because the router would answer 500 for an action that worked", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)
		f.dispatcher.boom = true

		require.NotPanics(t, func() { f.notify(f.orgA, actor, input(a)) })

		require.Equal(t, 1, f.count(f.orgA))
		require.Contains(t, f.logs.String(), "Notify panicked")
	})

	t.Run("a recipient who is not a person of the organization gets nothing, and the others are still notified", func(t *testing.T) {
		f := newFlow(t)
		tokenA, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)
		_, foreign := f.person(f.orgB)

		// foreign belongs to org B, 999999 to nobody: neither may be notified through org A.
		f.notify(f.orgA, actor, input(a, foreign, 999999))

		require.Equal(t, 1, f.count(f.orgA))
		require.Zero(t, f.count(f.orgB), "a notification must never land in another tenant")
		require.Len(t, f.list(tokenA, "").Data, 1)
		require.Contains(t, f.logs.String(), "some recipients are not people of the organization")
		calls := f.dispatcher.got()
		require.Len(t, calls, 1)
		require.Len(t, calls[0].Sent, 1, "only what was written is dispatched")
	})

	t.Run("a request that was cancelled does not drop the notification", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)
		// The phone that pressed the panic button lost its signal: the request context is gone
		// by the time Notify runs.
		ctx, cancel := context.WithCancel(f.as(f.orgA, actor))
		cancel()

		f.service.Notify(ctx, input(a))

		require.Equal(t, 1, f.count(f.orgA))
	})

	t.Run("a type the database does not know is logged", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)

		in := input(a)
		in.Type = "NOT_A_TYPE"
		require.NotPanics(t, func() { f.notify(f.orgA, actor, in) })

		require.Zero(t, f.count(f.orgA))
		require.Contains(t, f.logs.String(), "could not notify")
	})

	t.Run("an entity without a type or an id is refused", func(t *testing.T) {
		f := newFlow(t)
		_, a := f.person(f.orgA)
		_, actor := f.person(f.orgA)

		for _, entity := range []*notifications.Entity{{Type: "panic"}, {ID: 4}, {Type: "panic", ID: -1}} {
			in := input(a)
			in.Entity = entity
			f.notify(f.orgA, actor, in)
		}

		require.Zero(t, f.count(f.orgA))
	})

	t.Run("nobody to notify is not an error and delivers nothing", func(t *testing.T) {
		f := newFlow(t)
		_, actor := f.person(f.orgA)

		f.notify(f.orgA, actor, input())
		in := input(actor)
		in.ExcludePersonnelID = actor
		f.notify(f.orgA, actor, in)

		require.Zero(t, f.count(f.orgA))
		require.Empty(t, f.dispatcher.got())
		require.Empty(t, f.logs.String(), "an empty audience is normal, not worth a log line")
	})
}

func TestTheDefaultDispatcherDoesNothing(t *testing.T) {
	f := newFlow(t)
	_, a := f.person(f.orgA)
	_, actor := f.person(f.orgA)

	bare := notifications.NewService(testkit.Pool(t), f.clock, slog.New(slog.DiscardHandler))
	bare.Notify(f.as(f.orgA, actor), notifications.Input{Type: notifications.TypeLeaveDecided, RecipientIDs: []int64{a}, Vars: notifications.Vars{Approved: true}})
	require.Equal(t, 1, f.count(f.orgA), "the inbox works with no delivery plugged in")

	// Taking a dispatcher out puts the default back.
	bare.UseDispatcher(f.dispatcher)
	bare.UseDispatcher(nil)
	bare.Notify(f.as(f.orgA, actor), notifications.Input{Type: notifications.TypeLeaveDecided, RecipientIDs: []int64{a}})
	require.Equal(t, 2, f.count(f.orgA))
	require.Empty(t, f.dispatcher.got())
}
