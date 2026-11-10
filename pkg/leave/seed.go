package leave

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// defaultCategory is one row of the starting list. The ids of kpi-personalia-api's
// m_attendance_leave_permit_categories seeder are kept in the comments: it is a recovered
// snapshot, so confirm them against production before launch.
type defaultCategory struct {
	code, name        string
	typ               Type
	annual, skipsDays bool
	timeRange         bool
	changeDay         bool
	// noLeaveDays is true for the categories whose approval does not write LEAVE rows.
	noLeaveDays     bool
	excusesLateness bool
	noQuota         bool
}

var defaultCategories = []defaultCategory{
	{code: "ANNUAL", name: "Cuti Tahunan", typ: TypeLeave, annual: true},                                            // KPI 1
	{code: "UNPAID", name: "Cuti Potong Gaji", typ: TypeLeave},                                                      // KPI 2
	{code: "MATERNITY", name: "Cuti Melahirkan (Wanita)", typ: TypePermit, skipsDays: true},                         // KPI 3
	{code: "FAMILY_DEATH_CORE", name: "Cuti Keluarga Inti Meninggal", typ: TypePermit},                              // KPI 4
	{code: "FAMILY_DEATH_HOME", name: "Cuti Keluarga Serumah Meninggal", typ: TypePermit},                           // KPI 5
	{code: "SPECIAL", name: "Cuti Khusus", typ: TypePermit},                                                         // KPI 6
	{code: "CHILD_BAPTISM", name: "Cuti Pembaptisan Anak", typ: TypePermit},                                         // KPI 7
	{code: "CHILD_CIRCUMCISION", name: "Cuti Penyunatan Anak", typ: TypePermit},                                     // KPI 8
	{code: "OTHER_PART_DAY", name: "Other Part of Day", typ: TypePermit, timeRange: true, noLeaveDays: true},        // KPI 10
	{code: "ON_DUTY_PART_DAY", name: "On Duty Part of Day", typ: TypePermit, timeRange: true, noLeaveDays: true},    // KPI 11
	{code: "ON_DUTY_FULL_DAY", name: "On Duty Full Day", typ: TypePermit, noLeaveDays: true, excusesLateness: true}, // KPI 12
	{code: "CHANGE_DAY", name: "On Change Day (Tukar Hari)", typ: TypePermit, changeDay: true},                      // KPI 13
	{code: "MARRIAGE", name: "Cuti Menikah", typ: TypePermit},                                                       // KPI 15
	{code: "CHILD_MARRIAGE", name: "Cuti Pernikahan Anak", typ: TypePermit},                                         // KPI 16
	{code: "CHILD_BIRTH", name: "Cuti Kelahiran / Keguguran Anak (Pria)", typ: TypePermit},                          // KPI 17
	{code: "SICK", name: "Sakit", typ: TypeSick, noQuota: true},                                                     // KPI 21
	{code: "MISCARRIAGE", name: "Cuti Keguguran (Wanita)", typ: TypePermit},                                         // KPI 23
}

// EnsureDefaults gives an organization its starting categories, in a transaction of its own, and
// returns how many it added. It runs when an organization is first seen (login, like
// masterdata's) and is safe to call again: a second call adds nothing.
//
// Insert-if-missing by code, never an overwrite: an admin who renamed, edited or deactivated a
// default is not given the original back. Quotas are not seeded: with no row a category has 0
// days, so HR sets them.
func EnsureDefaults(ctx context.Context, pool *pgxpool.Pool, c clock.Clock, organizationID int64) (int64, error) {
	var added int64
	err := database.WithTenant(ctx, pool, organizationID, func(q *db.Queries) error {
		var err error
		added, err = EnsureDefaultsTx(ctx, q, c.Now())
		return err
	})
	return added, err
}

// EnsureDefaultsTx is EnsureDefaults inside the tenant transaction of q.
func EnsureDefaultsTx(ctx context.Context, q *db.Queries, now time.Time) (int64, error) {
	n := len(defaultCategories)
	p := db.InsertDefaultLeaveCategoriesParams{
		Now: ts(now), Codes: make([]string, 0, n), Names: make([]string, 0, n), Types: make([]string, 0, n),
		IsAnnual: make([]bool, 0, n), SkipsDayCheck: make([]bool, 0, n), RequiresTimeRange: make([]bool, 0, n),
		RequiresChangeDay: make([]bool, 0, n), MakesLeaveDays: make([]bool, 0, n), ExcusesLateness: make([]bool, 0, n),
		HasQuota: make([]bool, 0, n), Orders: make([]int32, 0, n),
	}
	for i, c := range defaultCategories {
		p.Codes, p.Names, p.Types = append(p.Codes, c.code), append(p.Names, c.name), append(p.Types, string(c.typ))
		p.IsAnnual, p.SkipsDayCheck = append(p.IsAnnual, c.annual), append(p.SkipsDayCheck, c.skipsDays)
		p.RequiresTimeRange, p.RequiresChangeDay = append(p.RequiresTimeRange, c.timeRange), append(p.RequiresChangeDay, c.changeDay)
		p.MakesLeaveDays, p.ExcusesLateness = append(p.MakesLeaveDays, !c.noLeaveDays), append(p.ExcusesLateness, c.excusesLateness)
		p.HasQuota, p.Orders = append(p.HasQuota, !c.noQuota), append(p.Orders, int32(i)) //nolint:gosec // a handful of rows
	}
	added, err := q.InsertDefaultLeaveCategories(ctx, p)
	if err != nil {
		return 0, fmt.Errorf("insert default leave categories: %w", err)
	}
	return added, nil
}
