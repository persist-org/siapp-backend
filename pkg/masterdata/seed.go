package masterdata

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

type defaultPanicType struct{ code, name string }

// The panic types every organization starts with. "Lainnya" (manual §2.12) is the one the client
// adds more types next to.
var defaultPanicTypes = []defaultPanicType{
	{"SECURITY", "Keamanan"}, {"MEDICAL", "Medis"}, {"FIRE", "Kebakaran"}, {"OTHER", "Lainnya"},
}

type defaultCategory struct{ name, urgency string }

var defaultReportCategories = []defaultCategory{
	{"Penemuan Barang", ""}, {"Kejadian", ""}, {"Kehilangan", ""}, {"Lainnya", ""},
}

var defaultInstructionCategories = []defaultCategory{
	{"Umum", UrgencyNormal}, {"Pengecekan", UrgencyHigh},
}

// EnsureDefaults gives an organization its starting master data, in a transaction of its own, and
// returns how many rows it added. It runs when an organization is first seen (identity login,
// like access.EnsureDefaults) and is safe to call again: a second call adds nothing.
//
// Insert-if-missing only, never an overwrite: a panic type is added when its code is missing, and
// the categories of a kind only while the organization has none of that kind at all. So an admin
// who renamed, deactivated or deleted a default is not given it back, and a default that a later
// release adds reaches panic types but not categories of organizations that already have some.
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
	var total int64

	codes, names, orders := make([]string, 0, len(defaultPanicTypes)), make([]string, 0, len(defaultPanicTypes)), make([]int32, 0, len(defaultPanicTypes))
	for i, t := range defaultPanicTypes {
		codes, names, orders = append(codes, t.code), append(names, t.name), append(orders, int32(i)) //nolint:gosec // a handful of rows
	}
	n, err := q.InsertDefaultPanicTypes(ctx, db.InsertDefaultPanicTypesParams{Now: ts(now), Codes: codes, Names: names, Orders: orders})
	if err != nil {
		return 0, fmt.Errorf("insert default panic types: %w", err)
	}
	total += n

	for _, set := range []struct {
		kind Kind
		rows []defaultCategory
	}{{KindReport, defaultReportCategories}, {KindInstruction, defaultInstructionCategories}} {
		names, urgencies, orders := make([]string, 0, len(set.rows)), make([]string, 0, len(set.rows)), make([]int32, 0, len(set.rows))
		for i, c := range set.rows {
			names, urgencies, orders = append(names, c.name), append(urgencies, c.urgency), append(orders, int32(i)) //nolint:gosec // a handful of rows
		}
		n, err := q.InsertDefaultCategories(ctx, db.InsertDefaultCategoriesParams{
			Kind: set.kind, Now: ts(now), Names: names, Urgencies: urgencies, Orders: orders,
		})
		if err != nil {
			return 0, fmt.Errorf("insert default %s categories: %w", set.kind, err)
		}
		total += n
	}
	return total, nil
}
