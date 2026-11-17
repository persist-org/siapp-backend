package patrol

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/access"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/audit"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/database"
)

// CheckpointInput is a request to create a checkpoint. Its checklist may come with it.
type CheckpointInput struct {
	ProjectCode  string        `json:"project_code" validate:"required,notblank,max=100"`
	Name         string        `json:"name" validate:"required,notblank,max=200"`
	Description  *string       `json:"description" validate:"omitempty,max=2000"`
	Latitude     *float64      `json:"latitude" validate:"omitempty,gte=-90,lte=90"`
	Longitude    *float64      `json:"longitude" validate:"omitempty,gte=-180,lte=180"`
	RadiusMeters *int          `json:"radius_meters" validate:"omitempty,min=1,max=1000"`
	Active       *bool         `json:"is_active"`
	Actions      []ActionInput `json:"actions"`
}

// CheckpointUpdate is a request to change a checkpoint. A field that is left out keeps its value;
// null empties the ones that may be empty. project_code is only accepted when it is the current one:
// a checkpoint never moves to another project, because the zones it is in are in its project.
type CheckpointUpdate struct {
	ProjectCode  *string           `json:"project_code"`
	Name         *string           `json:"name" validate:"omitempty,notblank,max=200"`
	Description  optional[string]  `json:"description"`
	Latitude     optional[float64] `json:"latitude"`
	Longitude    optional[float64] `json:"longitude"`
	RadiusMeters optional[int]     `json:"radius_meters"`
	Active       *bool             `json:"is_active"`
}

// CheckpointFilter narrows the admin list.
type CheckpointFilter struct {
	ProjectCode string
	ZoneID      int64 // 0 for any zone
	Active      *bool
	Q           string
	Limit       int
	Offset      int
}

// ListCheckpoints returns one page of the checkpoints the admin's region holds, with their checklists
// and the zones they are in, and the total.
func (s *Service) ListCheckpoints(ctx context.Context, actor auth.Principal, scope access.AdminScope, f CheckpointFilter) ([]Checkpoint, int, error) {
	if f.ProjectCode != "" {
		if err := assertRegion(scope, f.ProjectCode); err != nil {
			return nil, 0, err
		}
	}
	var out []Checkpoint
	var total int64
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		region := pgtype.Text{String: scope.RegionCode, Valid: !scope.All}
		zone := pgtype.Int8{Int64: f.ZoneID, Valid: f.ZoneID != 0}
		rows, err := q.ListPatrolCheckpoints(ctx, db.ListPatrolCheckpointsParams{
			ScopeRegion: region, ProjectCode: text(f.ProjectCode), ZoneID: zone, Active: boolOrNull(f.Active), Q: text(f.Q),
			PageLimit: int32(f.Limit), PageOffset: int32(f.Offset), //nolint:gosec // bounded by ParseListQuery
		})
		if err != nil {
			return fmt.Errorf("list checkpoints: %w", err)
		}
		total, err = q.CountPatrolCheckpoints(ctx, db.CountPatrolCheckpointsParams{
			ScopeRegion: region, ProjectCode: text(f.ProjectCode), ZoneID: zone, Active: boolOrNull(f.Active), Q: text(f.Q),
		})
		if err != nil {
			return fmt.Errorf("count checkpoints: %w", err)
		}
		out, err = withActionsAndZones(ctx, q, rows)
		return err
	})
	return out, int(total), err
}

// GetCheckpoint returns one checkpoint with its checklist and zones.
func (s *Service) GetCheckpoint(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string) (Checkpoint, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	var out Checkpoint
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpoint(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		out, err = withActionsAndZone(ctx, q, row)
		return err
	})
	return out, err
}

