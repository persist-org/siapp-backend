-- Branches (kantor cabang). Everything runs inside database.WithTenant.
--
-- A soft-deleted branch keeps its row, so its code stays taken (D19). Reads below hide it;
-- CreateBranch does not, so reusing the code is a unique violation.

-- scope_region is the REGION_ADMIN's own region (NULL for an ORG_ADMIN, who sees all).
-- region_code is the optional filter of the request.
-- name: ListBranches :many
SELECT * FROM branches
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR region_code = sqlc.narg('scope_region'))
  AND (sqlc.narg('region_code')::text IS NULL OR region_code = sqlc.narg('region_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0)
ORDER BY length(code), code
LIMIT @page_limit OFFSET @page_offset;

-- name: CountBranches :one
SELECT count(*) FROM branches
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR region_code = sqlc.narg('scope_region'))
  AND (sqlc.narg('region_code')::text IS NULL OR region_code = sqlc.narg('region_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0);

-- name: GetBranch :one
SELECT * FROM branches WHERE code = $1 AND deleted_at IS NULL;

-- Locks the row, so an edit or a delete of one branch is serialised with another admin's,
-- and a client created under it cannot slip in beside a delete.
-- name: GetBranchForUpdate :one
SELECT * FROM branches WHERE code = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CreateBranch :one
INSERT INTO branches (code, region_code, company_letter, manager_name, city, phone, fax, email,
                      address, zip_code, description, active, created_at, updated_at)
VALUES (@code, @region_code, @company_letter, @manager_name, @city, @phone, @fax, @email,
        @address, @zip_code, @description, @active, @now, @now)
RETURNING *;

-- The code, the region and the letter are not here: the code is built from them (D19).
-- name: UpdateBranch :one
UPDATE branches SET
    manager_name = COALESCE(sqlc.narg('manager_name'), manager_name),
    city         = COALESCE(sqlc.narg('city'), city),
    phone        = COALESCE(sqlc.narg('phone'), phone),
    fax          = COALESCE(sqlc.narg('fax'), fax),
    email        = COALESCE(sqlc.narg('email'), email),
    address      = COALESCE(sqlc.narg('address'), address),
    zip_code     = COALESCE(sqlc.narg('zip_code'), zip_code),
    description  = COALESCE(sqlc.narg('description'), description),
    active       = COALESCE(sqlc.narg('active'), active),
    updated_at   = @now
WHERE code = @code AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteBranch :execrows
UPDATE branches SET deleted_at = @now, active = false, updated_at = @now
WHERE code = @code AND deleted_at IS NULL;

-- A branch cannot be deleted while a client that is not deleted sits under it, active or not.
-- name: CountClientsOfBranch :one
SELECT count(*) FROM clients WHERE branch_code = $1 AND deleted_at IS NULL;
