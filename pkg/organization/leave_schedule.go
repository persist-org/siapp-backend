package organization

import (
	"context"
	"fmt"

	"siapp/pkg/leave"
	"siapp/pkg/platform/timeutil"
)

// LeaveSchedule answers leave's question "which days of this range does the person have a shift
// on, and which of them are the day off" from Shifts (Phase 07.5).
type LeaveSchedule struct {
	Shifts Shifts
}

var _ leave.Schedule = LeaveSchedule{}

// Days returns the dates of [from, to] that have a schedule row, the day off marked.
func (l LeaveSchedule) Days(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]leave.ScheduleDay, error) {
	instances, err := l.Shifts.Instances(ctx, organizationID, personnelID, from, to)
	if err != nil {
		return nil, fmt.Errorf("instances: %w", err)
	}
	out := make([]leave.ScheduleDay, 0, len(instances))
	for _, inst := range instances {
		out = append(out, leave.ScheduleDay{Date: inst.WorkDate, Off: inst.IsOff})
	}
	return out, nil
}
