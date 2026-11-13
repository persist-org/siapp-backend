-- The patrol route (Phase 10.4). Everything runs inside database.WithTenant.

-- name: GetPatrolRouteByLocalID :one
SELECT * FROM patrol_routes WHERE local_id = $1;

-- name: GetPatrolRoute :one
SELECT * FROM patrol_routes WHERE id = $1;

-- name: GetActivePatrolRoute :one
SELECT * FROM patrol_routes WHERE personnel_id = $1 AND status = 'ACTIVE';

-- name: InsertPatrolRoute :one
INSERT INTO patrol_routes (local_id, personnel_id, team_code, project_code, started_at, work_date, submitted_offline,
                           point_count, last_point_at, created_at, updated_at)
VALUES (@local_id, @personnel_id, @team_code, @project_code, @started_at, @work_date, @submitted_offline,
        1, @started_at, @now, @now)
RETURNING *;

-- The points of one batch. The same seq twice is not an error, it is skipped: the app numbers its
-- points, so a retried or offline batch cannot duplicate or reorder anything. The number of rows
-- inserted is the number accepted.
-- name: InsertRoutePoints :execrows
INSERT INTO patrol_route_points (patrol_id, seq, logged_at, latitude, longitude, accuracy_meters)
-- Five unnest() in one select list run side by side (PostgreSQL 10 and later), one row per index,
-- which is what a column-wise batch needs; the arrays are always the same length.
SELECT @patrol_id::bigint, unnest(@seqs::int[]), unnest(@logged_ats::timestamptz[]), unnest(@lats::float8[]),
       unnest(@lngs::float8[]), unnest(@accs::float8[])
ON CONFLICT (patrol_id, seq) DO NOTHING;

-- A batch moves the patrol's last_point_at to its newest point, never back (a late batch of old
-- points), and adds the points it brought to the count.
-- name: NoteRoutePoints :exec
UPDATE patrol_routes
SET last_point_at = GREATEST(last_point_at, @latest::timestamptz), point_count = point_count + @added::int, updated_at = @now
WHERE id = @id;

-- name: ListRoutePoints :many
SELECT seq, logged_at, latitude, longitude, accuracy_meters FROM patrol_route_points
WHERE patrol_id = $1 ORDER BY seq;

-- name: CountRoutePoints :one
SELECT count(*) FROM patrol_route_points WHERE patrol_id = $1;

-- Ends a patrol that is still ACTIVE. No row: someone else ended it first.
-- name: EndPatrolRoute :one
UPDATE patrol_routes
SET status = @status, ended_at = @ended_at, path = @path, distance_meters = @distance_meters,
    point_count = @point_count, scan_count = @scan_count, updated_at = @now
WHERE id = @id AND status = 'ACTIVE'
RETURNING *;

-- The scans of a patrol, in the order they were made, for the pins on the line.
-- name: ListRouteScans :many
SELECT s.id, s.zone_id, z.name AS zone_name, s.occurred_at, s.latitude, s.longitude, s.status, s.subject
FROM patrol_scans s
JOIN patrol_zones z ON z.id = s.zone_id
WHERE s.patrol_id = $1
ORDER BY s.occurred_at, s.id;

-- The patrol a scan belongs to: the one the person had running in this project at that moment. A
-- patrol that has ended counts for the scans made before its end, so an offline scan delivered
-- after the patrol ended still joins it.
-- name: FindRouteForScan :one
SELECT id FROM patrol_routes
WHERE personnel_id = @personnel_id AND project_code = @project_code
  AND started_at <= @at AND (ended_at IS NULL OR @at <= ended_at)
ORDER BY started_at DESC
LIMIT 1;

-- name: AddScanToRoute :exec
UPDATE patrol_routes SET scan_count = scan_count + 1 WHERE id = $1;

-- The list leaves the path out: a long patrol's path is a megabyte, and a page holds fifty.
-- team_codes is what the caller reaches as a DANRU; their own patrols are always visible.
-- name: ListPatrolRoutes :many
SELECT r.id, r.organization_id, r.local_id, r.personnel_id, r.team_code, r.project_code, r.status, r.started_at,
       r.ended_at, r.work_date, r.distance_meters, r.point_count, r.scan_count, r.submitted_offline,
       r.last_point_at, r.created_at, r.updated_at, p.full_name
FROM patrol_routes r
JOIN personnels p ON p.id = r.personnel_id
WHERE (r.personnel_id = @viewer_id OR r.team_code = ANY(@team_codes::text[]))
  AND (sqlc.narg('project_code')::text IS NULL OR r.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('team_code')::text IS NULL OR r.team_code = sqlc.narg('team_code'))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR r.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('work_date')::date IS NULL OR r.work_date = sqlc.narg('work_date'))
  AND (sqlc.narg('status')::patrol_route_status IS NULL OR r.status = sqlc.narg('status'))
ORDER BY r.started_at DESC, r.id DESC
LIMIT @page_limit OFFSET @page_offset;

-- name: CountPatrolRoutes :one
SELECT count(*) FROM patrol_routes r
WHERE (r.personnel_id = @viewer_id OR r.team_code = ANY(@team_codes::text[]))
  AND (sqlc.narg('project_code')::text IS NULL OR r.project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('team_code')::text IS NULL OR r.team_code = sqlc.narg('team_code'))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR r.personnel_id = sqlc.narg('personnel_id'))
  AND (sqlc.narg('work_date')::date IS NULL OR r.work_date = sqlc.narg('work_date'))
  AND (sqlc.narg('status')::patrol_route_status IS NULL OR r.status = sqlc.narg('status'));

-- The active patrols that have logged nothing since a moment: their phone died or the guard forgot.
-- name: ListStaleRoutes :many
SELECT id FROM patrol_routes WHERE status = 'ACTIVE' AND last_point_at < @before ORDER BY id LIMIT @batch;

-- The ended patrols whose raw points are due to go: ended before the cutoff and still holding some.
-- name: ListRoutesWithOldPoints :many
SELECT r.id FROM patrol_routes r
WHERE r.status <> 'ACTIVE' AND r.ended_at < @before
  AND EXISTS (SELECT 1 FROM patrol_route_points p WHERE p.patrol_id = r.id)
ORDER BY r.id
LIMIT @batch;

-- name: DeleteRoutePoints :execrows
DELETE FROM patrol_route_points WHERE patrol_id = ANY(@ids::bigint[]);
