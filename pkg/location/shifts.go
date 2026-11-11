package location

import (
	"context"
	"time"

	"siapp/pkg/attendance"
)

// Site is a regu the person is on shift in right now, with whether its project tracks GPS.
type Site struct {
	TeamCode        string
	ProjectCode     string
	TrackingEnabled bool
}

// Shifts says where a person is on shift.
type Shifts interface {
	OnShift(ctx context.Context, orgID, personnelID int64, now time.Time) ([]Site, error)
}

// AttendanceShifts answers from the attendance module's shifts, so "on shift" means the same
// here as for a check-in: the shift window plus its tolerance before and after, and a swapped-in
// shift counts.
type AttendanceShifts struct{ Service *attendance.Service }

// OnShift implements Shifts.
func (a AttendanceShifts) OnShift(ctx context.Context, orgID, personnelID int64, now time.Time) ([]Site, error) {
	shifts, err := a.Service.Scheduled(ctx, orgID, personnelID, now)
	if err != nil {
		return nil, err
	}
	out := make([]Site, 0, len(shifts))
	for _, s := range shifts {
		out = append(out, Site{TeamCode: s.Team.Code, ProjectCode: s.Project.Code, TrackingEnabled: s.Project.GPSTrackingEnabled})
	}
	return out, nil
}
