package masterdata

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// Kind is which contract resource a category belongs to.
//
// ⚠ One table serves two contract resources (/report-categories and /instruction-categories):
// they have the same shape and the same rules. The handlers fix the kind from the path, and no
// request body has a field for it, so a category cannot be moved from one kind to the other.
type Kind = db.CategoryKind

// The two kinds of category.
const (
	KindReport      = db.CategoryKindREPORT
	KindInstruction = db.CategoryKindINSTRUCTION
)

// The urgency levels of an instruction (manual §2.8).
const (
	UrgencyLow    = "LOW"
	UrgencyNormal = "NORMAL"
	UrgencyHigh   = "HIGH"
)

// Category is a report or instruction category.
type Category struct {
	ID             int64
	OrganizationID int64
	Kind           Kind
	Name           string
	// Urgency is set for an instruction category and "" for a report category.
	Urgency string
	// BusinessLines are the lines the category is offered to; empty means all of them.
	BusinessLines []string
	SortOrder     int
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	deletedAt     *time.Time
}

// CategoryInput is a request to create a category. The json tags carry the contract's names.
type CategoryInput struct {
	Name string `json:"name" validate:"required,notblank,max=500"`
	// Urgency is required for an instruction category and rejected for a report category.
	Urgency       string   `json:"urgency" validate:"omitempty,oneof=LOW NORMAL HIGH"`
	BusinessLines []string `json:"business_lines" validate:"omitempty,max=2,dive,oneof=security cleaning"`
	SortOrder     *int     `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active        *bool    `json:"is_active"`
}

// CategoryUpdate is a request to change a category. A field that is absent is left as it is;
// business_lines: [] means "all business lines".
type CategoryUpdate struct {
	Name          *string  `json:"name" validate:"omitempty,notblank,max=500"`
	Urgency       *string  `json:"urgency"`
	BusinessLines []string `json:"business_lines" validate:"omitempty,max=2,dive,oneof=security cleaning"`
	SortOrder     *int     `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active        *bool    `json:"is_active"`
}

// ListFilter narrows an admin list. Zero values match everything.
type ListFilter struct {
	Q      string
	Active *bool
	Limit  int
	Offset int
}

// ListCategories returns one page of the organization's categories of a kind, and their total.
func (s *Service) ListCategories(ctx context.Context, actor auth.Principal, scope access.AdminScope, kind Kind, f ListFilter) ([]Category, int, error) {
	if !scope.All {
		return nil, 0, access.ErrOrgAdminOnly
	}
	var out []Category
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListCategories(ctx, db.ListCategoriesParams{
			Kind: kind, Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list categories: %w", err)
		}
		for _, r := range rows {
			out = append(out, toCategory(r))
		}
		total, err = q.CountCategories(ctx, db.CountCategoriesParams{Kind: kind, Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count categories: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetCategory returns one category of a kind. A category of the other kind is not found here.
func (s *Service) GetCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, kind Kind, id int64) (Category, error) {
	if !scope.All {
		return Category{}, access.ErrOrgAdminOnly
	}
	var out Category
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetCategory(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.Kind != kind) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get category: %w", err)
		}
		out = toCategory(row)
		return nil
	})
	return out, err
}

// CreateCategory adds a category of a kind.
//
// The name is taken per kind and per business line by every category that is not deleted, active
// or not, ignoring case: "Kejadian" may exist once for security and once for cleaning, but not
// twice for security, and not for all lines next to either (CATEGORY_NAME_TAKEN).
func (s *Service) CreateCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, kind Kind, in CategoryInput) (Category, error) {
	if !scope.All {
		return Category{}, access.ErrOrgAdminOnly
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return Category{}, err
	}
	// ⚠ The urgency rule is enforced here, for the friendly error: an instruction has one and a
	// report has none. The table's CHECK is only the backstop.
	urgency, err := urgencyFor(kind, in.Urgency)
	if err != nil {
		return Category{}, err
	}
	lines := toLines(in.BusinessLines)
	active := in.Active == nil || *in.Active

	var out Category
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := claimCategoryName(ctx, q, actor.OrganizationID, kind, 0, name, lines); err != nil {
			return err
		}
		row, err := q.InsertCategory(ctx, db.InsertCategoryParams{
			Kind: kind, Name: name, Urgency: urgency, BusinessLines: lines,
			SortOrder: intPtr(in.SortOrder), Active: active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toCategory(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.category.create", EntityType: "category", EntityID: out.ID,
			Metadata: map[string]any{"after": categoryMeta(out)},
		})
	})
	return out, categoryWriteError(err, "create category")
}

