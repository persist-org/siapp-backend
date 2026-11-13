-- Progress, approval and decline of scans (Phase 10.3). Everything runs inside database.WithTenant.

-- name: GetPatrolScan :one
SELECT * FROM patrol_scans WHERE id = $1;

-- name: GetPatrolScansByIDs :many
SELECT * FROM patrol_scans WHERE id = ANY(@ids::bigint[]) ORDER BY id;

-- One zone's scans, narrowed by what the caller may see: the scans of the regu in team_codes, and
-- always their own (viewer_id). An empty team_codes leaves only the caller's own.
-- name: ListZoneScans :many
SELECT * FROM patrol_scans
WHERE zone_id = @zone_id
  AND (team_code = ANY(@team_codes::text[]) OR personnel_id = @viewer_id)
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('work_date')::date IS NULL OR work_date = sqlc.narg('work_date'))
  AND (sqlc.narg('status')::patrol_scan_status IS NULL OR status = sqlc.narg('status'))
ORDER BY occurred_at DESC, id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountZoneScans :one
SELECT count(*) FROM patrol_scans
WHERE zone_id = @zone_id
  AND (team_code = ANY(@team_codes::text[]) OR personnel_id = @viewer_id)
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('work_date')::date IS NULL OR work_date = sqlc.narg('work_date'))
  AND (sqlc.narg('status')::patrol_scan_status IS NULL OR status = sqlc.narg('status'));

-- The DANRU's queue: SUBMITTED scans, oldest first, of the regu each permission reaches. A scan of a
-- patrol zone is judged by patrol.approve and one of a guard zone by guard.approve, so each type
-- has its own list of regu.
-- Which (zone type, regu, work date) the SUBMITTED scans fall in: what the service resolves the
-- caller's reach for, one date at a time, before it lists them.
-- name: ListPendingKeys :many
SELECT DISTINCT zone_type, team_code, work_date FROM patrol_scans
WHERE status = 'SUBMITTED'
  AND (sqlc.narg('team_code')::text IS NULL OR team_code = sqlc.narg('team_code'));

-- name: ListPendingScans :many
SELECT * FROM patrol_scans
WHERE status = 'SUBMITTED'
  AND (zone_type::text, team_code, work_date) IN (SELECT unnest(@types::text[]), unnest(@teams::text[]), unnest(@days::date[]))
  AND (sqlc.narg('team_code')::text IS NULL OR team_code = sqlc.narg('team_code'))
ORDER BY occurred_at, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPendingScans :one
SELECT count(*) FROM patrol_scans
WHERE status = 'SUBMITTED'
  AND (zone_type::text, team_code, work_date) IN (SELECT unnest(@types::text[]), unnest(@teams::text[]), unnest(@days::date[]))
  AND (sqlc.narg('team_code')::text IS NULL OR team_code = sqlc.narg('team_code'));

-- ⚠ The three circles of every zone in ONE query: the zones LEFT JOIN the day's scans, grouped by
-- zone, with count(*) FILTER for each status. One round trip however many zones the project has,
-- and a zone nobody scanned still comes back (all zeros) because of the LEFT JOIN. The scans are
-- joined on the regu in team_codes, so the counts are those of the regu the caller may see.
-- name: ZoneProgress :many
SELECT z.id, z.project_code, z.team_code, z.type, z.name, z.alias, z.min_visit,
       count(s.id) FILTER (WHERE s.status = 'SUBMITTED')::int AS submitted,
       count(s.id) FILTER (WHERE s.status = 'APPROVED')::int  AS approved,
       count(s.id) FILTER (WHERE s.status = 'DECLINED')::int  AS declined
FROM patrol_zones z
LEFT JOIN patrol_scans s ON s.zone_id = z.id AND s.work_date = @work_date AND s.team_code = ANY(@team_codes::text[])
WHERE z.deleted_at IS NULL AND z.active AND z.project_code = @project_code AND z.type = @type
  AND (z.team_code IS NULL OR z.team_code = ANY(@team_codes::text[]))
GROUP BY z.id
ORDER BY z.name, z.id;

-- Who scanned a zone on a day, with the number next to each name (§3.2.6.2). personnel_id narrows
-- it to one person, for a reader who may only see themselves.
-- name: ZoneScanners :many
SELECT s.personnel_id, p.full_name,
       count(*) FILTER (WHERE s.status = 'SUBMITTED')::int AS submitted_count,
       count(*) FILTER (WHERE s.status = 'APPROVED')::int  AS approved_count
FROM patrol_scans s
JOIN personnels p ON p.id = s.personnel_id
WHERE s.zone_id = @zone_id AND s.work_date = @work_date AND s.team_code = ANY(@team_codes::text[])
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR s.personnel_id = sqlc.narg('personnel_id'))
GROUP BY s.personnel_id, p.full_name
ORDER BY p.full_name, s.personnel_id;

-- ⚠ The decision itself. The status in the WHERE is what makes it safe: two DANRU deciding the same
-- scan at once both run this statement, Postgres re-checks the status on the row the other just
-- changed, and exactly one of them gets it. A row that is not returned was already decided.
-- name: DecideScans :many
UPDATE patrol_scans SET status = @status, decided_by_id = @decided_by, decided_at = @now,
                        decision_note = sqlc.narg('note'), updated_at = @now
WHERE id = ANY(@ids::bigint[]) AND status = 'SUBMITTED'
RETURNING *;

-- The photos of scans: one row per photo, with the scan it is attached to.
-- name: ListScanPhotos :many
SELECT attached_entity_id AS scan_id, id AS media_id FROM media
WHERE attached_entity_type = 'PATROL_SCAN' AND attached_entity_id = ANY(@scan_ids::bigint[]) AND status = 'UPLOADED'
ORDER BY attached_entity_id, id;

-- name: ListPersonnelNames :many
SELECT id, full_name FROM personnels WHERE id = ANY(@ids::bigint[]);
