-- Field permission matrix (Phase 03.1). Everything runs inside database.WithTenant.

-- How many of these keys already have a row. EnsureDefaults skips its insert when every
-- (role, key) of the catalog is there, which is every call but the first of an organization.
-- name: CountRolePermissionsOf :one
SELECT count(*) FROM role_permissions WHERE permission = ANY(@permissions::text[]);

-- ⚠ Inserts only the (role, key) pairs that have no row. An existing row is never touched, so an
-- admin's edit (a NONE included) survives, and a key the catalog gained since reaches
-- organizations that were seeded before. The three arrays are the same length and are read
-- side by side, always in the catalog's order, so two first logins inserting at once queue on
-- the same row first and cannot deadlock.
-- name: InsertDefaultRolePermissions :execrows
INSERT INTO role_permissions (team_role, permission, scope, created_at, updated_at)
SELECT d.team_role::team_role, d.permission, d.scope::permission_scope, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@team_roles::text[]) AS team_role, unnest(@permissions::text[]) AS permission,
             unnest(@scopes::text[]) AS scope) AS d
ON CONFLICT (organization_id, team_role, permission) DO NOTHING;

-- name: ListRolePermissions :many
SELECT team_role, permission, scope FROM role_permissions ORDER BY team_role, permission;

-- ⚠ Reads the grants of one role and locks them, in key order, before they are replaced. Two
-- admins saving the same role queue here, and the second one's audit "before" is what the
-- first one saved.
-- name: LockRolePermissionsOfRole :many
SELECT permission, scope FROM role_permissions
WHERE team_role = @team_role
ORDER BY permission
FOR UPDATE;

-- The caller passes only the grants that differ from what it just locked.
-- name: UpsertRolePermissions :execrows
INSERT INTO role_permissions (team_role, permission, scope, created_at, updated_at)
SELECT @team_role::team_role, d.permission, d.scope::permission_scope, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@permissions::text[]) AS permission, unnest(@scopes::text[]) AS scope) AS d
ON CONFLICT (organization_id, team_role, permission) DO UPDATE SET
    scope      = EXCLUDED.scope,
    updated_at = EXCLUDED.updated_at
WHERE role_permissions.scope IS DISTINCT FROM EXCLUDED.scope;
