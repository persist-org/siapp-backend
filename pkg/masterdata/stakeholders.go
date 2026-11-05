package masterdata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// StakeholderType is who an emergency number belongs to: Polisi, Rumah Sakit, Pemadam Kebakaran
// (the manual's "Telp Stakeholder", DL-14).
type StakeholderType struct {
	ID             int64
	OrganizationID int64
	Code           string
	Name           string
	// DefaultPhone is what a project without a contact of this type falls back to. nil: none.
	DefaultPhone *string
	SortOrder    int
	Active       bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// StakeholderTypeInput is a request to create a stakeholder type. The code is made from the name
// when it is left out, and never changes afterwards.
type StakeholderTypeInput struct {
	Code         string  `json:"code" validate:"omitempty,max=30"`
	Name         string  `json:"name" validate:"required,notblank,max=500"`
	DefaultPhone *string `json:"default_phone" validate:"omitempty,max=50"`
	SortOrder    *int    `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active       *bool   `json:"is_active"`
}

// StakeholderTypeUpdate is a request to change one. default_phone: null (or "") clears it. There
// is no field for the code.
type StakeholderTypeUpdate struct {
	Name         *string          `json:"name" validate:"omitempty,notblank,max=500"`
	DefaultPhone Optional[string] `json:"default_phone"`
	SortOrder    *int             `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active       *bool            `json:"is_active"`
}

// defaultPhoneOf cleans a default phone: nothing (nil, "" or spaces) is no default; anything else
// must be a phone number.
func defaultPhoneOf(raw *string, field string) (*string, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil //nolint:nilnil // nil is "no default phone", not a missing value
	}
	phone, ok := normalizePhone(*raw)
	if !ok {
		return nil, validationError(field, "Harus nomor telepon internasional (+62812…), nomor lokal (0812…) atau nomor darurat nasional (110, 113, …).")
	}
	return &phone, nil
}

// ListStakeholderTypes returns one page of the organization's stakeholder types, and their total.
func (s *Service) ListStakeholderTypes(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ListFilter) ([]StakeholderType, int, error) {
	if !scope.All {
		return nil, 0, access.ErrOrgAdminOnly
	}
	var out []StakeholderType
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListStakeholderTypes(ctx, db.ListStakeholderTypesParams{
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list stakeholder types: %w", err)
		}
		for _, r := range rows {
			out = append(out, toStakeholderType(r))
		}
		total, err = q.CountStakeholderTypes(ctx, db.CountStakeholderTypesParams{Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count stakeholder types: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetStakeholderType returns one stakeholder type.
func (s *Service) GetStakeholderType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (StakeholderType, error) {
	if !scope.All {
		return StakeholderType{}, access.ErrOrgAdminOnly
	}
	var out StakeholderType
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetStakeholderType(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStakeholderTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get stakeholder type: %w", err)
		}
		out = toStakeholderType(row)
		return nil
	})
	return out, err
}

// CreateStakeholderType adds a stakeholder type. The name is taken by every type that is not
// deleted, active or not, ignoring case; a code made from the name gets a suffix when it is taken.
func (s *Service) CreateStakeholderType(ctx context.Context, actor auth.Principal, scope access.AdminScope, in StakeholderTypeInput) (StakeholderType, error) {
	if !scope.All {
		return StakeholderType{}, access.ErrOrgAdminOnly
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return StakeholderType{}, err
	}
	code, derived, err := codeFor(in.Code, name)
	if err != nil {
		return StakeholderType{}, err
	}
	phone, err := defaultPhoneOf(in.DefaultPhone, "default_phone")
	if err != nil {
		return StakeholderType{}, err
	}
	active := in.Active == nil || *in.Active

	var out StakeholderType
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := q.LockMasterName(ctx, fmt.Sprintf("masterdata:stakeholder_type:%d:%s", actor.OrganizationID, strings.ToLower(name))); err != nil {
			return fmt.Errorf("lock stakeholder type name: %w", err)
		}
		clash, err := q.StakeholderTypeNameClash(ctx, db.StakeholderTypeNameClashParams{Name: name})
		if err != nil {
			return fmt.Errorf("check stakeholder type name: %w", err)
		}
		if clash {
			return ErrStakeholderTypeNameTaken
		}
		if derived {
			if err := q.LockMasterName(ctx, fmt.Sprintf("masterdata:stakeholder_type_code:%d:%s", actor.OrganizationID, codeRoot(code))); err != nil {
				return fmt.Errorf("lock stakeholder type code: %w", err)
			}
			taken, err := q.ListStakeholderTypeCodesWithPrefix(ctx, codeRoot(code))
			if err != nil {
				return fmt.Errorf("list stakeholder type codes: %w", err)
			}
			code = freeCode(code, taken)
		}
		row, err := q.InsertStakeholderType(ctx, db.InsertStakeholderTypeParams{
			Code: code, Name: name, DefaultPhone: textPtr(phone), SortOrder: intPtr(in.SortOrder),
			Active: active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toStakeholderType(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.stakeholder_type.create", EntityType: "stakeholder_type", EntityID: out.ID,
			Metadata: map[string]any{"after": stakeholderTypeMeta(out)},
		})
	})
	return out, stakeholderTypeWriteError(err, "create stakeholder type")
}

// UpdateStakeholderType changes a stakeholder type. A change that leaves every field as it was
// writes nothing: no updated_at, no audit row.
func (s *Service) UpdateStakeholderType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in StakeholderTypeUpdate) (StakeholderType, error) {
	if !scope.All {
		return StakeholderType{}, access.ErrOrgAdminOnly
	}
	if err := resolveAll(in.DefaultPhone.resolve("default_phone")); err != nil {
		return StakeholderType{}, err
	}
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return StakeholderType{}, err
		}
		name = &cleaned
	}
	var phone *string
	if in.DefaultPhone.Set {
		var err error
		if phone, err = defaultPhoneOf(in.DefaultPhone.Value, "default_phone"); err != nil {
			return StakeholderType{}, err
		}
	}

	var out StakeholderType
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetStakeholderTypeForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStakeholderTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get stakeholder type: %w", err)
		}
		was := toStakeholderType(before)
		next := was
		if name != nil {
			next.Name = *name
		}
		if in.DefaultPhone.Set {
			next.DefaultPhone = phone
		}
		if in.SortOrder != nil {
			next.SortOrder = *in.SortOrder
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		if next.Name == was.Name && samePhone(next.DefaultPhone, was.DefaultPhone) && next.SortOrder == was.SortOrder && next.Active == was.Active {
			out = was
			return nil
		}
		if next.Name != was.Name {
			if err := q.LockMasterName(ctx, fmt.Sprintf("masterdata:stakeholder_type:%d:%s", actor.OrganizationID, strings.ToLower(next.Name))); err != nil {
				return fmt.Errorf("lock stakeholder type name: %w", err)
			}
			clash, err := q.StakeholderTypeNameClash(ctx, db.StakeholderTypeNameClashParams{ID: id, Name: next.Name})
			if err != nil {
				return fmt.Errorf("check stakeholder type name: %w", err)
			}
			if clash {
				return ErrStakeholderTypeNameTaken
			}
		}
		row, err := q.UpdateStakeholderType(ctx, db.UpdateStakeholderTypeParams{
			ID: id, Name: next.Name, DefaultPhone: textPtr(next.DefaultPhone), SortOrder: int32(next.SortOrder), //nolint:gosec // validated 0..10000
			Active: next.Active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toStakeholderType(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.stakeholder_type.update", EntityType: "stakeholder_type", EntityID: id,
			Metadata: map[string]any{"before": stakeholderTypeMeta(was), "after": stakeholderTypeMeta(out)},
		})
	})
	return out, stakeholderTypeWriteError(err, "update stakeholder type")
}

