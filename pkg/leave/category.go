package leave

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// Type is what kind of request a category is for. The three kinds are what kpi-personalia-api
// calls leave, permit and sick.
type Type = db.LeaveCategoryType

// The three types of category.
const (
	TypeLeave  = db.LeaveCategoryTypeLEAVE
	TypePermit = db.LeaveCategoryTypePERMIT
	TypeSick   = db.LeaveCategoryTypeSICK
)

var codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)

// Category is a leave, permit or sick category. The flags say what is special about it (see the
// migration): behavior is never keyed on the code or the name.
type Category struct {
	ID                int64
	OrganizationID    int64
	Code              string
	Name              string
	Type              Type
	IsAnnual          bool
	SkipsDayCheck     bool
	RequiresTimeRange bool
	RequiresChangeDay bool
	MakesLeaveDays    bool
	ExcusesLateness   bool
	HasQuota          bool
	SortOrder         int
	Active            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func toCategory(r db.LeaveCategory) Category {
	return Category{
		ID: r.ID, OrganizationID: r.OrganizationID, Code: r.Code, Name: r.Name, Type: r.Type,
		IsAnnual: r.IsAnnual, SkipsDayCheck: r.SkipsDayCheck, RequiresTimeRange: r.RequiresTimeRange,
		RequiresChangeDay: r.RequiresChangeDay, MakesLeaveDays: r.MakesLeaveDays, ExcusesLateness: r.ExcusesLateness,
		HasQuota: r.HasQuota, SortOrder: int(r.SortOrder), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// CategoryInput is a request to create a category. The code is the category's identity and, with
// the type and is_annual, never changes afterwards.
type CategoryInput struct {
	Code              string `json:"code" validate:"required,max=40"`
	Name              string `json:"name" validate:"required,notblank,max=100"`
	Type              string `json:"type" validate:"required,oneof=LEAVE PERMIT SICK"`
	IsAnnual          bool   `json:"is_annual"`
	SkipsDayCheck     *bool  `json:"skips_day_check"`
	RequiresTimeRange *bool  `json:"requires_time_range"`
	RequiresChangeDay *bool  `json:"requires_change_day"`
	// MakesLeaveDays defaults to true, HasQuota to false for sick leave and true otherwise.
	MakesLeaveDays  *bool `json:"makes_leave_days"`
	ExcusesLateness *bool `json:"excuses_lateness"`
	HasQuota        *bool `json:"has_quota"`
	SortOrder       *int  `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active          *bool `json:"is_active"`
}

// CategoryUpdate is a request to change a category. There is no field for the code, the type or
// is_annual: a client that sends one gets 422 from the decoder's unknown-field rule.
type CategoryUpdate struct {
	Name              *string `json:"name" validate:"omitempty,notblank,max=100"`
	SkipsDayCheck     *bool   `json:"skips_day_check"`
	RequiresTimeRange *bool   `json:"requires_time_range"`
	RequiresChangeDay *bool   `json:"requires_change_day"`
	MakesLeaveDays    *bool   `json:"makes_leave_days"`
	ExcusesLateness   *bool   `json:"excuses_lateness"`
	HasQuota          *bool   `json:"has_quota"`
	SortOrder         *int    `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active            *bool   `json:"is_active"`
}

// CategoryFilter narrows a list of categories.
type CategoryFilter struct {
	Type   Type
	Active *bool
	Q      string
	Limit  int
	Offset int
}

// validateFlags checks that the flags of a category agree with each other. The table has the same
// CHECKs as a backstop; this gives the admin a message about the field.
func validateFlags(c Category) error {
	switch {
	case c.IsAnnual && c.Type != TypeLeave:
		return validationError("is_annual", "Hanya kategori bertipe LEAVE yang bisa tahunan.")
	case c.IsAnnual && !c.HasQuota:
		return validationError("has_quota", "Cuti tahunan selalu memakai saldo.")
	case c.IsAnnual && !c.MakesLeaveDays:
		return validationError("makes_leave_days", "Cuti tahunan selalu menandai hari cuti.")
	case c.Type == TypeSick && c.HasQuota:
		return validationError("has_quota", "Sakit tidak memakai kuota.")
	case c.RequiresTimeRange && c.RequiresChangeDay:
		return validationError("requires_change_day", "Kategori tidak boleh meminta rentang jam dan tukar hari sekaligus.")
	}
	return nil
}

func requireOrgAdmin(scope access.AdminScope) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return nil
}

func cleanName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", validationError("name", "Wajib diisi.")
	case utf8.RuneCountInString(name) > 100:
		return "", validationError("name", "Maksimum 100 karakter.")
	}
	return name, nil
}

