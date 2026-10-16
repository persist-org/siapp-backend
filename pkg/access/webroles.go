package access

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/pii"
)

// WebRoleInput is a request to grant or change a web role. The json tags carry the
// contract's field names.
type WebRoleInput struct {
	Role       string  `json:"role" validate:"required,oneof=ORG_ADMIN PROJECT_ADMIN"`
	ProjectIDs []int64 `json:"project_ids" validate:"omitempty,max=500,dive,gt=0"`
}

// WebRole is one assignment, with who it belongs to. The NIK is masked.
type WebRole struct {
	PersonnelID          int64
	FullName             string
	NIK                  string
	Role                 string
	ProjectIDs           []int64
	GrantedByPersonnelID *int64
	CreatedAt, UpdatedAt time.Time
}

// WebRoles manages the web role assignments of an organization. Only an ORG_ADMIN may.
type WebRoles struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewWebRoles returns the WebRoles usecase.
func NewWebRoles(pool *pgxpool.Pool, c clock.Clock) *WebRoles {
	return &WebRoles{pool: pool, clock: c}
}

// List returns one page of the organization's assignments and their total.
func (w *WebRoles) List(ctx context.Context, actor auth.Principal, scope AdminScope, limit, offset int) ([]WebRole, int, error) {
	if !scope.All {
		return nil, 0, ErrOrgAdminOnly
	}
	var out []WebRole
	var total int64
	err := database.WithTenant(ctx, w.pool, actor.OrganizationID, func(q *db.Queries) error {
		// limit is at most httpx.MaxLimit, and a page this deep is nonsense: an empty answer.
		if offset > math.MaxInt32 || limit > math.MaxInt32 {
			return nil
		}
		rows, err := q.ListWebRoles(ctx, db.ListWebRolesParams{PageLimit: int32(limit), PageOffset: int32(offset)}) //nolint:gosec // bounded just above
		if err != nil {
			return fmt.Errorf("list web roles: %w", err)
		}
		for _, r := range rows {
			out = append(out, WebRole{
				PersonnelID: r.PersonnelID, FullName: r.FullName, NIK: pii.MaskNIK(r.Nik.String),
				Role: string(r.Role), ProjectIDs: r.ProjectIds, GrantedByPersonnelID: int8Ptr(r.GrantedByPersonnelID),
				CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
			})
		}
		if total, err = q.CountWebRoles(ctx); err != nil {
			return fmt.Errorf("count web roles: %w", err)
		}
		return nil
	})
	return out, int(total), err
}

// Put grants or changes a person's web role.
//
// ⚠ An admin cannot narrow their own assignment, so the last admin cannot lock everyone out:
// the only change an ORG_ADMIN may make to themselves is none at all (ORG_ADMIN again, which
// changes nothing), and a PROJECT_ADMIN cannot reach this at all because only ORG_ADMINs
// manage roles.
func (w *WebRoles) Put(ctx context.Context, actor auth.Principal, scope AdminScope, target int64, in WebRoleInput) (WebRole, error) {
	if !scope.All {
		return WebRole{}, ErrOrgAdminOnly
	}
	projectIDs, err := normalize(in)
	if err != nil {
		return WebRole{}, err
	}
	self := target == actor.PersonnelID
	if self && in.Role != RoleOrgAdmin {
		return WebRole{}, ErrOwnRoleProtected
	}

	var out WebRole
	err = database.WithTenant(ctx, w.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := lockAndRecheckActor(ctx, q, actor.PersonnelID, target); err != nil {
			return err
		}
		who, err := q.PersonnelNameAndNIK(ctx, target)
		if errors.Is(err, pgx.ErrNoRows) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get personnel: %w", err)
		}
		if self {
			// ORG_ADMIN again: nothing to change, and nothing to audit. The row exists: the
			// re-check above just found it.
			current, err := q.GetWebRole(ctx, target)
			if err != nil {
				return fmt.Errorf("get own web role: %w", err)
			}
			out = toWebRole(current, who)
			return nil
		}

		row, err := q.UpsertWebRole(ctx, db.UpsertWebRoleParams{
			PersonnelID:          target,
			Role:                 db.WebRole(in.Role),
			ProjectIds:           projectIDs,
			GrantedByPersonnelID: pgtype.Int8{Int64: actor.PersonnelID, Valid: true},
			Now:                  pgtype.Timestamptz{Time: w.clock.Now(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("upsert web role: %w", err)
		}
		actorID := actor.PersonnelID
		if err := audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "web_role.grant", EntityType: "web_role", EntityID: target,
			Metadata: map[string]any{"role": in.Role, "project_ids": projectIDs},
		}); err != nil {
			return err
		}
		out = toWebRole(row, who)
		return nil
	})
	return out, err
}

