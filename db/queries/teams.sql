-- Teams (regu). Everything runs inside database.WithTenant.
--
-- The region of a team is the number its code starts with (1K-IBMI-S1 is under OS1), as for
-- clients. scope_region is the REGION_ADMIN's own region (NULL for an ORG_ADMIN).

-- name: ListTeams :many
SELECT * FROM teams
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0)
-- S2 before S10: the project, then the number.
ORDER BY project_code, length(code), code
LIMIT @page_limit OFFSET @page_offset;

-- name: CountTeams :one
SELECT count(*) FROM teams
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('project_code')::text IS NULL OR project_code = sqlc.narg('project_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0);

-- name: GetTeam :one
SELECT * FROM teams WHERE code = $1 AND deleted_at IS NULL;

-- name: GetTeamForUpdate :one
SELECT * FROM teams WHERE code = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CreateTeam :one
INSERT INTO teams (code, project_code, description, phone, min_personnel, notes, validate_shift,
                   reject_attendance_after_tolerance, relationship_officer_personnel_id, active,
                   created_at, updated_at)
VALUES (@code, @project_code, @description, @phone, @min_personnel, @notes, @validate_shift,
        @reject_attendance_after_tolerance, @relationship_officer_personnel_id, @active, @now, @now)
RETURNING *;

-- The code and the project are not here: the code is built from them (D19).
-- name: UpdateTeam :one
UPDATE teams SET
    description                       = COALESCE(sqlc.narg('description'), description),
    phone                             = COALESCE(sqlc.narg('phone'), phone),
    min_personnel                     = COALESCE(sqlc.narg('min_personnel'), min_personnel),
    notes                             = COALESCE(sqlc.narg('notes'), notes),
    validate_shift                    = COALESCE(sqlc.narg('validate_shift'), validate_shift),
    reject_attendance_after_tolerance = COALESCE(sqlc.narg('reject_attendance_after_tolerance'), reject_attendance_after_tolerance),
    -- set_officer tells "not sent" (keep) from null (remove the officer).
    relationship_officer_personnel_id = CASE WHEN @set_officer::boolean THEN sqlc.narg('relationship_officer_personnel_id')::bigint
                                             ELSE relationship_officer_personnel_id END,
    active                            = COALESCE(sqlc.narg('active'), active),
    updated_at                        = @now
WHERE code = @code AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteTeam :execrows
UPDATE teams SET deleted_at = @now, active = false, updated_at = @now
WHERE code = @code AND deleted_at IS NULL;

-- A team's relationship officer is a person of the caller's organization (RLS hides the
-- others), so an id from another tenant is not found.