// UpdateCategory changes a category. A change that leaves every field as it was (an empty body,
// or the same values sent again) writes nothing: no updated_at, so no delta-sync traffic, and no
// audit row.
func (s *Service) UpdateCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, kind Kind, id int64, in CategoryUpdate) (Category, error) {
	if !scope.All {
		return Category{}, access.ErrOrgAdminOnly
	}
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return Category{}, err
		}
		name = &cleaned
	}
	var urgency db.NullUrgency
	if in.Urgency != nil {
		switch {
		case kind == KindReport && *in.Urgency == "":
			// "No urgency" on a report category is what it already has: nothing to change.
		case kind == KindInstruction && *in.Urgency == "":
			return Category{}, validationError("urgency", "Tidak boleh dikosongkan untuk kategori instruksi.")
		default:
			var err error
			if urgency, err = urgencyFor(kind, *in.Urgency); err != nil {
				return Category{}, err
			}
		}
	}

	var out Category
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetCategoryForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && before.Kind != kind) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get category: %w", err)
		}
		was := toCategory(before)

		// What the row would become; fields that are absent keep their value.
		next := was
		if name != nil {
			next.Name = *name
		}
		if urgency.Valid {
			next.Urgency = string(urgency.Urgency)
		}
		if in.BusinessLines != nil {
			next.BusinessLines = toLines(in.BusinessLines)
		}
		if in.SortOrder != nil {
			next.SortOrder = *in.SortOrder
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		if sameCategory(was, next) {
			out = was
			return nil
		}
		// The name is checked whenever the name or the lines change. Reactivating needs no
		// check: an inactive category already holds its name.
		if next.Name != was.Name || !slices.Equal(next.BusinessLines, was.BusinessLines) {
			if err := claimCategoryName(ctx, q, actor.OrganizationID, kind, id, next.Name, next.BusinessLines); err != nil {
				return err
			}
		}

		params := db.UpdateCategoryParams{
			ID: id, Name: textPtr(name), Urgency: urgency, SortOrder: intPtr(in.SortOrder),
			Active: boolOrNull(in.Active), Now: ts(s.clock.Now()),
		}
		if in.BusinessLines != nil {
			params.BusinessLines = next.BusinessLines
		}
		row, err := q.UpdateCategory(ctx, params)
		if err != nil {
			return err
		}
		out = toCategory(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.category.update", EntityType: "category", EntityID: id,
			Metadata: map[string]any{"before": categoryMeta(was), "after": categoryMeta(out)},
		})
	})
	return out, categoryWriteError(err, "update category")
}

// claimCategoryName takes the lock on a name and fails with CATEGORY_NAME_TAKEN when another
// category (not id) holds it on an overlapping business line. The lock is held to the end of the
// transaction, so a second admin writing the same name waits and then sees the first one's row.
func claimCategoryName(ctx context.Context, q *db.Queries, orgID int64, kind Kind, id int64, name string, lines []string) error {
	key := fmt.Sprintf("masterdata:category:%d:%s:%s", orgID, kind, strings.ToLower(name))
	if err := q.LockMasterName(ctx, key); err != nil {
		return fmt.Errorf("lock category name: %w", err)
	}
	clash, err := q.CategoryNameClash(ctx, db.CategoryNameClashParams{Kind: kind, ID: id, Name: name, Lines: lines})
	if err != nil {
		return fmt.Errorf("check category name: %w", err)
	}
	if clash {
		return ErrCategoryNameTaken
	}
	return nil
}

// categoryWriteError maps the unique index (the backstop of claimCategoryName) to its reason and
// wraps database failures. Errors the client should see are returned as they are.
func categoryWriteError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err):
		return ErrCategoryNameTaken
	case isAppErr(err):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

