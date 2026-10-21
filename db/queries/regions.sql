-- Regions (koordinator wilayah). Everything runs inside database.WithTenant.
--
-- A soft-deleted region keeps its row, so its code stays taken (D19). Reads below hide it;
-- CreateRegion does not, so reusing the code is a unique violation.

-- Filters are optional: a NULL narg matches everything. region_code is the REGION_ADMIN's own
-- region (NULL for an ORG_ADMIN, who sees all).
-- name: ListRegions :many
SELECT * FROM regions
WHERE deleted_at IS NULL
  AND (sqlc.narg('region_code')::text IS NULL OR code = sqlc.narg('region_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0)
-- OS2 before OS10: by length first, then text.
ORDER BY length(code), code
LIMIT @page_limit OFFSET @page_offset;

-- name: CountRegions :one
SELECT count(*) FROM regions
WHERE deleted_at IS NULL
  AND (sqlc.narg('region_code')::text IS NULL OR code = sqlc.narg('region_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0);

-- name: GetRegion :one
SELECT * FROM regions WHERE code = $1 AND deleted_at IS NULL;

-- A region something is about to point at (a REGION_ADMIN grant): FOR SHARE waits for a delete
-- of it in flight, and then no longer finds it.
-- name: GetRegionForShare :one
SELECT * FROM regions WHERE code = $1 AND deleted_at IS NULL FOR SHARE;

-- Locks the row, so an edit or a delete of one region is serialised with another admin's.
-- name: GetRegionForUpdate :one
SELECT * FROM regions WHERE code = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CreateRegion :one
INSERT INTO regions (code, description, active, created_at, updated_at)
VALUES (@code, @description, @active, @now, @now)
RETURNING *;

-- name: UpdateRegion :one
UPDATE regions SET
    description = COALESCE(sqlc.narg('description'), description),
    active      = COALESCE(sqlc.narg('active'), active),
    updated_at  = @now
WHERE code = @code AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteRegion :execrows
UPDATE regions SET deleted_at = @now, active = false, updated_at = @now
WHERE code = @code AND deleted_at IS NULL;

-- A region cannot be deleted while a REGION_ADMIN points at it.
-- name: CountWebRolesOfRegion :one
SELECT count(*) FROM web_role_assignments WHERE region_code = $1;

-- A region cannot be deleted while a kantor cabang that is not deleted sits under it,
-- active or not: an inactive one would otherwise outlive its region (and could be switched
-- back on under it).
-- name: CountBranchesOfRegion :one
SELECT count(*) FROM branches WHERE region_code = $1 AND deleted_at IS NULL;
