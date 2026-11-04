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

type defaultStakeholderType struct{ code, name string }

// The stakeholder types every organization starts with, without a default phone: the numbers are
// the organization's to fill in. Each goes in while its code was never used (see
// InsertDefaultStakeholderTypes).
var defaultStakeholderTypes = []defaultStakeholderType{
	{"POLICE", "Polisi"}, {"HOSPITAL", "Rumah Sakit"}, {"FIRE_DEPARTMENT", "Pemadam Kebakaran"},
}

// Who each default panic type calls. "Lainnya" calls nobody in particular.
var defaultPanicStakeholders = []struct{ panicCode, stakeholderCode string }{
	{"SECURITY", "POLICE"}, {"MEDICAL", "HOSPITAL"}, {"FIRE", "FIRE_DEPARTMENT"},
}

type defaultAppLink struct{ name, iconKey string }

// The links the home screen has a place for. They stay inactive, with no target, until the real
// links are provided (DL-16: SiAPP links to these apps and nothing more).
var defaultAppLinks = []defaultAppLink{{"E-Koperasi Janusa", "koperasi"}, {"IKI", "iki"}}

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

	stakeholders, err := ensureStakeholderTypes(ctx, q, now)
	if err != nil {
		return 0, err
	}
	total += stakeholders

	codes, names, orders := make([]string, 0, len(defaultPanicTypes)), make([]string, 0, len(defaultPanicTypes)), make([]int32, 0, len(defaultPanicTypes))
	for i, t := range defaultPanicTypes {
		codes, names, orders = append(codes, t.code), append(names, t.name), append(orders, int32(i)) //nolint:gosec // a handful of rows
	}
	n, err := q.InsertDefaultPanicTypes(ctx, db.InsertDefaultPanicTypesParams{Now: ts(now), Codes: codes, Names: names, Orders: orders})
	if err != nil {
		return 0, fmt.Errorf("insert default panic types: %w", err)
	}
	total += n

	// ⚠ The default panic types are linked to their default stakeholders only in the call that
	// created the stakeholders: after that the link is the admin's to keep or clear.
	if stakeholders > 0 {
		panicCodes, stakeholderCodes := make([]string, 0, len(defaultPanicStakeholders)), make([]string, 0, len(defaultPanicStakeholders))
		for _, l := range defaultPanicStakeholders {
			panicCodes, stakeholderCodes = append(panicCodes, l.panicCode), append(stakeholderCodes, l.stakeholderCode)
		}
		linked, err := q.LinkDefaultPanicTypes(ctx, db.LinkDefaultPanicTypesParams{
			Now: ts(now), PanicCodes: panicCodes, StakeholderCodes: stakeholderCodes,
		})
		if err != nil {
			return 0, fmt.Errorf("link default panic types: %w", err)
		}
		total += linked
	}

	links, err := ensureAppLinks(ctx, q, now)
	if err != nil {
		return 0, err
	}
	total += links

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

func ensureStakeholderTypes(ctx context.Context, q *db.Queries, now time.Time) (int64, error) {
	codes, names, orders := make([]string, 0, len(defaultStakeholderTypes)), make([]string, 0, len(defaultStakeholderTypes)), make([]int32, 0, len(defaultStakeholderTypes))
	for i, t := range defaultStakeholderTypes {
		codes, names, orders = append(codes, t.code), append(names, t.name), append(orders, int32(i)) //nolint:gosec // a handful of rows
	}
	n, err := q.InsertDefaultStakeholderTypes(ctx, db.InsertDefaultStakeholderTypesParams{Now: ts(now), Codes: codes, Names: names, Orders: orders})
	if err != nil {
		return 0, fmt.Errorf("insert default stakeholder types: %w", err)
	}
	return n, nil
}

func ensureAppLinks(ctx context.Context, q *db.Queries, now time.Time) (int64, error) {
	names, icons, orders := make([]string, 0, len(defaultAppLinks)), make([]string, 0, len(defaultAppLinks)), make([]int32, 0, len(defaultAppLinks))
	for i, l := range defaultAppLinks {
		names, icons, orders = append(names, l.name), append(icons, l.iconKey), append(orders, int32(i)) //nolint:gosec // a handful of rows
	}
	n, err := q.InsertDefaultAppLinks(ctx, db.InsertDefaultAppLinksParams{Now: ts(now), Names: names, IconKeys: icons, Orders: orders})
	if err != nil {
		return 0, fmt.Errorf("insert default app links: %w", err)
	}
	return n, nil
}