// CreateCheckpoint adds a checkpoint, with its checklist when one is sent, and gives it a QR secret.
func (s *Service) CreateCheckpoint(ctx context.Context, actor auth.Principal, scope access.AdminScope, in CheckpointInput) (Checkpoint, error) {
	if err := assertRegion(scope, in.ProjectCode); err != nil {
		return Checkpoint{}, err
	}
	geo, err := geofenceWith(in.Latitude, in.Longitude, in.RadiusMeters, false, checkpointFence)
	if err != nil {
		return Checkpoint{}, err
	}
	specs, err := parseActions(in.Actions)
	if err != nil {
		return Checkpoint{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return Checkpoint{}, err
	}

	var out Checkpoint
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		if err := checkProjectAndTeam(ctx, q, in.ProjectCode, nil); err != nil {
			return err
		}
		row, err := q.InsertPatrolCheckpoint(ctx, db.InsertPatrolCheckpointParams{
			ProjectCode: in.ProjectCode, Name: strings.TrimSpace(in.Name), Description: textOrNull(in.Description),
			Latitude: geo.lat, Longitude: geo.lng, RadiusMeters: intParam(geo.radius), QrSecret: secret,
			Active: in.Active == nil || *in.Active, Now: ts(s.clock.Now()),
		})
		if database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_checkpoints_name_uq" {
			return ErrCheckpointNameTaken
		}
		if err != nil {
			return fmt.Errorf("insert checkpoint: %w", err)
		}
		if _, _, _, err := replaceActions(ctx, q, s.clock, row.ID, specs); err != nil {
			return err
		}
		out, err = withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}
		return auditCheckpoint(ctx, q, actor, "patrol.checkpoint.create", out, map[string]any{"after": checkpointMeta(out)})
	})
	return out, err
}

// UpdateCheckpoint changes a checkpoint. The row is locked for the whole change, so two admins editing
// it take turns.
func (s *Service) UpdateCheckpoint(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string, in CheckpointUpdate) (Checkpoint, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	fields := collect([]*apperr.FieldError{
		in.Description.resolve("description"), in.Latitude.resolve("latitude"), in.Longitude.resolve("longitude"),
		in.RadiusMeters.resolve("radius_meters"),
	})
	if len(fields) > 0 {
		return Checkpoint{}, apperr.ErrValidation.WithFields(fields...)
	}

	var out Checkpoint
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpointForUpdate(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		if in.ProjectCode != nil && *in.ProjectCode != row.ProjectCode {
			return apperr.ErrValidation.WithFields(apperr.FieldError{
				Field: "project_code", Code: "INVALID", Message: "Proyek checkpoint tidak bisa diubah.",
			})
		}
		before, err := withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}

		next := db.UpdatePatrolCheckpointParams{
			ID: uid, Name: row.Name, Description: row.Description, Latitude: row.Latitude, Longitude: row.Longitude,
			RadiusMeters: row.RadiusMeters, Active: row.Active, Now: ts(s.clock.Now()),
		}
		if in.Name != nil {
			next.Name = strings.TrimSpace(*in.Name)
		}
		if in.Description.set {
			next.Description = textOrNull(in.Description.ptr())
		}
		if in.Active != nil {
			next.Active = *in.Active
		}

		// The geofence is the point and the radius together. Start from what the checkpoint has, apply
		// what was sent, and check the result as a whole (the same merge as a zone's).
		lat, lng, radius := row.Latitude, row.Longitude, intOf(row.RadiusMeters)
		if in.Latitude.set {
			lat = in.Latitude.ptr()
		}
		if in.Longitude.set {
			lng = in.Longitude.ptr()
		}
		if in.RadiusMeters.set {
			radius = in.RadiusMeters.ptr()
		} else if lat == nil && lng == nil {
			// Taking the point away takes the radius with it; the admin need not say so twice.
			radius = nil
		}
		geo, err := geofenceWith(lat, lng, radius, in.RadiusMeters.set, checkpointFence)
		if err != nil {
			return err
		}
		next.Latitude, next.Longitude, next.RadiusMeters = geo.lat, geo.lng, intParam(geo.radius)

		row, err = q.UpdatePatrolCheckpoint(ctx, next)
		if database.IsUniqueViolation(err) && database.ConstraintName(err) == "patrol_checkpoints_name_uq" {
			return ErrCheckpointNameTaken
		}
		if err != nil {
			return fmt.Errorf("update checkpoint: %w", err)
		}
		out, err = withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}
		return auditCheckpoint(ctx, q, actor, "patrol.checkpoint.update", out, map[string]any{
			"before": checkpointMeta(before), "after": checkpointMeta(out),
		})
	})
	return out, err
}