// DeleteStakeholderType deletes a stakeholder type that nothing points at. One that a panic type
// or a project's emergency contact points at is MASTER_IN_USE: deactivate it instead (DL-09).
func (s *Service) DeleteStakeholderType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		// The lock first: a contact or a panic type being added waits for this delete.
		before, err := q.GetStakeholderTypeForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStakeholderTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get stakeholder type: %w", err)
		}
		panics, err := q.CountPanicTypesOfStakeholderType(ctx, pgInt8(id))
		if err != nil {
			return fmt.Errorf("count panic types of stakeholder type: %w", err)
		}
		projects, err := q.CountProjectsWithStakeholderType(ctx, id)
		if err != nil {
			return fmt.Errorf("count projects with stakeholder type: %w", err)
		}
		if panics > 0 || projects > 0 {
			return ErrMasterInUse
		}
		if _, err := q.SoftDeleteStakeholderType(ctx, db.SoftDeleteStakeholderTypeParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete stakeholder type: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.stakeholder_type.delete", EntityType: "stakeholder_type", EntityID: id,
			Metadata: map[string]any{"before": stakeholderTypeMeta(toStakeholderType(before))},
		})
	})
}

func stakeholderTypeWriteError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err):
		if uniqueConstraint(err) == "stakeholder_types_name_uq" {
			return ErrStakeholderTypeNameTaken
		}
		return ErrStakeholderTypeCodeTaken
	case isAppErr(err):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// codeFor is the code a new panic or stakeholder type gets: the one sent (upper-cased, checked), or
// one made from the name (derived is then true, so the caller makes it free).
func codeFor(sent, name string) (code string, derived bool, err error) {
	code = strings.ToUpper(strings.TrimSpace(sent))
	if code == "" {
		if code = codeFromName(name); code == "" {
			return "", false, validationError("code", "Kode tidak bisa dibuat dari nama ini. Isi kode secara manual.")
		}
		return code, true, nil
	}
	if !panicCodePattern.MatchString(code) {
		return "", false, validationError("code", "Harus 2 sampai 30 karakter: huruf besar, angka atau garis bawah, diawali huruf.")
	}
	return code, false, nil
}

func samePhone(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func toStakeholderType(r db.StakeholderType) StakeholderType {
	return StakeholderType{
		ID: r.ID, OrganizationID: r.OrganizationID, Code: r.Code, Name: r.Name, DefaultPhone: textValue(r.DefaultPhone),
		SortOrder: int(r.SortOrder), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

// stakeholderTypeMeta is what an audit row keeps of a stakeholder type. The default phone is only
// said to be there: a number is not an audit matter.
func stakeholderTypeMeta(t StakeholderType) map[string]any {
	return map[string]any{
		"code": t.Code, "name": t.Name, "has_default_phone": t.DefaultPhone != nil,
		"sort_order": t.SortOrder, "active": t.Active,
	}
}
