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
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
)

// Client is one klien. Its description is its name.
type Client struct {
	Code               string
	BranchCode         string
	Description        string
	CompanyCode        *string
	CompanyPrefix      string
	CompanyBidangUsaha *string
	DirectorName       *string
	City               *string
	Phone              *string
	Fax                *string
	Address            *string
	ZipCode            *string
	Email              *string
	Active             bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ClientInput is a request to create a client. The code is built from BranchCode and
// CompanyPrefix (D19), so the client never sends one.
type ClientInput struct {
	BranchCode         string  `json:"branch_code" validate:"required"`
	CompanyPrefix      string  `json:"company_prefix" validate:"required"`
	Description        string  `json:"description" validate:"required,notblank,max=200"`
	CompanyCode        *string `json:"company_code" validate:"omitempty,max=50"`
	CompanyBidangUsaha *string `json:"company_bidang_usaha" validate:"omitempty,max=200"`
	DirectorName       *string `json:"director_name" validate:"omitempty,max=200"`
	City               *string `json:"city" validate:"omitempty,max=100"`
	Phone              *string `json:"phone" validate:"omitempty,max=50"`
	Fax                *string `json:"fax" validate:"omitempty,max=50"`
	Address            *string `json:"address" validate:"omitempty,max=500"`
	ZipCode            *string `json:"zip_code" validate:"omitempty,max=20"`
	Email              *string `json:"email" validate:"omitempty,max=200"`
	Active             *bool   `json:"is_active"`
}

// ClientUpdate is a request to change a client. The code, the branch and the prefix have no
// field: the code is built from them, so a client that sends one gets 422 from the decoder's
// unknown-field rule.
type ClientUpdate struct {
	Description        *string `json:"description" validate:"omitempty,notblank,max=200"`
	CompanyCode        *string `json:"company_code" validate:"omitempty,max=50"`
	CompanyBidangUsaha *string `json:"company_bidang_usaha" validate:"omitempty,max=200"`
	DirectorName       *string `json:"director_name" validate:"omitempty,max=200"`
	City               *string `json:"city" validate:"omitempty,max=100"`
	Phone              *string `json:"phone" validate:"omitempty,max=50"`
	Fax                *string `json:"fax" validate:"omitempty,max=50"`
	Address            *string `json:"address" validate:"omitempty,max=500"`
	ZipCode            *string `json:"zip_code" validate:"omitempty,max=20"`
	Email              *string `json:"email" validate:"omitempty,max=200"`
	Active             *bool   `json:"is_active"`
}

// ClientFilter narrows a list. Zero values match everything.
type ClientFilter struct {
	Q          string
	BranchCode string
	Active     *bool
	Limit      int
	Offset     int
}

// Clients manages the klien of an organization.
type Clients struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewClients returns the Clients usecase.
func NewClients(pool *pgxpool.Pool, c clock.Clock) *Clients {
	return &Clients{pool: pool, clock: c}
}

// List returns one page of the clients the caller may see, and their total. A REGION_ADMIN
// sees only the clients under their own region.
func (c *Clients) List(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ClientFilter) ([]Client, int, error) {
	var out []Client
	var total int64
	err := database.WithTenant(ctx, c.pool, actor.OrganizationID, func(q *db.Queries) error {
		if f.Offset > math.MaxInt32 || f.Limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListClients(ctx, db.ListClientsParams{
			ScopeRegion: scopeFilter(scope), BranchCode: textOrNull(f.BranchCode), Active: boolOrNull(f.Active),
			Q: textOrNull(f.Q), PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded just above
		})
		if err != nil {
			return fmt.Errorf("list clients: %w", err)
		}
		for _, row := range rows {
			out = append(out, toClient(row))
		}
		total, err = q.CountClients(ctx, db.CountClientsParams{
			ScopeRegion: scopeFilter(scope), BranchCode: textOrNull(f.BranchCode), Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count clients: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Get returns one client. A REGION_ADMIN may read only one under their own region.
func (c *Clients) Get(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) (Client, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Client{}, access.ErrOutsideAdminScope
	}
	var out Client
	err := database.WithTenant(ctx, c.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetClient(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClientNotFound
		}
		if err != nil {
			return fmt.Errorf("get client: %w", err)
		}
		out = toClient(row)
		return nil
	})
	return out, err
}

// Create adds a client under a branch. A REGION_ADMIN may create only under their own
// region. A code that is taken, even by a deleted client, is CODE_TAKEN.
func (c *Clients) Create(ctx context.Context, actor auth.Principal, scope access.AdminScope, in ClientInput) (Client, error) {
	in.BranchCode = strings.TrimSpace(in.BranchCode)
	in.CompanyPrefix = strings.TrimSpace(in.CompanyPrefix)
	code, err := BuildClientCode(in.BranchCode, in.CompanyPrefix)
	if err != nil {
		return Client{}, err
	}
	if !scope.AllowsRegion(RegionOfCode(in.BranchCode)) {
		return Client{}, access.ErrOutsideAdminScope
	}
	active := in.Active == nil || *in.Active

	var out Client
	err = database.WithTenant(ctx, c.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The branch is locked, so a delete of it cannot commit between this check and the insert.
		if _, err := q.GetBranchForUpdate(ctx, in.BranchCode); errors.Is(err, pgx.ErrNoRows) {
			return ErrBranchNotFound
		} else if err != nil {
			return fmt.Errorf("get branch: %w", err)
		}
		row, err := q.CreateClient(ctx, db.CreateClientParams{
			Code: code, BranchCode: in.BranchCode, Description: strings.TrimSpace(in.Description),
			CompanyCode: textPtr(in.CompanyCode), CompanyPrefix: in.CompanyPrefix, CompanyBidangUsaha: textPtr(in.CompanyBidangUsaha),
			DirectorName: textPtr(in.DirectorName), City: textPtr(in.City), Phone: textPtr(in.Phone), Fax: textPtr(in.Fax),
			Address: textPtr(in.Address), ZipCode: textPtr(in.ZipCode), Email: textPtr(in.Email), Active: active,
			Actor: personnelRef(actor), Now: ts(c.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toClient(row)
		return recordAudit(ctx, q, actor, "client.create", "client", code, nil, clientMeta(out))
	})
	if database.IsUniqueViolation(err) {
		return Client{}, ErrCodeTaken
	}
	if err != nil {
		return Client{}, fmt.Errorf("create client: %w", err)
	}
	return out, nil
}

// Update changes a client's details. A REGION_ADMIN may change only one under their region.
func (c *Clients) Update(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string, in ClientUpdate) (Client, error) {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return Client{}, access.ErrOutsideAdminScope
	}
	var out Client
	err := database.WithTenant(ctx, c.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetClientForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClientNotFound
		}
		if err != nil {
			return fmt.Errorf("get client: %w", err)
		}
		row, err := q.UpdateClient(ctx, db.UpdateClientParams{
			Code: code, Description: textPtr(trimmed(in.Description)), CompanyCode: textPtr(in.CompanyCode),
			CompanyBidangUsaha: textPtr(in.CompanyBidangUsaha), DirectorName: textPtr(in.DirectorName), City: textPtr(in.City),
			Phone: textPtr(in.Phone), Fax: textPtr(in.Fax), Address: textPtr(in.Address), ZipCode: textPtr(in.ZipCode),
			Email: textPtr(in.Email), Active: boolOrNull(in.Active), Actor: personnelRef(actor), Now: ts(c.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("update client: %w", err)
		}
		out = toClient(row)
		return recordAudit(ctx, q, actor, "client.update", "client", code, clientMeta(toClient(before)), clientMeta(out))
	})
	return out, err
}

// Delete removes a client (a soft delete: the row and its code stay, D19). A REGION_ADMIN may
// delete only one under their region. A client with an active project is HAS_ACTIVE_CHILDREN.
func (c *Clients) Delete(ctx context.Context, actor auth.Principal, scope access.AdminScope, code string) error {
	if !scope.AllowsRegion(RegionOfCode(code)) {
		return access.ErrOutsideAdminScope
	}
	return database.WithTenant(ctx, c.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetClientForUpdate(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClientNotFound
		}
		if err != nil {
			return fmt.Errorf("get client: %w", err)
		}
		projects, err := q.CountActiveProjectsOfClient(ctx, code)
		if err != nil {
			return fmt.Errorf("count projects of client: %w", err)
		}
		if projects > 0 {
			return ErrHasActiveChildren.WithMessage("Masih ada proyek aktif di klien ini.")
		}
		if _, err := q.SoftDeleteClient(ctx, db.SoftDeleteClientParams{Code: code, Actor: personnelRef(actor), Now: ts(c.clock.Now())}); err != nil {
			return fmt.Errorf("delete client: %w", err)
		}
		return recordAudit(ctx, q, actor, "client.delete", "client", code, clientMeta(toClient(before)), nil)
	})
}

func toClient(r db.Client) Client {
	return Client{
		Code: r.Code, BranchCode: r.BranchCode, Description: r.Description, CompanyCode: textValue(r.CompanyCode),
		CompanyPrefix: r.CompanyPrefix, CompanyBidangUsaha: textValue(r.CompanyBidangUsaha),
		DirectorName: textValue(r.DirectorName), City: textValue(r.City), Phone: textValue(r.Phone), Fax: textValue(r.Fax),
		Address: textValue(r.Address), ZipCode: textValue(r.ZipCode), Email: textValue(r.Email),
		Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// clientMeta is what an audit row keeps of a client.
func clientMeta(c Client) map[string]any {
	return map[string]any{
		"branch_code": c.BranchCode, "company_prefix": c.CompanyPrefix, "description": c.Description,
		"company_code": c.CompanyCode, "company_bidang_usaha": c.CompanyBidangUsaha, "director_name": c.DirectorName,
		"city": c.City, "phone": c.Phone, "fax": c.Fax, "address": c.Address, "zip_code": c.ZipCode,
		"email": c.Email, "active": c.Active,
	}
}
