package masterdata

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// panicCodePattern is the shape of a panic type code. The table has the same CHECK.
var panicCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,29}$`)

// PanicType is a kind of emergency a person can raise (Keamanan, Medis, Kebakaran, Lainnya).
type PanicType struct {
	ID             int64
	OrganizationID int64
	Code           string
	Name           string
	SortOrder      int
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	deletedAt      *time.Time
}

// PanicTypeInput is a request to create a panic type. The code is the type's identity: when it is
// left out it is made from the name ("Banjir" becomes BANJIR), and it never changes afterwards.
type PanicTypeInput struct {
	Code      string `json:"code" validate:"omitempty,max=30"`
	Name      string `json:"name" validate:"required,notblank,max=500"`
	SortOrder *int   `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active    *bool  `json:"is_active"`
}

// PanicTypeUpdate is a request to change a panic type. There is no field for the code: a client
// that sends one gets 422 from the decoder's unknown-field rule.
type PanicTypeUpdate struct {
	Name      *string `json:"name" validate:"omitempty,notblank,max=500"`
	SortOrder *int    `json:"sort_order" validate:"omitempty,min=0,max=10000"`
	Active    *bool   `json:"is_active"`
}

// ListPanicTypes returns one page of the organization's panic types, and their total.
func (s *Service) ListPanicTypes(ctx context.Context, actor auth.Principal, scope access.AdminScope, f ListFilter) ([]PanicType, int, error) {
	if !scope.All {
		return nil, 0, access.ErrOrgAdminOnly
	}
	var out []PanicType
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListPanicTypes(ctx, db.ListPanicTypesParams{
			Active: boolOrNull(f.Active), Q: textOrNull(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list panic types: %w", err)
		}
		for _, r := range rows {
			out = append(out, toPanicType(r))
		}
		total, err = q.CountPanicTypes(ctx, db.CountPanicTypesParams{Active: boolOrNull(f.Active), Q: textOrNull(f.Q)})
		if err != nil {
			return fmt.Errorf("count panic types: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// GetPanicType returns one panic type.
func (s *Service) GetPanicType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) (PanicType, error) {
	if !scope.All {
		return PanicType{}, access.ErrOrgAdminOnly
	}
	var out PanicType
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPanicType(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPanicTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get panic type: %w", err)
		}
		out = toPanicType(row)
		return nil
	})
	return out, err
}

// CreatePanicType adds a panic type. Its name is taken by every type that is not deleted, active
// or not, whatever its code, ignoring case (PANIC_TYPE_NAME_TAKEN). A code that is sent is checked
// as it is (PANIC_TYPE_CODE_TAKEN, even against deleted types); a code made from the name gets a
// suffix when it is taken ("Kebakaran Hutan" -> KEBAKARAN_HUTAN, then KEBAKARAN_HUTAN_2).
func (s *Service) CreatePanicType(ctx context.Context, actor auth.Principal, scope access.AdminScope, in PanicTypeInput) (PanicType, error) {
	if !scope.All {
		return PanicType{}, access.ErrOrgAdminOnly
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return PanicType{}, err
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	derived := code == ""
	if derived {
		if code = codeFromName(name); code == "" {
			return PanicType{}, validationError("code", "Kode tidak bisa dibuat dari nama ini. Isi kode secara manual.")
		}
	} else if !panicCodePattern.MatchString(code) {
		return PanicType{}, validationError("code", "Harus 2 sampai 30 karakter: huruf besar, angka atau garis bawah, diawali huruf.")
	}
	active := in.Active == nil || *in.Active

	var out PanicType
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := claimPanicTypeName(ctx, q, actor.OrganizationID, 0, name); err != nil {
			return err
		}
		if derived {
			taken, err := q.ListPanicTypeCodesWithPrefix(ctx, code)
			if err != nil {
				return fmt.Errorf("list panic type codes: %w", err)
			}
			code = freeCode(code, taken)
		}
		row, err := q.InsertPanicType(ctx, db.InsertPanicTypeParams{
			Code: code, Name: name, SortOrder: intPtr(in.SortOrder), Active: active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toPanicType(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.panic_type.create", EntityType: "panic_type", EntityID: out.ID,
			Metadata: map[string]any{"after": panicTypeMeta(out)},
		})
	})
	return out, panicTypeWriteError(err, "create panic type")
}

// UpdatePanicType changes a panic type. A change that leaves every field as it was writes nothing:
// no updated_at, no audit row.
func (s *Service) UpdatePanicType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64, in PanicTypeUpdate) (PanicType, error) {
	if !scope.All {
		return PanicType{}, access.ErrOrgAdminOnly
	}
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return PanicType{}, err
		}
		name = &cleaned
	}
	var out PanicType
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetPanicTypeForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPanicTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get panic type: %w", err)
		}
		was := toPanicType(before)
		next := was
		if name != nil {
			next.Name = *name
		}
		if in.SortOrder != nil {
			next.SortOrder = *in.SortOrder
		}
		if in.Active != nil {
			next.Active = *in.Active
		}
		if next.Name == was.Name && next.SortOrder == was.SortOrder && next.Active == was.Active {
			out = was
			return nil
		}
		if next.Name != was.Name {
			if err := claimPanicTypeName(ctx, q, actor.OrganizationID, id, next.Name); err != nil {
				return err
			}
		}
		row, err := q.UpdatePanicType(ctx, db.UpdatePanicTypeParams{
			ID: id, Name: textPtr(name), SortOrder: intPtr(in.SortOrder), Active: boolOrNull(in.Active), Now: ts(s.clock.Now()),
		})
		if err != nil {
			return err
		}
		out = toPanicType(row)
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.panic_type.update", EntityType: "panic_type", EntityID: id,
			Metadata: map[string]any{"before": panicTypeMeta(was), "after": panicTypeMeta(out)},
		})
	})
	return out, panicTypeWriteError(err, "update panic type")
}