func sameCategory(a, b Category) bool {
	return a.Name == b.Name && a.Urgency == b.Urgency && slices.Equal(a.BusinessLines, b.BusinessLines) &&
		a.SortOrder == b.SortOrder && a.Active == b.Active
}

// DeleteCategory deletes a category that nothing points at. One that something points at is
// MASTER_IN_USE: the client offers to deactivate it instead (DL-09).
//
// The row stays, deleted and inactive, so a delta pull can send it to the apps as a tombstone.
func (s *Service) DeleteCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, kind Kind, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The lock first: a reference being added waits for this delete, and then fails on it.
		before, err := q.GetCategoryForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && before.Kind != kind) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get category: %w", err)
		}
		for _, inUse := range s.categoryUsage[kind] {
			used, err := inUse(ctx, q, id)
			if err != nil {
				return fmt.Errorf("check category usage: %w", err)
			}
			if used {
				return ErrMasterInUse
			}
		}
		if _, err := q.SoftDeleteCategory(ctx, db.SoftDeleteCategoryParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete category: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.category.delete", EntityType: "category", EntityID: id,
			Metadata: map[string]any{"before": categoryMeta(toCategory(before))},
		})
	})
}

// AssertCategory checks that a category exists in the caller's organization and is of the kind:
// ErrCategoryNotFound, or ErrCategoryKindMismatch when it is the other kind. Reports and
// instructions call it before they accept a category_id. The organization is the caller's, from
// ctx (auth.From), so another organization's category is simply not found.
//
// An inactive category passes: a report written offline before an admin deactivated its category
// must not be lost. The app stops offering the category through the delta pull. A deleted one
// does not pass.
func (s *Service) AssertCategory(ctx context.Context, id int64, kind Kind) error {
	p, ok := auth.From(ctx)
	if !ok {
		return errors.New("masterdata: AssertCategory needs an authenticated caller in ctx")
	}
	return database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		return AssertCategoryTx(ctx, q, id, kind)
	})
}

// AssertCategoryTx is AssertCategory inside the tenant transaction of q, for a use case that
// checks the category in the transaction that writes its own row.
func AssertCategoryTx(ctx context.Context, q *db.Queries, id int64, kind Kind) error {
	row, err := q.GetCategory(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCategoryNotFound
	}
	if err != nil {
		return fmt.Errorf("get category: %w", err)
	}
	if row.Kind != kind {
		return ErrCategoryKindMismatch
	}
	return nil
}

// FieldCategories is the full pull of the field app: the active categories of a kind that are
// offered to the business lines of the caller's projects. It returns the server time the app
// starts its delta pulls from, taken before the read.
func (s *Service) FieldCategories(ctx context.Context, p auth.Principal, kind Kind) ([]Category, time.Time, error) {
	serverTime := s.clock.Now()
	var out []Category
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		lines, err := s.callerLines(ctx, q, p, serverTime)
		if err != nil {
			return err
		}
		rows, err := q.ListActiveCategoriesForLines(ctx, db.ListActiveCategoriesForLinesParams{Kind: kind, Lines: lines})
		if err != nil {
			return fmt.Errorf("list active categories: %w", err)
		}
		for _, r := range rows {
			out = append(out, toCategory(r))
		}
		return nil
	})
	return out, serverTime, err
}

