-- Clients (klien). Everything runs inside database.WithTenant.
--
-- The region of a client is the number its code starts with (1K-IBMI is under OS1): the
-- branch code embeds it and the CHECK on branches keeps it true. scope_region is the
-- REGION_ADMIN's own region (NULL for an ORG_ADMIN, who sees all).

-- name: ListClients :many
SELECT * FROM clients
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('branch_code')::text IS NULL OR branch_code = sqlc.narg('branch_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0)
ORDER BY code
LIMIT @page_limit OFFSET @page_offset;

-- name: CountClients :one
SELECT count(*) FROM clients
WHERE deleted_at IS NULL
  AND (sqlc.narg('scope_region')::text IS NULL OR 'OS' || substring(code from '^[0-9]+') = sqlc.narg('scope_region'))
  AND (sqlc.narg('branch_code')::text IS NULL OR branch_code = sqlc.narg('branch_code'))
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(code), lower(sqlc.narg('q'))) > 0 OR strpos(lower(description), lower(sqlc.narg('q'))) > 0);

-- name: GetClient :one
SELECT * FROM clients WHERE code = $1 AND deleted_at IS NULL;

-- name: GetClientForUpdate :one
SELECT * FROM clients WHERE code = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: CreateClient :one
INSERT INTO clients (code, branch_code, description, company_code, company_prefix, company_bidang_usaha,
                     director_name, city, phone, fax, address, zip_code, email, active,
                     created_by, modify_by, created_at, updated_at)
VALUES (@code, @branch_code, @description, @company_code, @company_prefix, @company_bidang_usaha,
        @director_name, @city, @phone, @fax, @address, @zip_code, @email, @active,
        @actor, @actor, @now, @now)
RETURNING *;

-- The code, the branch and the prefix are not here: the code is built from them (D19).
-- name: UpdateClient :one
UPDATE clients SET
    description          = COALESCE(sqlc.narg('description'), description),
    company_code         = COALESCE(sqlc.narg('company_code'), company_code),
    company_bidang_usaha = COALESCE(sqlc.narg('company_bidang_usaha'), company_bidang_usaha),
    director_name        = COALESCE(sqlc.narg('director_name'), director_name),
    city                 = COALESCE(sqlc.narg('city'), city),
    phone                = COALESCE(sqlc.narg('phone'), phone),
    fax                  = COALESCE(sqlc.narg('fax'), fax),
    address              = COALESCE(sqlc.narg('address'), address),
    zip_code             = COALESCE(sqlc.narg('zip_code'), zip_code),
    email                = COALESCE(sqlc.narg('email'), email),
    active               = COALESCE(sqlc.narg('active'), active),
    modify_by            = @actor,
    updated_at           = @now
WHERE code = @code AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteClient :execrows
UPDATE clients SET deleted_at = @now, active = false, modify_by = @actor, updated_at = @now
WHERE code = @code AND deleted_at IS NULL;

-- A client cannot be deleted while a project that is not deleted sits under it, active or not.
-- name: CountProjectsOfClient :one
SELECT count(*) FROM projects WHERE client_code = $1 AND deleted_at IS NULL;
