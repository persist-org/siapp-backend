-- Patrol checkpoints, their zone links, their checklist and the answers to it (Phase 10.6).
-- Everything runs inside database.WithTenant.

-- Checkpoints
-- ===========

-- name: InsertPatrolCheckpoint :one
INSERT INTO patrol_checkpoints (project_code, name, description, latitude, longitude, radius_meters,
                                qr_secret, qr_rotated_at, active, created_at, updated_at)
VALUES (@project_code, @name, sqlc.narg('description'), sqlc.narg('latitude'), sqlc.narg('longitude'),
        sqlc.narg('radius_meters'), @qr_secret, @now, @active, @now, @now)
RETURNING *;

-- name: GetPatrolCheckpoint :one
SELECT * FROM patrol_checkpoints WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPatrolCheckpointForUpdate :one
SELECT * FROM patrol_checkpoints WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- The admin list. scope_region is the REGION_ADMIN's region (NULL for an ORG_ADMIN): a project code
-- starts with its region's number, as in ListPatrolZones. zone_id keeps only the checkpoints of that zone.
-- name: ListPatrolCheckpoints :many
SELECT c.* FROM patrol_checkpoints c
WHERE c.deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(c.project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR c.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('zone_id')::bigint IS NULL OR EXISTS (
        SELECT 1 FROM patrol_zone_checkpoints zc WHERE zc.zone_id = sqlc.narg('zone_id') AND zc.checkpoint_id = c.id))
  AND (sqlc.narg('active')::boolean IS NULL OR c.active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(c.name), lower(sqlc.narg('q'))) > 0)
ORDER BY c.project_code, lower(c.name), c.id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPatrolCheckpoints :one
SELECT count(*) FROM patrol_checkpoints c
WHERE c.deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(c.project_code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR c.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('zone_id')::bigint IS NULL OR EXISTS (
        SELECT 1 FROM patrol_zone_checkpoints zc WHERE zc.zone_id = sqlc.narg('zone_id') AND zc.checkpoint_id = c.id))
  AND (sqlc.narg('active')::boolean IS NULL OR c.active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(c.name), lower(sqlc.narg('q'))) > 0);

-- Every column an admin can change (never the project), written together: the usecase has already
-- merged the patch into the row it locked, so a field that was not sent keeps its value.
-- name: UpdatePatrolCheckpoint :one
UPDATE patrol_checkpoints SET
    name = @name, description = sqlc.narg('description'), latitude = sqlc.narg('latitude'),
    longitude = sqlc.narg('longitude'), radius_meters = sqlc.narg('radius_meters'),
    active = @active, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- A new secret invalidates the sticker printed before.
-- name: RotateCheckpointSecret :one
UPDATE patrol_checkpoints SET qr_secret = @qr_secret, qr_rotated_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- A checkpoint is never deleted: scans point at it. It becomes inactive and loses its name.
-- name: SoftDeletePatrolCheckpoint :one
UPDATE patrol_checkpoints SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- Zone links
-- ==========

-- The active checkpoints of a zone, in order.
-- name: ListZoneCheckpoints :many
SELECT c.*, zc.sort_order AS position
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
WHERE zc.zone_id = $1 AND c.active AND c.deleted_at IS NULL
ORDER BY zc.sort_order;

-- The zones a checkpoint is in, with its position in each.
-- name: ListCheckpointZones :many
SELECT z.*, zc.sort_order AS position
FROM patrol_zone_checkpoints zc
JOIN patrol_zones z ON z.organization_id = zc.organization_id AND z.id = zc.zone_id
WHERE zc.checkpoint_id = $1 AND z.deleted_at IS NULL
ORDER BY z.project_code, z.name, z.id;

-- The zones of many checkpoints at once (a page of the admin list), with each position.
-- name: ListZonesOfCheckpoints :many
SELECT zc.checkpoint_id, z.id AS zone_id, z.name AS zone_name, z.type AS zone_type, zc.sort_order AS position
FROM patrol_zone_checkpoints zc
JOIN patrol_zones z ON z.organization_id = zc.organization_id AND z.id = zc.zone_id
WHERE zc.checkpoint_id = ANY(@checkpoint_ids::uuid[]) AND z.deleted_at IS NULL
ORDER BY zc.checkpoint_id, z.name, z.id;

-- Every link of a zone in order, the inactive checkpoints too: they keep their position. Used to
-- close the gap when a checkpoint leaves the zone.
-- name: ListZoneLinks :many
SELECT project_code, checkpoint_id, sort_order FROM patrol_zone_checkpoints WHERE zone_id = $1 ORDER BY sort_order;

