package organization

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// regionCodePattern is the shape of a koordinator wilayah code: OS, then a number without a
// leading zero. The regions table has the same CHECK.
var regionCodePattern = regexp.MustCompile(`^OS[1-9][0-9]*$`)

// Region is one koordinator wilayah. Its description is its name ("Outsourcing 1").
type Region struct {
	Code        string
	Description string
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// RegionInput is a request to create a region. The json tags carry the contract's names.
type RegionInput struct {
	Code        string `json:"code" validate:"required"`
	Description string `json:"description" validate:"required,notblank,max=200"`
	Active      *bool  `json:"is_active"`
}

// RegionUpdate is a request to change a region. Only these two fields can change: the code
// is its identity (D19) and the request body type has no field for it, so a client that
// sends one gets 422 from the decoder's unknown-field rule.
type RegionUpdate struct {
	Description *string `json:"description" validate:"omitempty,notblank,max=200"`
	Active      *bool   `json:"is_active"`
}

// RegionFilter narrows a list. Zero values match everything.
type RegionFilter struct {
	Q      string
	Active *bool
	Limit  int
	Offset int
}

// Regions manages the koordinator wilayah of an organization.
type Regions struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewRegions returns the Regions usecase.
func NewRegions(pool *pgxpool.Pool, c clock.Clock) *Regions {
	return &Regions{pool: pool, clock: c}
}

// List returns one page of the regions the caller may see, and their total. An ORG_ADMIN
// sees all of them; a REGION_ADMIN only their own.
func (r *Regions) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f RegionFilter) ([]Region, int, error) {
	var out []Region
	var total int64
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		// A page this deep is nonsense: an empty answer, not an overflow.
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		regionCode := scopeFilter(scope)
		rows, err := q.ListRegions(ctx, db.ListRegionsParams{
			RegionCode: regionCode, Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list regions: %w", err)
		}
		for _, row := range rows {
			out = append(out, toRegion(row))
		}
		total, err = q.CountRegions(ctx, db.CountRegionsParams{RegionCode: regionCode, Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count regions: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one region. A REGION_ADMIN may read only their own: another one is
// SCOPE_DENIED, answered before the database is asked so the code is not probed.
func (r *Regions) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (Region, error) {
	if !scope.AllowsRegion(code) {
		return Region{}, access.ErrOutsideAdminScope
	}
	var out Region
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetRegion(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRegionNotFound
		}
		if err != nil {
			return fmt.Errorf("get region: %w", err)
		}
		out = toRegion(row)
		return nil
	})
	return out, err
}

// Create adds a region. Only an ORG_ADMIN may. A code that is taken, even by a deleted
// region, is CODE_TAKEN.
func (r *Regions) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in RegionInput) (Region, error) {
	if !scope.All {
		return Region{}, access.ErrOrgAdminOnly
	}
	in.Code = strings.TrimSpace(in.Code)
	in.Description = strings.TrimSpace(in.Description)
	if !regionCodePattern.MatchString(in.Code) {
		return Region{}, apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: "code", Code: "INVALID", Message: "Harus berformat OS diikuti angka tanpa nol di depan, misalnya OS1.",
		})
	}
	active := in.Active == nil || *in.Active

	var out Region
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.CreateRegion(ctx, db.CreateRegionParams{
			Code: in.Code, Description: in.Description, Active: active, Now: ts(r.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toRegion(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "region.create", EntityType: "region", EntityCode: in.Code,
			Metadata: map[string]any{"after": regionMeta(out)},
		})
	})
	if database.IsUniqueViolation(err) {
		return Region{}, ErrCodeTaken
	}
	if err != nil {
		return Region{}, fmt.Errorf("create region: %w", err)
	}
	return out, nil
}

// Update changes a region's description or active flag. Only an ORG_ADMIN may.
func (r *Regions) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in RegionUpdate) (Region, error) {
	if !scope.All {
		return Region{}, access.ErrOrgAdminOnly
	}
	var desc *string
	if in.Description != nil {
		trimmed := strings.TrimSpace(*in.Description)
		desc = &trimmed
	}
	var out Region
	err := database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetRegionForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRegionNotFound
		}
		if err != nil {
			return fmt.Errorf("get region: %w", err)
		}
		row, err := q.UpdateRegion(ctx, db.UpdateRegionParams{
			Code: code, Description: textPtr(desc), Active: boolOrNull(in.Active), Now: ts(r.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update region: %w", err)
		}
		out = toRegion(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "region.update", EntityType: "region", EntityCode: code,
			Metadata: map[string]any{"before": regionMeta(toRegion(before)), "after": regionMeta(out)},
		})
	})
	return out, err
}

// Delete removes a region (a soft delete: the row and its code stay, D19). Only an ORG_ADMIN
// may.
//
// ⚠ A region that a REGION_ADMIN is scoped to cannot be deleted: that admin would be left
// with a scope that matches nothing. Nor can one with an active kantor cabang below it.
func (r *Regions) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, r.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetRegionForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRegionNotFound
		}
		if err != nil {
			return fmt.Errorf("get region: %w", err)
		}
		admins, err := q.CountWebRolesOfRegion(ctx, pgtype.Text{String: code, Valid: true})
		if err != nil {
			return fmt.Errorf("count admins of region: %w", err)
		}
		if admins > 0 {
			return ErrHasActiveChildren.WithMessage("Masih ada admin wilayah yang menunjuk koordinator wilayah ini.")
		}
		branches, err := q.CountActiveBranchesOfRegion(ctx, code)
		if err != nil {
			return fmt.Errorf("count branches of region: %w", err)
		}
		if branches > 0 {
			return ErrHasActiveChildren.WithMessage("Masih ada kantor cabang aktif di koordinator wilayah ini.")
		}
		if _, err := q.SoftDeleteRegion(ctx, db.SoftDeleteRegionParams{Code: code, Now: ts(r.clock.Now())}); err != nil {
			return fmt.Errorf("delete region: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "region.delete", EntityType: "region", EntityCode: code,
			Metadata: map[string]any{"before": regionMeta(toRegion(before))},
		})
	})
}

// scopeFilter is the REGION_ADMIN's own region as a query filter; NULL (no filter) for an
// ORG_ADMIN, who sees all.
func scopeFilter(scope access.AdminScope) pgtype.Text {
	if scope.All {
		return pgtype.Text{}
	}
	// A REGION_ADMIN with no region cannot exist (CHECK constraint), but if one did, "" would
	// match nothing rather than everything: the filter below is an equality on code.
	return pgtype.Text{String: scope.RegionCode, Valid: true}
}

func toRegion(r db.Region) Region {
	return Region{Code: r.Code, Description: r.Description, Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
}

// regionMeta is what an audit row keeps of a region.
func regionMeta(r Region) map[string]any {
	return map[string]any{"description": r.Description, "active": r.Active}
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func textOrNull(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func textPtr(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
