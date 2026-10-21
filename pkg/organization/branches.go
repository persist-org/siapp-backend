package organization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Branch is one kantor cabang. Its description is its name.
type Branch struct {
	Code          string
	RegionCode    string
	CompanyLetter string
	ManagerName   *string
	City          *string
	Phone         *string
	Fax           *string
	Email         *string
	Address       *string
	ZipCode       *string
	Description   string
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// BranchInput is a request to create a branch. The code is built from RegionCode and
// CompanyLetter (D19), so the client never sends one.
type BranchInput struct {
	RegionCode    string  `json:"region_code" validate:"required"`
	CompanyLetter string  `json:"company_letter" validate:"required"`
	Description   string  `json:"description" validate:"required,notblank,max=200"`
	ManagerName   *string `json:"manager_name" validate:"omitempty,max=200"`
	City          *string `json:"city" validate:"omitempty,max=100"`
	Phone         *string `json:"phone" validate:"omitempty,max=50"`
	Fax           *string `json:"fax" validate:"omitempty,max=50"`
	Email         *string `json:"email" validate:"omitempty,max=200"`
	Address       *string `json:"address" validate:"omitempty,max=500"`
	ZipCode       *string `json:"zip_code" validate:"omitempty,max=20"`
	Active        *bool   `json:"is_active"`
}

// BranchUpdate is a request to change a branch. The code, the region and the letter have no
// field: the code is built from them, so a client that sends one gets 422 from the decoder's
// unknown-field rule.
type BranchUpdate struct {
	Description *string `json:"description" validate:"omitempty,notblank,max=200"`
	ManagerName *string `json:"manager_name" validate:"omitempty,max=200"`
	City        *string `json:"city" validate:"omitempty,max=100"`
	Phone       *string `json:"phone" validate:"omitempty,max=50"`
	Fax         *string `json:"fax" validate:"omitempty,max=50"`
	Email       *string `json:"email" validate:"omitempty,max=200"`
	Address     *string `json:"address" validate:"omitempty,max=500"`
	ZipCode     *string `json:"zip_code" validate:"omitempty,max=20"`
	Active      *bool   `json:"is_active"`
}

// BranchFilter narrows a list. Zero values match everything.
type BranchFilter struct {
	Q          string
	RegionCode string
	Active     *bool
	Limit      int
	Offset     int
}

// Branches manages the kantor cabang of an organization.
type Branches struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewBranches returns the Branches usecase.
func NewBranches(pool *pgxpool.Pool, c clock.Clock) *Branches {
	return &Branches{pool: pool, clock: c}
}

// List returns one page of the branches the caller may see, and their total. A REGION_ADMIN
// sees only the branches of their own region.
func (b *Branches) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f BranchFilter) ([]Branch, int, error) {
	var out []Branch
	var total int64
	err := database.WithTenant(ctx, b.pool, actor.OrganizationID, func(q *db.Queries) error {
		// A page this deep is nonsense: an empty answer, not an overflow.
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListBranches(ctx, db.ListBranchesParams{
			ScopeRegion: scopeFilter(scope), RegionCode: textOrNull(f.RegionCode), Active: boolOrNull(f.Active),
			Q: textOrNull(f.Q), PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list branches: %w", err)
		}
		for _, row := range rows {
			out = append(out, toBranch(row))
		}
		total, err = q.CountBranches(ctx, db.CountBranchesParams{
			ScopeRegion: scopeFilter(scope), RegionCode: textOrNull(f.RegionCode), Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count branches: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one branch. A REGION_ADMIN may read only one of their own region: another is
// SCOPE_DENIED, answered before the database is asked so the code is not probed.
func (b *Branches) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (Branch, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Branch{}, access.ErrOutsideAdminScope
	}
	var out Branch
	err := database.WithTenant(ctx, b.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetBranch(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBranchNotFound
		}
		if err != nil {
			return fmt.Errorf("get branch: %w", err)
		}
		out = toBranch(row)
		return nil
	})
	return out, err
}

// Create adds a branch under a region. A REGION_ADMIN may create only under their own
// region. A code that is taken, even by a deleted branch, is CODE_TAKEN.
func (b *Branches) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in BranchInput) (Branch, error) {
	in.RegionCode = strings.TrimSpace(in.RegionCode)
	in.CompanyLetter = strings.TrimSpace(in.CompanyLetter)
	code, err := BuildBranchCode(in.RegionCode, in.CompanyLetter)
	if err != nil {
		return Branch{}, err
	}
	if !scope.AllowsRegion(in.RegionCode) {
		return Branch{}, access.ErrOutsideAdminScope
	}
	active := in.Active == nil || *in.Active

	var out Branch
	err = database.WithTenant(ctx, b.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The region is locked, so a delete of it cannot commit between this check and the insert.
		if _, err := q.GetRegionForUpdate(ctx, in.RegionCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrRegionNotFound
		} else if err != nil {
			return fmt.Errorf("get region: %w", err)
		}
		row, err := q.CreateBranch(ctx, db.CreateBranchParams{
			Code: code, RegionCode: in.RegionCode, CompanyLetter: in.CompanyLetter,
			ManagerName: textPtr(in.ManagerName), City: textPtr(in.City), Phone: textPtr(in.Phone),
			Fax: textPtr(in.Fax), Email: textPtr(in.Email), Address: textPtr(in.Address), ZipCode: textPtr(in.ZipCode),
			Description: strings.TrimSpace(in.Description), Active: active, Now: ts(b.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toBranch(row)
		return recordAudit(ctx, q, actor, "branch.create", "branch", code, nil, branchMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Branch{}, ErrCodeTaken
	}
	if err != nil {
		return Branch{}, fmt.Errorf("create branch: %w", err)
	}
	return out, nil
}

// Update changes a branch's details. A REGION_ADMIN may change only their own region's.
func (b *Branches) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in BranchUpdate) (Branch, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Branch{}, access.ErrOutsideAdminScope
	}
	var out Branch
	err := database.WithTenant(ctx, b.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetBranchForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBranchNotFound
		}
		if err != nil {
			return fmt.Errorf("get branch: %w", err)
		}
		row, err := q.UpdateBranch(ctx, db.UpdateBranchParams{
			Code: code, Description: textPtr(trimmed(in.Description)), ManagerName: textPtr(in.ManagerName),
			City: textPtr(in.City), Phone: textPtr(in.Phone), Fax: textPtr(in.Fax), Email: textPtr(in.Email),
			Address: textPtr(in.Address), ZipCode: textPtr(in.ZipCode), Active: boolOrNull(in.Active), Now: ts(b.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update branch: %w", err)
		}
		out = toBranch(row)
		return recordAudit(ctx, q, actor, "branch.update", "branch", code, branchMeta(toBranch(before)), branchMeta(out))
	})
	return out, err
}

// Delete removes a branch (a soft delete: the row and its code stay, D19). A REGION_ADMIN
// may delete only their own region's.
//
// ⚠ A branch with an active client is HAS_ACTIVE_CHILDREN: its clients would be left with a
// parent that no longer shows up.
func (b *Branches) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) error {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return access.ErrOutsideAdminScope
	}
	return database.WithTenant(ctx, b.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetBranchForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBranchNotFound
		}
		if err != nil {
			return fmt.Errorf("get branch: %w", err)
		}
		clients, err := q.CountClientsOfBranch(ctx, code)
		if err != nil {
			return fmt.Errorf("count clients of branch: %w", err)
		}
		if clients > 0 {
			return ErrHasActiveChildren.WithMessage("Masih ada klien (aktif maupun nonaktif) di kantor cabang ini.")
		}
		if _, err := q.SoftDeleteBranch(ctx, db.SoftDeleteBranchParams{Code: code, Now: ts(b.clock.Now())}); err != nil {
			return fmt.Errorf("delete branch: %w", err)
		}
		return recordAudit(ctx, q, actor, "branch.delete", "branch", code, branchMeta(toBranch(before)), nil)
	})
}

func toBranch(r db.Branch) Branch {
	return Branch{
		Code: r.Code, RegionCode: r.RegionCode, CompanyLetter: r.CompanyLetter,
		ManagerName: textValue(r.ManagerName), City: textValue(r.City), Phone: textValue(r.Phone),
		Fax: textValue(r.Fax), Email: textValue(r.Email), Address: textValue(r.Address), ZipCode: textValue(r.ZipCode),
		Description: r.Description, Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// branchMeta is what an audit row keeps of a branch.
func branchMeta(b Branch) map[string]any {
	return map[string]any{
		"region_code": b.RegionCode, "company_letter": b.CompanyLetter, "description": b.Description,
		"manager_name": b.ManagerName, "city": b.City, "phone": b.Phone, "fax": b.Fax, "email": b.Email,
		"address": b.Address, "zip_code": b.ZipCode, "active": b.Active,
	}
}

// recordAudit writes one hierarchy audit row with its before and after (either may be nil).
func recordAudit(ctx context.Context, q *db.Queries, actor auth.Principal, action, entity, code string, before, after map[string]any) error {
	meta := map[string]any{}
	if before != nil {
		meta["before"] = before
	}
	if after != nil {
		meta["after"] = after
	}
	actorID := actor.PersonnelID
	if err := audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &actorID, Action: action, EntityType: entity, EntityCode: code, Metadata: meta,
	}); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
