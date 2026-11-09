package organization

import (
	"context"
	"fmt"

	"siapp/pkg/leave"
	"siapp/pkg/platform/timeutil"
)

// LeaveSchedule answers leave's question "which shifts of this range does the person have, and which
// of them are the day off" from Shifts (Phase 07.5).
type LeaveSchedule struct {
	Shifts Shifts
}

var _ leave.Schedule = LeaveSchedule{}

// Instances returns the shifts of [from, to] that have a schedule row, the day off marked, with the
// instants and the regu of each working one.
func (l LeaveSchedule) Instances(ctx context.Context, organizationID, personnelID int64, from, to timeutil.Date) ([]leave.ScheduleInstance, error) {
	instances, err := l.Shifts.Instances(ctx, organizationID, personnelID, from, to)
	if err != nil {
		return nil, fmt.Errorf("instances: %w", err)
	}
	out := make([]leave.ScheduleInstance, 0, len(instances))
	for _, inst := range instances {
		zone := ""
		if inst.Location != nil {
			zone = inst.Location.String()
		}
		out = append(out, leave.ScheduleInstance{
			ScheduleDay: leave.ScheduleDay{Date: inst.WorkDate, Off: inst.IsOff},
			ShiftID:     inst.ShiftID, StartsAt: inst.StartsAt, EndsAt: inst.EndsAt, Timezone: zone, TeamCodes: inst.TeamCodes,
		})
	}
	return out, nil
}