// claimPanicTypeName takes the lock on a name and fails with PANIC_TYPE_NAME_TAKEN when another
// type (not id) that is not deleted has it.
func claimPanicTypeName(ctx context.Context, q *db.Queries, orgID, id int64, name string) error {
	if err := q.LockMasterName(ctx, fmt.Sprintf("masterdata:panic_type:%d:%s", orgID, strings.ToLower(name))); err != nil {
		return fmt.Errorf("lock panic type name: %w", err)
	}
	clash, err := q.PanicTypeNameClash(ctx, db.PanicTypeNameClashParams{ID: id, Name: name})
	if err != nil {
		return fmt.Errorf("check panic type name: %w", err)
	}
	if clash {
		return ErrPanicTypeNameTaken
	}
	return nil
}

// panicTypeWriteError tells the two unique indexes apart (a code race, or the name backstop) and
// wraps database failures.
func panicTypeWriteError(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case database.IsUniqueViolation(err):
		if uniqueConstraint(err) == "panic_types_name_uq" {
			return ErrPanicTypeNameTaken
		}
		return ErrPanicTypeCodeTaken
	case isAppErr(err):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}

// freeCode is code itself when no type has it, else code_2, code_3, ... cut so it stays within
// 30 characters. taken holds the codes that start with code.
func freeCode(code string, taken []string) string {
	used := make(map[string]bool, len(taken))
	for _, c := range taken {
		used[c] = true
	}
	if !used[code] {
		return code
	}
	for n := 2; ; n++ {
		suffix := "_" + strconv.Itoa(n)
		base := code
		if len(base)+len(suffix) > 30 {
			base = strings.TrimRight(base[:30-len(suffix)], "_")
		}
		if candidate := base + suffix; !used[candidate] {
			return candidate
		}
	}
}

