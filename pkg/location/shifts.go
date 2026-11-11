package location

import (
	"context"
	"time"

	"siapp/pkg/attendance"
)

// Window is a stretch of time the person works in a regu, with whether its project tracks GPS.
type Window struct {
	TeamCode        string
	ProjectCode     string
	TrackingEnabled bool
	StartsAt        time.Time
	EndsAt          time.Time
}

// contains reports whether t falls inside the window, both ends included.
func (w Window) contains(t time.Time) bool { return !t.Before(w.StartsAt) && !t.After(w.EndsAt) }

// Shifts says when a person works.
type Shifts interface {
	// Windows returns the person's shifts that overlap [from, to].
	Windows(ctx context.Context, orgID, personnelID int64, from, to time.Time) ([]Window, error)
}

// AttendanceShifts answers from the attendance module's shifts: the person's own regu's and the
// ones covered through a swap.
type AttendanceShifts struct{ Service *attendance.Service }

// Windows implements Shifts.
func (a AttendanceShifts) Windows(ctx context.Context, orgID, personnelID int64, from, to time.Time) ([]Window, error) {
	shifts, err := a.Service.ShiftWindows(ctx, orgID, personnelID, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]Window, 0, len(shifts))
	for _, s := range shifts {
		out = append(out, Window{TeamCode: s.TeamCode, ProjectCode: s.ProjectCode, TrackingEnabled: s.Tracking, StartsAt: s.StartsAt, EndsAt: s.EndsAt})
	}
	return out, nil
}