// ReplaceActions replaces the checklist and nothing else of the checkpoint.
func (s *Service) ReplaceActions(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string, in ActionsReplace) (Checkpoint, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	specs, err := parseActions(in.Actions)
	if err != nil {
		return Checkpoint{}, err
	}
	var out Checkpoint
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpointForUpdate(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		created, updated, deactivated, err := replaceActions(ctx, q, s.clock, uid, specs)
		if err != nil {
			return err
		}
		// The checklist is part of what the checkpoint looks like to the app, so its change moves
		// updated_at.
		row, err = q.UpdatePatrolCheckpoint(ctx, db.UpdatePatrolCheckpointParams{
			ID: uid, Name: row.Name, Description: row.Description, Latitude: row.Latitude, Longitude: row.Longitude,
			RadiusMeters: row.RadiusMeters, Active: row.Active, Now: ts(s.clock.Now()),
		})
		if err != nil {
			return fmt.Errorf("touch checkpoint: %w", err)
		}
		out, err = withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}
		return auditCheckpoint(ctx, q, actor, "patrol.checkpoint.actions", out, map[string]any{
			"created": created, "updated": updated, "deactivated": deactivated,
		})
	})
	return out, err
}

// DeleteCheckpoint removes a checkpoint: it stays in the table, inactive, because scans point at it.
// It leaves every zone it was in, and those zones' positions are renumbered so they have no gap.
func (s *Service) DeleteCheckpoint(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string) error {
	uid, ok := parseUUID(id)
	if !ok {
		return ErrCheckpointNotFound
	}
	return database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpointForUpdate(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		before, err := withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}

		// ⚠ Lock the zones it leaves, in id order, before touching their links: a zone's order is
		// rewritten as a whole, and two requests that rewrite it (this one and an admin assigning
		// checkpoints to the zone, Phase 10.8) must take turns. A fixed order keeps two deletions
		// that share zones from deadlocking.
		zoneIDs := make([]int64, 0, len(before.Zones))
		for _, z := range before.Zones {
			zoneIDs = append(zoneIDs, z.ID)
		}
		slices.Sort(zoneIDs)
		var locked []int64
		for _, zoneID := range zoneIDs {
			if _, err := q.GetPatrolZoneForUpdate(ctx, zoneID); errors.Is(err, pgx.ErrNoRows) {
				continue // a deleted zone has no order to keep
			} else if err != nil {
				return fmt.Errorf("lock zone: %w", err)
			}
			locked = append(locked, zoneID)
		}

		if _, err := q.DeleteCheckpointLinks(ctx, uid); err != nil {
			return fmt.Errorf("delete checkpoint links: %w", err)
		}
		for _, zoneID := range locked {
			if err := closeGaps(ctx, q, zoneID); err != nil {
				return err
			}
		}
		if _, err := q.SoftDeletePatrolCheckpoint(ctx, db.SoftDeletePatrolCheckpointParams{ID: uid, Now: ts(s.clock.Now())}); err != nil {
			return fmt.Errorf("delete checkpoint: %w", err)
		}
		return auditCheckpoint(ctx, q, actor, "patrol.checkpoint.delete", before, map[string]any{
			"before": checkpointMeta(before), "zones_left": len(zoneIDs),
		})
	})
}

// closeGaps renumbers a zone's checkpoints 1..n in their current order, after one has left.
//
// It rewrites the links instead of updating sort_order in place: UNIQUE (zone, sort_order) is checked
// row by row, and an UPDATE that shifts 3 to 2 and 4 to 3 can hit the old 3 before it moves, in an
// order Postgres does not promise.
func closeGaps(ctx context.Context, q *db.Queries, zoneID int64) error {
	links, err := q.ListZoneLinks(ctx, zoneID)
	if err != nil {
		return fmt.Errorf("list zone links: %w", err)
	}
	if err := q.DeleteZoneCheckpoints(ctx, zoneID); err != nil {
		return fmt.Errorf("clear zone links: %w", err)
	}
	for i, l := range links {
		if err := q.InsertZoneCheckpoint(ctx, db.InsertZoneCheckpointParams{
			ProjectCode: l.ProjectCode, ZoneID: zoneID, CheckpointID: l.CheckpointID, SortOrder: int32(i + 1), //nolint:gosec // a zone has a handful of checkpoints
		}); err != nil {
			return fmt.Errorf("renumber zone links: %w", err)
		}
	}
	return nil
}

