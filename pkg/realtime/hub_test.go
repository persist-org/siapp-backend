package realtime_test

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/realtime"
)

func quietHub() *realtime.Hub { return realtime.NewHub(slog.New(slog.NewJSONHandler(io.Discard, nil))) }

func TestHubDeliversOnlyInsideTheOrganization(t *testing.T) {
	hub := quietHub()
	a := realtime.NewConn(1, 10, func() {})
	b := realtime.NewConn(2, 20, func() {})
	hub.Join(a, "team.7.panic")
	hub.Join(b, "team.7.panic")

	hub.Deliver(1, "team.7.panic", []byte("for-org-1"))

	require.Equal(t, []byte("for-org-1"), <-a.Send())
	require.Empty(t, b.Send(), "the same room name of another organization is another room")
}

func TestHubLeaveAndRemove(t *testing.T) {
	hub := quietHub()
	c := realtime.NewConn(1, 10, func() {})
	hub.Join(c, "team.7.panic")
	hub.Join(c, "team.7.tracking")
	require.ElementsMatch(t, []string{"team.7.panic", "team.7.tracking"}, hub.RoomsOf(c))

	hub.Leave(c, "team.7.panic")
	hub.Deliver(1, "team.7.panic", []byte("x"))
	require.Empty(t, c.Send())

	hub.Remove(c)
	require.Empty(t, hub.RoomsOf(c))
	hub.Deliver(1, "team.7.tracking", []byte("x"))
	require.Empty(t, c.Send())
}

// ⚠ One stuck phone must not stall a panic: a connection whose buffer is full is dropped, and the
// others still get the frame.
func TestHubDropsASlowConnectionAndKeepsDelivering(t *testing.T) {
	hub := quietHub()
	var kicked atomic.Int32
	slow := realtime.NewConn(1, 10, func() { kicked.Add(1) })
	fast := realtime.NewConn(1, 11, func() { t.Error("the fast connection must not be dropped") })
	hub.Join(slow, "team.7.panic")
	hub.Join(fast, "team.7.panic")

	const frames = 100
	for range frames {
		hub.Deliver(1, "team.7.panic", []byte("siren"))
		// The fast connection's writer keeps up; the slow one never reads.
		select {
		case <-fast.Send():
		default:
			t.Fatal("the fast connection lost a frame")
		}
	}

	require.Positive(t, kicked.Load(), "the slow connection was dropped")
	require.Len(t, slow.Send(), 32, "its buffer holds what fitted")
}
