-- Patrol and guard zones (Phase 10.1). Everything runs inside database.WithTenant.

-- name: InsertPatrolZone :one
INSERT INTO patrol_zones (project_code, team_code, type, name, alias, description, popup_text,
                          latitude, longitude, radius_meters, min_visit, start_date, photo_required,
                          qr_secret, active, created_at, updated_at)
VALUES (@project_code, sqlc.narg('team_code'), @type, @name, sqlc.narg('alias'), sqlc.narg('description'),
        sqlc.narg('popup_text'), sqlc.narg('latitude'), sqlc.narg('longitude'), sqlc.narg('radius_meters'),
        @min_visit, sqlc.narg('start_date'), @photo_required, @qr_secret, @active, @now, @now)
RETURNING *;

-- name: GetPatrolZone :one
SELECT * FROM patrol_zones WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPatrolZoneForUpdate :one
SELECT * FROM patrol_zones WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- The admin list. scope_region is the REGION_ADMIN's region (NULL for an ORG_ADMIN): a project
-- code starts with its region's number, as in ListProjects.
-- name: ListPatrolZones :many
SELECT * FROM patrol_zones
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('team_code')::text IS NULL OR team_code = sqlc.narg('team_code'))
  AND (sqlc.narg('type')::patrol_zone_type IS NULL OR type = sqlc.narg('type'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(coalesce(alias, '')), lower(sqlc.narg('q'))) > 0)
ORDER BY project_code, type, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPatrolZones :one
SELECT count(*) FROM patrol_zones
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('team_code')::text IS NULL OR team_code = sqlc.narg('team_code'))
  AND (sqlc.narg('type')::patrol_zone_type IS NULL OR type = sqlc.narg('type'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(name), lower(sqlc.narg('q'))) > 0
       OR strpos(lower(coalesce(alias, '')), lower(sqlc.narg('q'))) > 0);

-- The field list: the active zones of the projects the caller serves in, that are for the whole
-- project or for one of the caller's own regu. team_code narrows it to one regu's zones and the
-- whole-project ones.
-- name: ListPatrolZonesForField :many
SELECT * FROM patrol_zones
WHERE deleted_at IS NULL AND active
  AND project_code = ANY(@project_codes::text[])
  AND (team_code IS NULL OR team_code = ANY(@own_team_codes::text[]))
  AND (sqlc.narg('team_code')::text IS NULL OR team_code IS NULL OR team_code = sqlc.narg('team_code'))
  AND (sqlc.narg('type')::patrol_zone_type IS NULL OR type = sqlc.narg('type'))
ORDER BY project_code, type, name, id;

-- Every column an admin can change, written together: the usecase has already merged the
-- patch into the row it locked, so a field that was not sent keeps its value.
-- name: UpdatePatrolZone :one
UPDATE patrol_zones SET
    team_code = sqlc.narg('team_code'), type = @type, name = @name, alias = sqlc.narg('alias'),
    description = sqlc.narg('description'), popup_text = sqlc.narg('popup_text'),
    latitude = sqlc.narg('latitude'), longitude = sqlc.narg('longitude'), radius_meters = sqlc.narg('radius_meters'),
    min_visit = @min_visit, start_date = sqlc.narg('start_date'), photo_required = @photo_required,
    active = @active, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: RotatePatrolZoneSecret :one
UPDATE patrol_zones SET qr_secret = @qr_secret, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePatrolZone :execrows
UPDATE patrol_zones SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- name: TouchPatrolZone :exec
UPDATE patrol_zones SET updated_at = @now WHERE id = @id;

-- name: ListChecklistItems :many
SELECT * FROM patrol_checklist_items WHERE zone_id = $1 AND active ORDER BY sort_order, id;

-- name: ListChecklistItemsOfZones :many
SELECT * FROM patrol_checklist_items WHERE zone_id = ANY(@zone_ids::bigint[]) AND active
ORDER BY zone_id, sort_order, id;

-- name: InsertChecklistItem :one
INSERT INTO patrol_checklist_items (zone_id, sort_order, label, help_text, answer_type, options, required,
                                    photo_mode, photo_max, note_mode, attach_when, created_at, updated_at)
VALUES (@zone_id, @sort_order, @label, sqlc.narg('help_text'), @answer_type, sqlc.narg('options'), @required,
        @photo_mode, @photo_max, @note_mode, @attach_when, @now, @now)
RETURNING *;

-- name: UpdateChecklistItem :one
UPDATE patrol_checklist_items SET
    sort_order = @sort_order, label = @label, help_text = sqlc.narg('help_text'), answer_type = @answer_type,
    options = sqlc.narg('options'), required = @required, photo_mode = @photo_mode, photo_max = @photo_max,
    note_mode = @note_mode, attach_when = @attach_when, updated_at = @now
WHERE id = @id AND zone_id = @zone_id AND active
RETURNING *;

-- Deactivates what the admin left out of the checklist they sent. Returns the ids, for the audit.
-- name: DeactivateChecklistItemsExcept :many
UPDATE patrol_checklist_items SET active = false, deactivated_at = @now, updated_at = @now
WHERE zone_id = @zone_id AND active AND NOT (id = ANY(@keep_ids::bigint[]))
RETURNING id;