// DeletePanicType deletes a panic type that nothing points at; one that a panic was raised under
// is MASTER_IN_USE (deactivate it instead, DL-09). The row stays, as a tombstone for the apps.
func (s *Service) DeletePanicType(ctx context.Context, actor auth.Principal, scope access.AdminScope, id int64) error {
	if !scope.All {
		return access.ErrOrgAdminOnly
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		before, err := q.GetPanicTypeForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrPanicTypeNotFound
		}
		if err != nil {
			return fmt.Errorf("get panic type: %w", err)
		}
		for _, inUse := range s.panicTypeUsage {
			used, err := inUse(ctx, q, id)
			if err != nil {
				return fmt.Errorf("check panic type usage: %w", err)
			}
			if used {
				return ErrMasterInUse
			}
		}
		if _, err := q.SoftDeletePanicType(ctx, db.SoftDeletePanicTypeParams{ID: id, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete panic type: %w", err)
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "masterdata.panic_type.delete", EntityType: "panic_type", EntityID: id,
			Metadata: map[string]any{"before": panicTypeMeta(toPanicType(before))},
		})
	})
}

// FieldPanicTypes is the full pull of the field app: the active panic types, and the server time
// the app starts its delta pulls from, taken before the read.
func (s *Service) FieldPanicTypes(ctx context.Context, p auth.Principal) ([]PanicType, time.Time, error) {
	serverTime := s.clock.Now()
	var out []PanicType
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListActivePanicTypes(ctx)
		if err != nil {
			return fmt.Errorf("list active panic types: %w", err)
		}
		for _, r := range rows {
			out = append(out, toPanicType(r))
		}
		return nil
	})
	return out, serverTime, err
}

// FieldPanicTypeChanges is the delta pull. ⚠ DL-57: a deleted or deactivated type is sent in
// Deleted, so the offline cache can drop it (the full pull returns active rows only).
func (s *Service) FieldPanicTypeChanges(ctx context.Context, p auth.Principal, since time.Time) (Changes[PanicType], error) {
	out := Changes[PanicType]{ServerTime: s.clock.Now()}
	err := database.WithTenant(ctx, s.pool, p.OrganizationID, func(q *db.Queries) error {
		rows, err := q.ListPanicTypeChanges(ctx, ts(sinceBound(since)))
		if err != nil {
			return fmt.Errorf("list panic type changes: %w", err)
		}
		for _, r := range rows {
			t := toPanicType(r)
			if !r.Active || r.DeletedAt.Valid {
				gone := t.UpdatedAt
				if t.deletedAt != nil {
					gone = *t.deletedAt
				}
				out.Deleted = append(out.Deleted, Tombstone{ID: t.ID, DeletedAt: gone})
				continue
			}
			out.Upserted = append(out.Upserted, t)
		}
		return nil
	})
	return out, err
}

// codeFromName makes a code from a name: upper case, every run of characters other than a latin
// letter or a digit as one underscore. "" when nothing is left.
func codeFromName(name string) string {
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			underscore = false
			continue
		}
		if !underscore && b.Len() > 0 {
			b.WriteByte('_')
			underscore = true
		}
	}
	code := strings.TrimRight(b.String(), "_")
	if code == "" {
		return "" // nothing latin in the name: the admin has to give a code
	}
	// A code starts with a letter and has two characters at least ("112" -> P_112, "A" -> P_A).
	if code[0] < 'A' || code[0] > 'Z' || len(code) < 2 {
		code = "P_" + code
	}
	if len(code) > 30 {
		code = strings.TrimRight(code[:30], "_")
	}
	return code
}

func toPanicType(r db.PanicType) PanicType {
	t := PanicType{
		ID: r.ID, OrganizationID: r.OrganizationID, Code: r.Code, Name: r.Name,
		SortOrder: int(r.SortOrder), Active: r.Active, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
	if r.DeletedAt.Valid {
		d := r.DeletedAt.Time
		t.deletedAt = &d
	}
	return t
}

// panicTypeMeta is what an audit row keeps of a panic type.
func panicTypeMeta(t PanicType) map[string]any {
	return map[string]any{"code": t.Code, "name": t.Name, "sort_order": t.SortOrder, "active": t.Active}
}