// ListCategories returns one page of the organization's categories (active or not), and their total.
func (s *Service) ListCategories(ctx context.Context, actor auth.Principal, scope access.AdminScope, f CategoryFilter) ([]Category, int, error) {
	if err := requireOrgAdmin(scope); err != nil {
		return nil, 0, err
	}
	var out []Category
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		params := db.ListLeaveCategoriesParams{
			Type: typeOrNull(f.Type), Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		}
		rows, err := q.ListLeaveCategories(ctx, params)
		if err != nil {
			return fmt.Errorf("list leave categories: %w", err)
		}
		for _, r := range rows {
			out = append(out, toCategory(r))
		}
		total, err = q.CountLeaveCategories(ctx, db.CountLeaveCategoriesParams{Type: params.Type, Active: params.Active, Q: params.Q})
		if err != nil {
			return fmt.Errorf("count leave categories: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

func typeOrNull(t Type) db.NullLeaveCategoryType {
	return db.NullLeaveCategoryType{LeaveCategoryType: t, Valid: t != ""}
}

// CategoryDetail is a category with its quotas, for the admin screen.
type CategoryDetail struct {
	Category
	Quotas Quotas
}

// GetCategory returns one category with its quotas.
func (s *Service) GetCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (CategoryDetail, error) {
	if err := requireOrgAdmin(scope); err != nil {
		return CategoryDetail{}, err
	}
	var out CategoryDetail
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetLeaveCategory(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave category: %w", err)
		}
		out.Category = toCategory(row)
		out.Quotas, err = quotasOf(ctx, q, row.ID)
		return err
	})
	return out, err
}

// CreateCategory adds a category. The code is checked as it is sent, and is taken for good.
func (s *Service) CreateCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, in CategoryInput) (Category, error) {
	if err := requireOrgAdmin(scope); err != nil {
		return Category{}, err
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return Category{}, err
	}
	code := strings.TrimSpace(in.Code)
	if !codePattern.MatchString(code) {
		return Category{}, validationError("code", "Harus huruf besar, angka atau garis bawah, diawali huruf (2 sampai 40 karakter).")
	}
	c := Category{
		Code: code, Name: name, Type: Type(in.Type), IsAnnual: in.IsAnnual,
		SkipsDayCheck: in.SkipsDayCheck != nil && *in.SkipsDayCheck, RequiresTimeRange: in.RequiresTimeRange != nil && *in.RequiresTimeRange,
		RequiresChangeDay: in.RequiresChangeDay != nil && *in.RequiresChangeDay,
		MakesLeaveDays:    in.MakesLeaveDays == nil || *in.MakesLeaveDays,
		ExcusesLateness:   in.ExcusesLateness != nil && *in.ExcusesLateness,
		HasQuota:          in.HasQuota == nil && Type(in.Type) != TypeSick || in.HasQuota != nil && *in.HasQuota,
		Active:            in.Active == nil || *in.Active,
	}
	if err := validateFlags(c); err != nil {
		return Category{}, err
	}

	var out Category
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.InsertLeaveCategory(ctx, db.InsertLeaveCategoryParams{
			Code: c.Code, Name: c.Name, Type: c.Type, IsAnnual: c.IsAnnual, SkipsDayCheck: c.SkipsDayCheck,
			RequiresTimeRange: c.RequiresTimeRange, RequiresChangeDay: c.RequiresChangeDay, MakesLeaveDays: c.MakesLeaveDays,
			ExcusesLateness: c.ExcusesLateness, HasQuota: c.HasQuota, SortOrder: intOrNull(in.SortOrder), Active: c.Active,
			Now: ts(s.clock.Now()),
		})
		if uniqueConstraint(err) == "leave_categories_code_uq" {
			return ErrCategoryCodeTaken
		}
		if err != nil {
			return fmt.Errorf("insert leave category: %w", err)
		}
		out = toCategory(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.category.create", EntityType: "leave_category", EntityID: row.ID,
			Metadata: map[string]any{"code": row.Code, "type": string(row.Type)},
		})
	})
	return out, err
}

