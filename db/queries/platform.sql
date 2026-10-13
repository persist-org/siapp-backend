-- name: Ping :one
SELECT 1;

-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = $1;

-- Records a tenant the first time Personalia names it, and refreshes its name afterwards.
-- status and first_seen_at are left alone on conflict: suspending a tenant is an operator
-- decision that a login must not undo.
-- name: UpsertOrganizationSeen :one
INSERT INTO organizations (id, name)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE
SET name = EXCLUDED.name,
    updated_at = CASE WHEN organizations.name IS DISTINCT FROM EXCLUDED.name
                      THEN now() ELSE organizations.updated_at END
RETURNING *;

-- Background jobs that work per organization loop over this list, then run each
-- organization in its own WithTenant.
-- name: ListOrganizationIDs :many
SELECT id FROM organizations ORDER BY id;
