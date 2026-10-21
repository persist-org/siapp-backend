package organization

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// The starting shifts, as in PJ48's m_shifts (see docs/masterdata/schednshift.md), and a
// starting group that works them Monday to Friday.
const (
	SeedShiftOff     = "OFF"
	SeedShiftDay     = "Day Shift"
	SeedShiftNight   = "Night Shift"
	SeedGroupWeekday = "5 Hari Kerja" //nolint:gosec // a shift group's name, not a credential
)

var seedShifts = []struct {
	name       string
	start, end int
	off        bool
	tolerance  int16
}{
	{name: SeedShiftOff, off: true}, // an OFF day has no tolerance, as the API enforces
	{name: SeedShiftDay, start: 8 * 60, end: 17 * 60, tolerance: defaultTolerance},
	{name: SeedShiftNight, start: 22 * 60, end: 6 * 60, tolerance: defaultTolerance},
}

// SeedResult says what SeedDefaultShifts made; names it found already are not counted.
type SeedResult struct {
	ShiftsCreated []string
	GroupCreated  bool
}

// SeedDefaultShifts gives an organization PJ48's starting shifts (OFF, Day Shift 08:00-17:00,
// Night Shift 22:00-06:00) and the group "5 Hari Kerja" (Day Shift Monday to Friday, OFF on
// the weekend). It is for the development seed and a fresh environment.
//
// It can run any number of times: a shift or group whose name exists (ignoring case) is left
// exactly as it is, so a shift an admin has since changed is never reset. Rows are created by
// the system (no created_by) and audited as such.
//
// The organization must already be registered: it is, once anyone has signed in or the first
// admin has been granted.
func SeedDefaultShifts(ctx context.Context, pool *pgxpool.Pool, c clock.Clock, organizationID int64) (SeedResult, error) {
	if err := database.WithoutTenant(ctx, pool, func(q *db.Queries) error {
		_, err := q.GetOrganization(ctx, organizationID)
		return err
	}); errors.Is(err, pgx.ErrNoRows) {
		return SeedResult{}, fmt.Errorf("organization %d is not registered yet: sign in once, or run make grant-admin first", organizationID)
	} else if err != nil {
		return SeedResult{}, fmt.Errorf("get organization: %w", err)
	}

	var out SeedResult
	now := ts(c.Now())
	err := database.WithTenant(ctx, pool, organizationID, func(q *db.Queries) error {
		ids := map[string]int64{}
		for _, s := range seedShifts {
			existing, err := q.GetShiftByName(ctx, s.name)
			if err == nil {
				ids[s.name] = existing.ID
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("get shift %q: %w", s.name, err)
			}
			row, err := q.CreateShift(ctx, db.CreateShiftParams{
				Name: s.name, StartMinute: int16(s.start), EndMinute: int16(s.end), IsOff: s.off, //nolint:gosec // constants above
				ToleranceMinutes: s.tolerance, Active: true, Actor: pgtype.Int8{}, Now: now,
			})
			if err != nil {
				return fmt.Errorf("create shift %q: %w", s.name, err)
			}
			ids[s.name] = row.ID
			out.ShiftsCreated = append(out.ShiftsCreated, s.name)
			if err := audit.Record(ctx, q, audit.Entry{
				Action: "shift.create", EntityType: "shift", EntityID: row.ID,
				Metadata: map[string]any{"after": shiftMeta(toShift(row)), "source": "seed"},
			}); err != nil {
				return fmt.Errorf("audit shift.create: %w", err)
			}
		}

		if _, err := q.GetShiftGroupByName(ctx, SeedGroupWeekday); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get shift group: %w", err)
		}
		group, err := q.CreateShiftGroup(ctx, db.CreateShiftGroupParams{Name: SeedGroupWeekday, Actor: pgtype.Int8{}, Now: now})
		if err != nil {
			return fmt.Errorf("create shift group: %w", err)
		}
		days := map[int]int64{}
		for weekday := 1; weekday <= 7; weekday++ {
			days[weekday] = ids[SeedShiftDay]
			if weekday >= 6 {
				days[weekday] = ids[SeedShiftOff]
			}
		}
		if err := saveDays(ctx, q, group.ID, days); err != nil {
			return err
		}
		created, err := loadGroup(ctx, q, group.ID, false)
		if err != nil {
			return err
		}
		out.GroupCreated = true
		if err := audit.Record(ctx, q, audit.Entry{
			Action: "shift_group.create", EntityType: "shift_group", EntityID: group.ID,
			Metadata: map[string]any{"after": groupMeta(created), "source": "seed"},
		}); err != nil {
			return fmt.Errorf("audit shift_group.create: %w", err)
		}
		return nil
	})
	return out, err
}