// FieldCategoryChanges is the delta pull: what changed after `since`.
//
// ⚠ DL-57: a row that was deleted or deactivated is sent in Deleted, so the offline cache can
// drop it; the full pull above returns active rows only, which cannot say a row is gone. A row
// whose business lines changed so that it is no longer offered to the caller is dropped the same
// way.
//
// ⚠ The caller's own business lines can change too (they move to a cleaning project) without any
// category changing. When their memberships may have changed after `since`, the answer is the
// whole set instead: every offered row in Data and every other row in Deleted, so the cache ends
// up exactly right. A master is a few dozen rows, so this costs nothing worth saving.
func (s *Service) FieldCategoryChanges(ctx context.Context, p auth.Principal, kind Kind, since time.Time) (Changes[Category], error) {
	out := Changes[Category]{ServerTime: s.clock.Now()}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		lines, err := s.callerLines(ctx, q, p, out.ServerTime)
		if err != nil {
			return err
		}
		after := sinceBound(since)
		moved, err := q.MembershipsChangedSince(ctx, db.MembershipsChangedSinceParams{
			Since: ts(after), Now: ts(out.ServerTime), PersonnelID: p.PersonnelID,
		})
		if err != nil {
			return fmt.Errorf("check memberships: %w", err)
		}
		if moved {
			after = time.Unix(0, 0)
		}
		rows, err := q.ListCategoryChanges(ctx, db.ListCategoryChangesParams{Kind: kind, After: ts(after)})
		if err != nil {
			return fmt.Errorf("list category changes: %w", err)
		}
		for _, r := range rows {
			c := toCategory(r)
			if !r.Active || r.DeletedAt.Valid || !offeredTo(c, lines) {
				gone := c.UpdatedAt
				if c.deletedAt != nil {
					gone = *c.deletedAt
				}
				out.Deleted = append(out.Deleted, Tombstone{ID: c.ID, DeletedAt: gone})
				continue
			}
			out.Upserted = append(out.Upserted, c)
		}
		return nil
	})
	return out, err
}

// callerLines is the business lines of the projects the caller serves in today.
func (s *Service) callerLines(ctx context.Context, q *db.Queries, p auth.Principal, now time.Time) ([]string, error) {
	lines, err := q.ListBusinessLinesOfPersonnel(ctx, db.ListBusinessLinesOfPersonnelParams{PersonnelID: p.PersonnelID, Now: ts(now)})
	if err != nil {
		return nil, fmt.Errorf("list business lines of personnel: %w", err)
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, string(l))
	}
	return out, nil
}

// offeredTo reports whether a category is offered to someone who serves in these business lines.
// A category with no business lines is offered to everyone.
func offeredTo(c Category, lines []string) bool {
	if len(c.BusinessLines) == 0 {
		return true
	}
	for _, l := range lines {
		if slices.Contains(c.BusinessLines, l) {
			return true
		}
	}
	return false
}

// urgencyFor applies the urgency rule: required for an instruction, rejected for a report.
func urgencyFor(kind Kind, urgency string) (db.NullUrgency, error) {
	switch {
	case kind == KindInstruction && urgency == "":
		return db.NullUrgency{}, validationError("urgency", "Wajib diisi untuk kategori instruksi (LOW, NORMAL atau HIGH).")
	case kind == KindReport && urgency != "":
		return db.NullUrgency{}, validationError("urgency", "Hanya untuk kategori instruksi.")
	case urgency == "":
		return db.NullUrgency{}, nil
	}
	switch urgency {
	case UrgencyLow, UrgencyNormal, UrgencyHigh:
		return db.NullUrgency{Urgency: db.Urgency(urgency), Valid: true}, nil
	}
	return db.NullUrgency{}, validationError("urgency", "Harus LOW, NORMAL atau HIGH.")
}

// toLines drops duplicates and sorts, so two requests for the same lines store the same array.
func toLines(in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	slices.Sort(out)
	return out
}

func toCategory(r db.Category) Category {
	c := Category{
		ID: r.ID, OrganizationID: r.OrganizationID, Kind: r.Kind, Name: r.Name,
		BusinessLines: append(make([]string, 0, len(r.BusinessLines)), r.BusinessLines...),
		SortOrder:     int(r.SortOrder), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.Urgency.Valid {
		c.Urgency = string(r.Urgency.Urgency)
	}
	if r.DeletedAt.Valid {
		t := r.DeletedAt.Time
		c.deletedAt = &t
	}
	return c
}

// categoryMeta is what an audit row keeps of a category.
func categoryMeta(c Category) map[string]any {
	return map[string]any{
		"kind": string(c.Kind), "name": c.Name, "urgency": c.Urgency, "business_lines": c.BusinessLines,
		"sort_order": c.SortOrder, "active": c.Active,
	}
}
