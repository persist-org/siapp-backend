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

	slices.SortStableFunc(out, func(a, b CheckableShift) int {
		if c := a.StartsAt.Compare(b.StartsAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key.TeamCode, b.Key.TeamCode)
	})
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
