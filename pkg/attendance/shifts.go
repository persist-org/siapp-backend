package attendance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"siapp/internal/db"
	"siapp/pkg/organization"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/timeutil"
)

// CheckableShift is a shift the person can check into or out of right now, in one regu.
type CheckableShift struct {
	Key  shiftKey
	Ref  string
	Team organization.Team
	// Project is the regu's proyek: where the geofence is, and the timezone the shift is in.
	Project   organization.Project
	ShiftName string
	// ToleranceMinutes is how long after StartsAt a check-in still counts as on time.
	ToleranceMinutes int
	StartsAt         time.Time
	EndsAt           time.Time
	Location         *time.Location
	// Record is what is already captured for the shift, nil when nothing is.
	Record *Attendance
	// SwapID is set when this instance is a shift the person covers for someone else (07.7), nil
	// for a shift of their own regu.
	SwapID *int64
}

// Checkable returns the shifts the person can check into at now, one per regu they serve in that
// day, soonest first.
//
// It combines organization.Shifts.Current (the scheduled shifts whose window contains now: from
// two hours before the start to four hours after the end, so an overnight shift can still be
// checked out after midnight) with the records that already exist, so the app can show "belum
// absen", "sudah absen datang" or the leave and absence a DANRU recorded.
//
// A shift is per regu, not per person: someone who serves in two regu on one day is checked into
// each separately, and each is a separate record (and a separate event for Personel).
func (s *Service) Checkable(ctx context.Context, organizationID, personnelID int64, now time.Time) ([]CheckableShift, error) {
	out, err := s.scheduled(ctx, organizationID, personnelID, now)
	if err != nil || len(out) == 0 {
		return out, err
	}

	first, last := out[0].Key.WorkDate, out[0].Key.WorkDate
	for _, sh := range out {
		if sh.Key.WorkDate.Before(first) {
			first = sh.Key.WorkDate
		}
		if sh.Key.WorkDate.After(last) {
			last = sh.Key.WorkDate
		}
	}
	records := map[shiftKey]Attendance{}
	err = database.WithTenant(ctx, s.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListAttendancesOfPersonnelBetween(ctx, db.ListAttendancesOfPersonnelBetweenParams{
			PersonnelID: personnelID, DateFrom: dateParam(first), DateTo: dateParam(last),
		})
		if err != nil {
			return fmt.Errorf("list attendances: %w", err)
		}
		for _, r := range rows {
			records[shiftKey{TeamCode: r.TeamCode, WorkDate: dateValue(r.WorkDate), ShiftID: r.ShiftID}] = toAttendance(r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if rec, ok := records[out[i].Key]; ok {
			out[i].Record = &rec
		}
	}
	return out, nil
}

// scheduled is Checkable without the records: the shifts the person can check into at now,
// soonest first. find needs only this, so a check-in does not read the records it ignores.
func (s *Service) scheduled(ctx context.Context, organizationID, personnelID int64, now time.Time) ([]CheckableShift, error) {
	instances, err := s.shifts.Current(ctx, organizationID, personnelID, now)
	if err != nil {
		return nil, fmt.Errorf("current shifts: %w", err)
	}

	// The names and the geofence live on the regu and its proyek.
	sites := map[string]organization.TeamWithProject{}
	out := []CheckableShift{}
	for _, inst := range instances {
		for _, teamCode := range inst.TeamCodes {
			site, ok := sites[teamCode]
			if !ok {
				if site, err = s.sites.TeamWithProject(ctx, organizationID, teamCode); errors.Is(err, organization.ErrTeamNotFound) {
					continue // a regu deleted since: its memberships have ended, and so has the shift
				} else if err != nil {
					return nil, fmt.Errorf("team %s: %w", teamCode, err)
				}
				sites[teamCode] = site
			}
			key := shiftKey{TeamCode: teamCode, WorkDate: inst.WorkDate, ShiftID: inst.ShiftID}
			out = append(out, CheckableShift{
				Key: key, Ref: key.ref(), Team: site.Team, Project: site.Project,
				ShiftName: inst.ShiftName, ToleranceMinutes: inst.ToleranceMinutes,
				StartsAt: inst.StartsAt, EndsAt: inst.EndsAt, Location: inst.Location,
			})
		}
	}

	// 07.7: active swaps where the person is the replacer are extra instances, in the swap's own
	// team (not necessarily a regu the person serves in today).
	swaps, err := s.activeSwapsNear(ctx, organizationID, personnelID, now)
	if err != nil {
		return nil, err
	}
	for _, sw := range swaps {
		teamCode := sw.Swap.TeamCode
		site, ok := sites[teamCode]
		if !ok {
			if site, err = s.sites.TeamWithProject(ctx, organizationID, teamCode); errors.Is(err, organization.ErrTeamNotFound) {
				continue // the swap's regu deleted since
			} else if err != nil {
				return nil, fmt.Errorf("team %s: %w", teamCode, err)
			}
			sites[teamCode] = site
		}
		loc, err := time.LoadLocation(site.Project.Timezone)
		if err != nil {
			return nil, fmt.Errorf("load location %s: %w", site.Project.Timezone, err)
		}
		id := sw.Swap.ID
		key := shiftKey{TeamCode: teamCode, WorkDate: dateValue(sw.Swap.WorkDate), ShiftID: sw.Swap.ShiftID}
		out = append(out, CheckableShift{
			Key: key, Ref: key.ref(), Team: site.Team, Project: site.Project,
			ShiftName: sw.ShiftName, ToleranceMinutes: sw.ToleranceMinutes,
			StartsAt: sw.Swap.StartsAt.Time, EndsAt: sw.Swap.EndsAt.Time, Location: loc,
			SwapID: &id,
		})
	}

	slices.SortStableFunc(out, func(a, b CheckableShift) int {
		if c := a.StartsAt.Compare(b.StartsAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key.TeamCode, b.Key.TeamCode)
	})
	return out, nil
}

// ShiftWindow is a stretch of time the person works in a regu: the shift itself, from StartsAt to
// EndsAt, without the check-in tolerance. Tracking is the project's gps_tracking_enabled.
type ShiftWindow struct {
	TeamCode    string
	ProjectCode string
	Tracking    bool
	StartsAt    time.Time
	EndsAt      time.Time
}

// ShiftWindows returns the shifts (their own regu's and the ones covered through a swap) that
// overlap [from, to], OFF days left out. It asks "when was this person working" for a time that
// may be in the past: location tracking judges each recorded point against it.
//
// A day whose schedule is ambiguous (the person serves in projects of two timezones) yields no
// window: a shift whose instants cannot be told is not one to track in, so it fails closed.
func (s *Service) ShiftWindows(ctx context.Context, organizationID, personnelID int64, from, to time.Time) ([]ShiftWindow, error) {
	first := timeutil.BusinessDate(from, time.UTC).AddDays(-2)
	last := timeutil.BusinessDate(to, time.UTC).AddDays(1)
	// A day with two timezones has no window, the other days keep theirs.
	instances, err := s.shifts.Available(ctx, organizationID, personnelID, first, last)
	if err != nil {
		return nil, fmt.Errorf("shift instances: %w", err)
	}

	sites := map[string]organization.TeamWithProject{}
	site := func(teamCode string) (organization.TeamWithProject, bool, error) {
		if v, ok := sites[teamCode]; ok {
			return v, true, nil
		}
		v, err := s.sites.TeamWithProject(ctx, organizationID, teamCode)
		if errors.Is(err, organization.ErrTeamNotFound) {
			return v, false, nil // a regu deleted since: its memberships have ended
		}
		if err != nil {
			return v, false, fmt.Errorf("team %s: %w", teamCode, err)
		}
		sites[teamCode] = v
		return v, true, nil
	}

	var out []ShiftWindow
	add := func(teamCode string, start, end time.Time) error {
		if end.Before(from) || start.After(to) {
			return nil
		}
		v, ok, err := site(teamCode)
		if err != nil || !ok {
			return err
		}
		out = append(out, ShiftWindow{TeamCode: teamCode, ProjectCode: v.Project.Code, Tracking: v.Project.GPSTrackingEnabled, StartsAt: start, EndsAt: end})
		return nil
	}
	for _, inst := range instances {
		if inst.IsOff {
			continue
		}
		for _, teamCode := range inst.TeamCodes {
			if err := add(teamCode, inst.StartsAt, inst.EndsAt); err != nil {
				return nil, err
			}
		}
	}
	err = database.WithTenant(ctx, s.pool, organizationID, func(q *db.Queries) error {
		rows, err := q.ListActiveSwapsOfReplacer(ctx, db.ListActiveSwapsOfReplacerParams{
			PersonnelID: personnelID, DateFrom: dateParam(first), DateTo: dateParam(last),
		})
		if err != nil {
			return fmt.Errorf("active swaps: %w", err)
		}
		for _, sw := range rows {
			if err := add(sw.TeamCode, sw.StartsAt.Time, sw.EndsAt.Time); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// find returns the checkable shift a shift_ref names (without its Record), or
// ErrShiftNotActive. Matching the ref against the checkable shifts is what makes it safe: see
// shiftKey.ref.
func (s *Service) find(ctx context.Context, organizationID, personnelID int64, ref string, now time.Time) (CheckableShift, error) {
	key, ok := parseShiftRef(ref)
	if !ok {
		return CheckableShift{}, ErrShiftNotActive
	}
	shifts, err := s.scheduled(ctx, organizationID, personnelID, now)
	if err != nil {
		return CheckableShift{}, err
	}
	for _, sh := range shifts {
		if sh.Key == key {
			return sh, nil
		}
	}
	return CheckableShift{}, ErrShiftNotActive
}