-- name: DeleteZoneCheckpoints :exec
DELETE FROM patrol_zone_checkpoints WHERE zone_id = $1;

-- name: DeleteCheckpointLinks :many
DELETE FROM patrol_zone_checkpoints WHERE checkpoint_id = $1
RETURNING zone_id;

-- The project is the zone's, written on the link so the foreign keys can check it.
-- name: InsertZoneCheckpoint :exec
INSERT INTO patrol_zone_checkpoints (project_code, zone_id, checkpoint_id, sort_order)
VALUES (@project_code, @zone_id, @checkpoint_id, @sort_order);

-- Checklist (actions)
-- ===================

-- The active actions of a checkpoint, in order.
-- name: ListCheckpointActions :many
SELECT * FROM patrol_checkpoint_actions WHERE checkpoint_id = $1 AND active ORDER BY sort_order, id;

-- The active actions of many checkpoints at once (a zone's whole tree in one query).
-- name: ListActionsOfCheckpoints :many
SELECT * FROM patrol_checkpoint_actions WHERE checkpoint_id = ANY(@checkpoint_ids::uuid[]) AND active
ORDER BY checkpoint_id, sort_order, id;

-- Every action of a checkpoint, the deactivated ones too: an offline scan is judged against the
-- checklist as it was when the guard scanned.
-- name: ListAllCheckpointActions :many
SELECT * FROM patrol_checkpoint_actions WHERE checkpoint_id = $1 ORDER BY sort_order, id;

-- name: InsertCheckpointAction :one
INSERT INTO patrol_checkpoint_actions (checkpoint_id, sort_order, action, instruction, min_photos, max_photos,
                                       is_note_required, created_at, updated_at)
VALUES (@checkpoint_id, @sort_order, @action, sqlc.narg('instruction'), @min_photos, @max_photos,
        @is_note_required, @now, @now)
RETURNING *;

-- name: UpdateCheckpointAction :one
UPDATE patrol_checkpoint_actions SET
    sort_order = @sort_order, action = @action, instruction = sqlc.narg('instruction'),
    min_photos = @min_photos, max_photos = @max_photos, is_note_required = @is_note_required, updated_at = @now
WHERE id = @id AND checkpoint_id = @checkpoint_id AND active
RETURNING *;

-- Deactivates what the admin left out of the checklist they sent. Returns the ids, for the audit.
-- name: DeactivateCheckpointActionsExcept :many
UPDATE patrol_checkpoint_actions SET active = false, deactivated_at = @now, updated_at = @now
WHERE checkpoint_id = @checkpoint_id AND active AND NOT (id = ANY(@keep_ids::uuid[]))
RETURNING id;

-- Answers (the detail of a scan)
-- ==============================

-- name: InsertScanAction :one
INSERT INTO patrol_scan_actions (scan_id, checkpoint_id, action_id, personnel_id, note, media_ids, snapshot, created_at)
VALUES (@scan_id, @checkpoint_id, @action_id, @personnel_id, sqlc.narg('note'), @media_ids, @snapshot, @now)
RETURNING *;

-- The answers of a page of scans in one query, in the order of the checkpoint's checklist.
-- name: ListScanActionsByScans :many
SELECT * FROM patrol_scan_actions WHERE scan_id = ANY(@scan_ids::bigint[])
ORDER BY scan_id, (snapshot->>'sort_order')::int, id;

-- The order rule (Phase 10.9)
-- ===========================

-- How many times one person scanned each checkpoint of a zone on a work date, before a moment, in the
-- zone's order. Every active checkpoint of the zone is listed, with 0 when it was not scanned yet, so
-- the caller always gets one row per position. Any status counts: a later decline does not undo the walk.
-- name: CountScansByPosition :many
SELECT zc.checkpoint_id, zc.sort_order AS position, count(s.id)::int AS scans
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
LEFT JOIN patrol_scans s ON s.organization_id = zc.organization_id AND s.zone_id = zc.zone_id
                        AND s.checkpoint_id = zc.checkpoint_id AND s.personnel_id = @personnel_id
                        AND s.work_date = @work_date AND s.occurred_at < @before
WHERE zc.zone_id = @zone_id AND c.active AND c.deleted_at IS NULL
GROUP BY zc.checkpoint_id, zc.sort_order
ORDER BY zc.sort_order;
