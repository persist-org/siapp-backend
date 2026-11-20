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

-- A new secret invalidates the sticker printed before, for a scan made after the rotation. The secret it had
-- is kept as previous_qr_secret (one generation: a second rotation overwrites it), so an offline scan made
-- before the rotation is still accepted when it syncs (Phase 10.18). Both right-hand sides read the row as
-- it was before the update.
-- name: RotateCheckpointSecret :one
UPDATE patrol_checkpoints SET previous_qr_secret = qr_secret, qr_secret = @qr_secret, qr_rotated_at = @now, updated_at = @now
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

-- The active checkpoints of many zones at once, each zone's in order (the bundled field list, Phase 10.18).
-- name: ListCheckpointsOfZones :many
SELECT c.*, zc.sort_order AS position, zc.zone_id
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
WHERE zc.zone_id = ANY(@zone_ids::bigint[]) AND c.active AND c.deleted_at IS NULL
ORDER BY zc.zone_id, zc.sort_order;

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

-- Is this checkpoint one a guard can scan in this zone now: linked to it, and active? Its position when it is.
-- name: GetZoneCheckpointLink :one
SELECT zc.sort_order FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
WHERE zc.zone_id = $1 AND zc.checkpoint_id = $2 AND c.active AND c.deleted_at IS NULL;

-- Every checkpoint a zone holds, in order, the inactive ones too (an admin sees them and they keep
-- their place); only a deleted one is gone. is_active tells them apart.
-- name: ListZoneCheckpointsAdmin :many
SELECT c.*, zc.sort_order AS position
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
WHERE zc.zone_id = $1 AND c.deleted_at IS NULL
ORDER BY zc.sort_order;

-- How many active checkpoints each zone has: what a guard can scan, for the zone lists.
-- name: CountCheckpointsOfZones :many
SELECT zc.zone_id, count(*)::int AS checkpoints
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
WHERE zc.zone_id = ANY(@zone_ids::bigint[]) AND c.active AND c.deleted_at IS NULL
GROUP BY zc.zone_id;

-- Locks the checkpoints a zone is about to link, in id order, and returns them. ⚠ The order of locks
-- is checkpoint first, then zone, everywhere (deleting a checkpoint does the same): a link insert takes
-- a share lock on its checkpoint, so locking the zone first and the checkpoint second would deadlock
-- against a deletion that holds the checkpoint and waits for the zone.
-- name: LockCheckpointsByIDs :many
SELECT * FROM patrol_checkpoints
WHERE id = ANY(@ids::uuid[]) AND deleted_at IS NULL
ORDER BY id
FOR UPDATE;

-- Closing the gap a deleted checkpoint leaves: every link of the zone gets its rank, 1..n, in one UPDATE.
-- ⚠ One UPDATE of rows this transaction has not written, and never a delete and re-insert: an insert
-- checks its foreign key by taking a share lock on the checkpoint it points at, which for the zone's
-- OTHER checkpoints is a lock the deleting request does not hold and may have to wait for while the request
-- that holds it waits for this one (a deadlock). An UPDATE that leaves the key columns alone checks no
-- foreign key, unless the row was already written by this very transaction. Several rows change position
-- at once, which is why the position's unique is deferred (migration 00039).
-- name: RenumberZoneLinks :exec
UPDATE patrol_zone_checkpoints t SET sort_order = r.n
FROM (SELECT checkpoint_id, row_number() OVER (ORDER BY sort_order)::int AS n
      FROM patrol_zone_checkpoints WHERE zone_id = $1) r
WHERE t.zone_id = $1 AND t.checkpoint_id = r.checkpoint_id;

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
INSERT INTO patrol_scan_actions (scan_id, checkpoint_id, action_id, personnel_id, note, media_ids, snapshot, device_condition, created_at)
VALUES (@scan_id, @checkpoint_id, @action_id, @personnel_id, sqlc.narg('note'), @media_ids, @snapshot, sqlc.narg('device_condition'), @now)
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
--
-- last_scanned_at (Phase 10.19) is the person's latest scan of the checkpoint that the patrol interval counts:
-- outcome SCANNED and not declined, the same rows HasScanBetween looks at (a skip is not a visit, and a visit the
-- DANRU turned down did not happen). NULL when there is none. The order rule above counts every status; this
-- column does not, on purpose.
-- name: CountScansByPosition :many
SELECT zc.checkpoint_id, zc.sort_order AS position, count(s.id)::int AS scans,
       (max(s.occurred_at) FILTER (WHERE s.outcome = 'SCANNED' AND s.status <> 'DECLINED'))::timestamptz AS last_scanned_at
FROM patrol_zone_checkpoints zc
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
LEFT JOIN patrol_scans s ON s.organization_id = zc.organization_id AND s.zone_id = zc.zone_id
                        AND s.checkpoint_id = zc.checkpoint_id AND s.personnel_id = @personnel_id
                        AND s.work_date = @work_date AND s.occurred_at < @before
WHERE zc.zone_id = @zone_id AND c.active AND c.deleted_at IS NULL
GROUP BY zc.checkpoint_id, zc.sort_order
ORDER BY zc.sort_order;

-- CountScansByPosition for many zones at once, each on its own work date (the bundled field list, Phase 10.18):
-- zone_ids[i] is read on work_dates[i]. Same rows, same counting (any status, any outcome), no "before": the
-- field read wants every scan so far. One row per active checkpoint of each zone, ordered by zone and position.
-- name: CountScansByPositionOfZones :many
SELECT zc.zone_id, zc.checkpoint_id, zc.sort_order AS position, count(s.id)::int AS scans,
       (max(s.occurred_at) FILTER (WHERE s.outcome = 'SCANNED' AND s.status <> 'DECLINED'))::timestamptz AS last_scanned_at
FROM (SELECT z.zone_id, d.work_date
      FROM unnest(@zone_ids::bigint[]) WITH ORDINALITY AS z(zone_id, n)
      JOIN unnest(@work_dates::date[]) WITH ORDINALITY AS d(work_date, n) ON d.n = z.n) w
JOIN patrol_zone_checkpoints zc ON zc.zone_id = w.zone_id
JOIN patrol_checkpoints c ON c.organization_id = zc.organization_id AND c.id = zc.checkpoint_id
LEFT JOIN patrol_scans s ON s.organization_id = zc.organization_id AND s.zone_id = zc.zone_id
                        AND s.checkpoint_id = zc.checkpoint_id AND s.personnel_id = @personnel_id
                        AND s.work_date = w.work_date
WHERE c.active AND c.deleted_at IS NULL
GROUP BY zc.zone_id, zc.checkpoint_id, zc.sort_order
ORDER BY zc.zone_id, zc.sort_order;