// UpdateCategory changes a category. A category is deactivated (is_active = false), never deleted.
func (s *Service) UpdateCategory(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in CategoryUpdate) (Category, error) {
	if err := requireOrgAdmin(scope); err != nil {
		return Category{}, err
	}
	var name *string
	if in.Name != nil {
		n, err := cleanName(*in.Name)
		if err != nil {
			return Category{}, err
		}
		name = &n
	}
	var out Category
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		cur, err := q.GetLeaveCategoryForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCategoryNotFound
		}
		if err != nil {
			return fmt.Errorf("get leave category: %w", err)
		}
		merged := toCategory(cur)
		apply := func(dst *bool, v *bool) {
			if v != nil {
				*dst = *v
			}
		}
		apply(&merged.SkipsDayCheck, in.SkipsDayCheck)
		apply(&merged.RequiresTimeRange, in.RequiresTimeRange)
		apply(&merged.RequiresChangeDay, in.RequiresChangeDay)
		apply(&merged.MakesLeaveDays, in.MakesLeaveDays)
		apply(&merged.ExcusesLateness, in.ExcusesLateness)
		apply(&merged.HasQuota, in.HasQuota)
		if err := validateFlags(merged); err != nil {
			return err
		}
		row, err := q.UpdateLeaveCategory(ctx, db.UpdateLeaveCategoryParams{
			ID: cur.ID, Name: textPtrOrNull(name), SkipsDayCheck: boolOrNull(in.SkipsDayCheck),
			RequiresTimeRange: boolOrNull(in.RequiresTimeRange), RequiresChangeDay: boolOrNull(in.RequiresChangeDay),
			MakesLeaveDays: boolOrNull(in.MakesLeaveDays), ExcusesLateness: boolOrNull(in.ExcusesLateness),
			HasQuota: boolOrNull(in.HasQuota), SortOrder: intOrNull(in.SortOrder), Active: boolOrNull(in.Active), Now: ts(s.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update leave category: %w", err)
		}
		out = toCategory(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "leave.category.update", EntityType: "leave_category", EntityID: row.ID,
			Metadata: map[string]any{"code": row.Code, "active": row.Active},
		})
	})
	return out, err
}

// FieldCategories returns the active categories the field offers (optionally of one type) and the
// server time the next delta pull starts from.
func (s *Service) FieldCategories(ctx context.Context, p auth.Principal, t Type) ([]Category, time.Time, error) {
	serverTime := s.clock.Now()
	var out []Category
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListActiveLeaveCategories(ctx, typeOrNull(t))
		if err != nil {
			return fmt.Errorf("list active leave categories: %w", err)
		}
		for _, r := range rows {
			out = append(out, toCategory(r))
		}
		return nil
	})
	return out, serverTime, err
}

// Tombstone says a category is gone for the app's cache: it was deactivated.
type Tombstone struct {
	Code      string
	DeletedAt time.Time
}

// CategoryChanges is the answer of a delta pull.
type CategoryChanges struct {
	ServerTime time.Time
	Upserted   []Category
	Deleted    []Tombstone
}

// FieldCategoryChanges is the delta pull. ⚠ DL-57: a deactivated category is sent in Deleted, so
// the offline cache can drop it (the full pull returns active rows only).
func (s *Service) FieldCategoryChanges(ctx context.Context, p auth.Principal, t Type, since time.Time) (CategoryChanges, error) {
	out := CategoryChanges{ServerTime: s.clock.Now()}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListLeaveCategoryChanges(ctx, db.ListLeaveCategoryChangesParams{After: ts(sinceBound(since)), Type: typeOrNull(t)})
		if err != nil {
			return fmt.Errorf("list leave category changes: %w", err)
		}
		for _, r := range rows {
			c := toCategory(r)
			if !c.Active {
				out.Deleted = append(out.Deleted, Tombstone{Code: c.Code, DeletedAt: c.UpdatedAt})
				continue
			}
			out.Upserted = append(out.Upserted, c)
		}
		return nil
	})
	return out, err
}

// categoryByCode reads a category inside the caller's transaction.
func categoryByCode(ctx context.Context, q *db.Queries, code string) (db.LeaveCategory, error) {
	row, err := q.GetLeaveCategory(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LeaveCategory{}, ErrCategoryNotFound
	}
	if err != nil {
		return db.LeaveCategory{}, fmt.Errorf("get leave category: %w", err)
	}
	return row, nil
}
