-- Projects (proyek). Everything runs inside database.WithTenant.
--
-- The region of a project is the number its code starts with (1K-IBMI-S is under OS1), as
-- for clients. scope_region is the REGION_ADMIN's own region (NULL for an ORG_ADMIN).

-- name: ListProjects :many
SELECT * FROM projects
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('client_code')::text IS NULL OR client_code = sqlc.narg('client_code'))
  AND (sqlc.narg('business_line')::business_line IS NULL OR business_line = sqlc.narg('business_line'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR code ILIKE '%' || sqlc.narg('q') || '%' OR description ILIKE '%' || sqlc.narg('q') || '%')
ORDER BY code
LIMIT @page_limit OFFSET @page_offset;

-- name: CountProjects :one
SELECT count(*) FROM projects
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('client_code')::text IS NULL OR client_code = sqlc.narg('client_code'))
  AND (sqlc.narg('business_line')::business_line IS NULL OR business_line = sqlc.narg('business_line'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR code ILIKE '%' || sqlc.narg('q') || '%' OR description ILIKE '%' || sqlc.narg('q') || '%');

-- name: GetProject :one
SELECT * FROM projects WHERE code = $1 AND deleted_at IS NULL;

-- name: GetProjectForUpdate :one
SELECT * FROM projects WHERE code = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CreateProject :one
INSERT INTO projects (code, client_code, description, company_code, company_prefix, company_bidang_usaha,
                      business_line, city, address, phone, email, timezone, latitude, longitude,
                      attendance_radius_meters, strict_attendance_geofence, gps_tracking_enabled,
                      patrol_interval_minutes, emergency_contacts, active,
                      created_by, modify_by, created_at, updated_at)
VALUES (@code, @client_code, @description, @company_code, @company_prefix, @company_bidang_usaha,
        @business_line, @city, @address, @phone, @email, @timezone, @latitude, @longitude,
        @attendance_radius_meters, @strict_attendance_geofence, @gps_tracking_enabled,
        @patrol_interval_minutes, @emergency_contacts, @active,
        @actor, @actor, @now, @now)
RETURNING *;

-- The code, the client, the prefix and the business line are not here: the code is built
-- from them (D19). A NULL leaves a column alone, so a value set once (a coordinate, the
-- patrol interval) can be changed but not cleared.
-- name: UpdateProject :one
UPDATE projects SET
    description                = COALESCE(sqlc.narg('description'), description),
    company_code               = COALESCE(sqlc.narg('company_code'), company_code),
    company_bidang_usaha       = COALESCE(sqlc.narg('company_bidang_usaha'), company_bidang_usaha),
    city                       = COALESCE(sqlc.narg('city'), city),
    address                    = COALESCE(sqlc.narg('address'), address),
    phone                      = COALESCE(sqlc.narg('phone'), phone),
    email                      = COALESCE(sqlc.narg('email'), email),
    timezone                   = COALESCE(sqlc.narg('timezone'), timezone),
    latitude                   = COALESCE(sqlc.narg('latitude'), latitude),
    longitude                  = COALESCE(sqlc.narg('longitude'), longitude),
    attendance_radius_meters   = COALESCE(sqlc.narg('attendance_radius_meters'), attendance_radius_meters),
    strict_attendance_geofence = COALESCE(sqlc.narg('strict_attendance_geofence'), strict_attendance_geofence),
    gps_tracking_enabled       = COALESCE(sqlc.narg('gps_tracking_enabled'), gps_tracking_enabled),
    patrol_interval_minutes    = COALESCE(sqlc.narg('patrol_interval_minutes'), patrol_interval_minutes),
    emergency_contacts         = COALESCE(sqlc.narg('emergency_contacts')::jsonb, emergency_contacts),
    active                     = COALESCE(sqlc.narg('active'), active),
    modify_by                  = @actor,
    updated_at                 = @now
WHERE code = @code AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteProject :execrows
UPDATE projects SET deleted_at = @now, active = false, modify_by = @actor, updated_at = @now
WHERE code = @code AND deleted_at IS NULL;

-- name: CountActiveTeamsOfProject :one
SELECT count(*) FROM teams WHERE project_code = $1 AND deleted_at IS NULL AND active;

-- The number of the next regu of a project. ⚠ The counter only ever goes up and runs in the
-- same transaction as the team insert: a number that was handed out is never handed out
-- again, even when its team was deleted (past attendance and reports keep pointing at it).
-- name: TakeNextTeamNumber :one
UPDATE projects SET next_team_number = next_team_number + 1
WHERE code = @code AND deleted_at IS NULL
RETURNING (next_team_number - 1)::int AS number;

-- A project with its client, branch and region, for reports. The ancestors are not filtered
-- by deleted_at: a report about the past still names a branch that has since been deleted.
-- name: GetProjectAncestors :one
SELECT c.code AS client_code, c.description AS client_description,
       b.code AS branch_code, b.description AS branch_description,
       r.code AS region_code, r.description AS region_description
FROM projects p
JOIN clients  c ON c.organization_id = p.organization_id AND c.code = p.client_code
JOIN branches b ON b.organization_id = c.organization_id AND b.code = c.branch_code
JOIN regions  r ON r.organization_id = b.organization_id AND r.code = b.region_code
WHERE p.code = $1;