// RotateCheckpointQR gives the checkpoint a new QR secret, so every sticker printed with the old one
// stops working.
func (s *Service) RotateCheckpointQR(ctx context.Context, actor auth.Principal, scope access.AdminScope, id string) (Checkpoint, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return Checkpoint{}, ErrCheckpointNotFound
	}
	secret, err := newSecret()
	if err != nil {
		return Checkpoint{}, err
	}
	var out Checkpoint
	err = database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpointForUpdate(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		row, err = q.RotateCheckpointSecret(ctx, db.RotateCheckpointSecretParams{ID: uid, QrSecret: secret, Now: ts(s.clock.Now())})
		if err != nil {
			return fmt.Errorf("rotate checkpoint secret: %w", err)
		}
		out, err = withActionsAndZone(ctx, q, row)
		if err != nil {
			return err
		}
		// The secret itself is never written to the audit log.
		return auditCheckpoint(ctx, q, actor, "patrol.checkpoint.rotate_qr", out, nil)
	})
	return out, err
}

// CheckpointQRImage renders the checkpoint's QR code, in the format (FormatPNG or FormatSVG).
func (s *Service) CheckpointQRImage(ctx context.Context, actor auth.Principal, scope access.AdminScope, id, format string) (QR, error) {
	uid, ok := parseUUID(id)
	if !ok {
		return QR{}, ErrCheckpointNotFound
	}
	var out QR
	err := database.WithTenant(ctx, s.pool, actor.OrganizationID, func(q *db.Queries) error {
		row, err := q.GetPatrolCheckpoint(ctx, uid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCheckpointNotFound
		}
		if err != nil {
			return fmt.Errorf("get checkpoint: %w", err)
		}
		if err := assertRegion(scope, row.ProjectCode); err != nil {
			return err
		}
		body, contentType, err := renderQR(CheckpointPayload(actor.OrganizationID, uuidString(row.ID), row.QrSecret), format)
		if err != nil {
			return err
		}
		out = QR{Body: body, ContentType: contentType, FileName: checkpointFileName(row.Name, format)}
		return nil
	})
	return out, err
}

// withActionsAndZone reads one checkpoint's checklist and zones.
func withActionsAndZone(ctx context.Context, q *db.Queries, row db.PatrolCheckpoint) (Checkpoint, error) {
	list, err := withActionsAndZones(ctx, q, []db.PatrolCheckpoint{row})
	if err != nil {
		return Checkpoint{}, err
	}
	return list[0], nil
}

// withActionsAndZones reads the checklists and the zones of all the checkpoints in two queries.
func withActionsAndZones(ctx context.Context, q *db.Queries, rows []db.PatrolCheckpoint) ([]Checkpoint, error) {
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	actions, err := q.ListActionsOfCheckpoints(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint actions: %w", err)
	}
	zones, err := q.ListZonesOfCheckpoints(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint zones: %w", err)
	}
	actionsOf := make(map[string][]db.PatrolCheckpointAction, len(rows))
	for _, a := range actions {
		key := uuidString(a.CheckpointID)
		actionsOf[key] = append(actionsOf[key], a)
	}
	zonesOf := make(map[string][]db.ListZonesOfCheckpointsRow, len(rows))
	for _, z := range zones {
		key := uuidString(z.CheckpointID)
		zonesOf[key] = append(zonesOf[key], z)
	}
	out := make([]Checkpoint, 0, len(rows))
	for _, r := range rows {
		key := uuidString(r.ID)
		out = append(out, checkpointOf(r, actionsOf[key], zonesOf[key]))
	}
	return out, nil
}

func auditCheckpoint(ctx context.Context, q *db.Queries, actor auth.Principal, action string, c Checkpoint, meta map[string]any) error {
	actorID := actor.PersonnelID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["project_code"] = c.ProjectCode
	// A checkpoint is named by its uuid, so it goes in EntityCode: EntityID is a number.
	return audit.Record(ctx, q, audit.Entry{
		ActorPersonnelID: &actorID, Action: action, EntityType: "patrol_checkpoint", EntityCode: c.ID, Metadata: meta,
	})
}

// checkpointMeta is what the audit log keeps of a checkpoint: its settings and how many actions its
// checklist has, not the actions themselves.
func checkpointMeta(c Checkpoint) map[string]any {
	return map[string]any{
		"name": c.Name, "latitude": c.Latitude, "longitude": c.Longitude, "radius_meters": c.RadiusMeters,
		"active": c.Active, "actions": len(c.Actions), "zones": len(c.Zones),
	}
}
