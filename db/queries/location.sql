-- GPS points (Phase 08.2). Everything here runs inside database.WithTenant, except the two
-- partition functions, which take no tenant.

-- Stores a batch of points of one person. @points is a JSON array of
-- {logged_at, latitude, longitude, accuracy_meters, battery_pct}; battery_pct may be null. A point
-- already stored (same person, same logged_at) is skipped, so a batch the phone sends again after
-- a lost answer is harmless. Returns how many rows were really inserted.
-- name: InsertLocationLogs :execrows
INSERT INTO location_logs (personnel_id, logged_at, latitude, longitude, accuracy_meters, battery_pct)
SELECT @personnel_id::bigint, u.logged_at, u.latitude, u.longitude, u.accuracy_meters, u.battery_pct
FROM jsonb_to_recordset(@points::jsonb)
     AS u(logged_at timestamptz, latitude float8, longitude float8, accuracy_meters float8, battery_pct smallint)
ON CONFLICT (organization_id, personnel_id, logged_at) DO NOTHING;

-- ⚠ Only moves forward: an offline batch can arrive after a newer point is already known, and
-- must never put a person "back in time". Returns 1 when the row was written, 0 when the stored
-- point is the same age or newer.
-- name: UpsertLastLocation :execrows
INSERT INTO personnel_last_locations (personnel_id, logged_at, latitude, longitude, accuracy_meters)
VALUES (@personnel_id, @logged_at, @latitude, @longitude, @accuracy_meters)
ON CONFLICT (organization_id, personnel_id) DO UPDATE
SET logged_at = EXCLUDED.logged_at, latitude = EXCLUDED.latitude,
    longitude = EXCLUDED.longitude, accuracy_meters = EXCLUDED.accuracy_meters
WHERE EXCLUDED.logged_at > personnel_last_locations.logged_at;

-- The people who serve in some regu today with their last known point (NULL columns when they
-- never sent one); a person in two regu appears once per regu. Filters are optional and combine: regu, project, one person. A person Personalia
-- reports as inactive has left and is not listed. Whether they are online is not known here.
-- name: ListMemberLocations :many
SELECT tm.team_code, t.project_code, tm.personnel_id, tm.role, p.full_name, p.photo_url, p.position_name,
       l.logged_at, l.latitude, l.longitude, l.accuracy_meters
FROM team_members tm
JOIN teams t     ON t.organization_id = tm.organization_id AND t.code = tm.team_code AND t.deleted_at IS NULL
JOIN projects pr ON pr.organization_id = t.organization_id AND pr.code = t.project_code AND pr.deleted_at IS NULL
JOIN personnels p ON p.id = tm.personnel_id AND p.status = 'ACTIVE'
LEFT JOIN personnel_last_locations l ON l.organization_id = tm.organization_id AND l.personnel_id = tm.personnel_id
WHERE tm.start_date <= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date
  AND (tm.end_date IS NULL OR tm.end_date >= (sqlc.arg('now')::timestamptz AT TIME ZONE pr.timezone)::date)
  AND (sqlc.narg('team_codes')::text[] IS NULL OR tm.team_code = ANY(sqlc.narg('team_codes')::text[]))
  AND (sqlc.narg('project_codes')::text[] IS NULL OR t.project_code = ANY(sqlc.narg('project_codes')::text[]))
  AND (sqlc.narg('personnel_id')::bigint IS NULL OR tm.personnel_id = sqlc.narg('personnel_id')::bigint)
ORDER BY t.project_code, length(tm.team_code), tm.team_code, (tm.role <> 'DANRU'), lower(p.full_name), tm.personnel_id;

-- Partition upkeep. These are SECURITY DEFINER functions (migration 00031): the app role cannot
-- run DDL itself.
-- name: EnsureLocationPartition :exec
SELECT ensure_location_partition(@month::date);

-- name: DropLocationPartitions :one
SELECT drop_location_partitions(@cutoff::timestamptz)::int;
