-- Web role assignments (the admin gate). Everything runs inside database.WithTenant.

-- name: GetWebRole :one
SELECT * FROM web_role_assignments WHERE personnel_id = $1;

-- The admin list shows who each assignment belongs to, so it reads the name and NIK of the
-- people cache (identity's table) in the same query. Read-only.
-- name: ListWebRoles :many
SELECT w.personnel_id, p.full_name, p.nik, w.role, w.project_ids, w.granted_by_personnel_id,
       w.created_at, w.updated_at
FROM web_role_assignments w
JOIN personnels p ON p.id = w.personnel_id
ORDER BY p.full_name, w.personnel_id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountWebRoles :one
SELECT count(*) FROM web_role_assignments;

-- Grants or changes a role. A person who is not in the people cache is a foreign key
-- failure, which the caller reports as NOT_FOUND.
-- name: UpsertWebRole :one
INSERT INTO web_role_assignments (personnel_id, role, project_ids, granted_by_personnel_id, created_at, updated_at)
VALUES (@personnel_id, @role, @project_ids, sqlc.narg('granted_by_personnel_id'), @now, @now)
ON CONFLICT (organization_id, personnel_id) DO UPDATE SET
    role                    = EXCLUDED.role,
    project_ids             = EXCLUDED.project_ids,
    granted_by_personnel_id = EXCLUDED.granted_by_personnel_id,
    updated_at              = EXCLUDED.updated_at
RETURNING *;

-- name: DeleteWebRole :execrows
DELETE FROM web_role_assignments WHERE personnel_id = $1;

-- name: PersonnelNameAndNIK :one
SELECT full_name, nik FROM personnels WHERE id = $1;