// Delete removes a person's web role. They are field staff again: their next admin request is
// refused, they cannot sign in on the web, and their web session ends at its next refresh
// (identity.Usecase.Refresh re-checks the role), so within one access-token lifetime.
func (w *WebRoles) Delete(ctx context.Context, actor auth.Principal, scope AdminScope, target int64) error {
	if !scope.All {
		return ErrOrgAdminOnly
	}
	if target == actor.PersonnelID {
		return ErrOwnRoleProtected
	}
	return database.WithTenant(ctx, w.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := lockAndRecheckActor(ctx, q, actor.PersonnelID, target); err != nil {
			return err
		}
		n, err := q.DeleteWebRole(ctx, target)
		if err != nil {
			return fmt.Errorf("delete web role: %w", err)
		}
		if n == 0 {
			return apperr.ErrNotFound
		}
		actorID := actor.PersonnelID
		return audit.Record(ctx, q, audit.Entry{
			ActorPersonnelID: &actorID, Action: "web_role.revoke", EntityType: "web_role", EntityID: target,
		})
	})
}

// GrantFirstAdmin makes a person ORG_ADMIN with no granting admin. It is the only way to
// create an admin without an existing one, and is used by `cmd/seed grant-admin`, which runs
// as the database owner role inside the person's tenant.
func GrantFirstAdmin(ctx context.Context, pool *pgxpool.Pool, c clock.Clock, orgID, personnelID int64) error {
	return database.WithTenant(ctx, pool, orgID, func(q *db.Queries) error {
		if _, err := q.UpsertWebRole(ctx, db.UpsertWebRoleParams{
			PersonnelID: personnelID, Role: db.WebRoleORGADMIN, ProjectIds: []int64{},
			Now: pgtype.Timestamptz{Time: c.Now(), Valid: true},
		}); err != nil {
			return fmt.Errorf("grant first admin: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			Action: "web_role.grant", EntityType: "web_role", EntityID: personnelID,
			Metadata: map[string]any{"role": RoleOrgAdmin, "source": "cmd/seed"},
		})
	})
}

// lockAndRecheckActor locks the assignments of the actor and the target (see the LockWebRoles
// query) and checks that the actor is still an ORG_ADMIN. RequireAdmin read the role in an
// earlier transaction; another admin may have removed or narrowed it since.
func lockAndRecheckActor(ctx context.Context, q *db.Queries, actorID, targetID int64) error {
	rows, err := q.LockWebRoles(ctx, []int64{actorID, targetID})
	if err != nil {
		return fmt.Errorf("lock web roles: %w", err)
	}
	for _, r := range rows {
		if r.PersonnelID != actorID {
			continue
		}
		if r.Role != db.WebRoleORGADMIN {
			return ErrOrgAdminOnly
		}
		return nil
	}
	return ErrNotAdmin
}

// normalize checks the cross-field rule of the role shapes and returns the project ids
// sorted and without duplicates.
func normalize(in WebRoleInput) ([]int64, error) {
	ids := slices.Clone(in.ProjectIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if ids == nil {
		ids = []int64{}
	}
	switch in.Role {
	case RoleOrgAdmin:
		if len(ids) > 0 {
			return nil, apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "project_ids", Code: "INVALID", Message: "Harus kosong untuk ORG_ADMIN: admin organisasi mencakup semua proyek.",
			})
		}
	case RoleProjectAdmin:
		if len(ids) == 0 {
			return nil, apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "project_ids", Code: "REQUIRED", Message: "Wajib diisi untuk PROJECT_ADMIN.",
			})
		}
	}
	return ids, nil
}

func toWebRole(r db.WebRoleAssignment, who db.PersonnelNameAndNIKRow) WebRole {
	return WebRole{
		PersonnelID: r.PersonnelID, FullName: who.FullName, NIK: pii.MaskNIK(who.Nik.String),
		Role: string(r.Role), ProjectIDs: r.ProjectIds, GrantedByPersonnelID: int8Ptr(r.GrantedByPersonnelID),
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}
}

func int8Ptr(v pgtype.Int8) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
