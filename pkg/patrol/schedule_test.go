package patrol

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScheduleStatus(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	before, during, after := now.Add(time.Hour), now.Add(-time.Hour), now.Add(-time.Hour)
	_ = during
	cancelled := now.Add(-2 * time.Hour)
	active, ended, auto := &LinkedPatrol{Status: "ACTIVE"}, &LinkedPatrol{Status: "ENDED"}, &LinkedPatrol{Status: "AUTO_ENDED"}

	tests := []struct {
		name   string
		sc     scheduleTimes
		patrol *LinkedPatrol
		want   string
	}{
		{"window ahead", scheduleTimes{EndsAt: before}, nil, ScheduleScheduled},
		{"inside the window, nobody started", scheduleTimes{EndsAt: now.Add(time.Minute)}, nil, ScheduleScheduled},
		{"window over, no patrol", scheduleTimes{EndsAt: after}, nil, ScheduleMissed},
		{"cancelled wins over a window ahead", scheduleTimes{EndsAt: before, CancelledAt: &cancelled}, nil, ScheduleCancelled},
		{"cancelled wins over a missed window", scheduleTimes{EndsAt: after, CancelledAt: &cancelled}, nil, ScheduleCancelled},
		{"cancelled wins over a patrol", scheduleTimes{EndsAt: after, CancelledAt: &cancelled}, ended, ScheduleCancelled},
		{"patrol running", scheduleTimes{EndsAt: before}, active, ScheduleInProgress},
		{"patrol running past the window", scheduleTimes{EndsAt: after}, active, ScheduleInProgress},
		{"patrol ended", scheduleTimes{EndsAt: after}, ended, ScheduleCompleted},
		{"patrol ended by the job", scheduleTimes{EndsAt: after}, auto, ScheduleCompleted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, scheduleStatus(now, tc.sc, tc.patrol))
		})
	}
}

func TestCheckWindow(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	require.Empty(t, checkWindow("slots[0]", now.Add(time.Hour), now.Add(2*time.Hour), now))
	require.Empty(t, checkWindow("slots[0]", now.Add(time.Hour), now.Add(25*time.Hour), now), "exactly 24 hours")
	require.NotEmpty(t, checkWindow("slots[0]", now.Add(time.Hour), now.Add(time.Hour), now))
	require.NotEmpty(t, checkWindow("slots[0]", now.Add(time.Hour), now.Add(25*time.Hour+time.Second), now))
	got := checkWindow("slots[3]", now.Add(-2*time.Hour), now.Add(-time.Hour), now)
	require.Len(t, got, 1)
	require.Equal(t, "slots[3].ends_at", got[0].Field)
}
