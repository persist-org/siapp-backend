-- App links (Phase 06.2): the home-screen buttons that open another app. Everything runs inside
-- database.WithTenant. A delete is a soft delete that also deactivates, so a delta pull can send
-- it as a tombstone.

-- name: ListAppLinks :many
SELECT * FROM app_links
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(app_links.name), lower(sqlc.narg('q'))) > 0)
ORDER BY sort_order, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountAppLinks :one
SELECT count(*) FROM app_links
WHERE deleted_at IS NULL
  AND (sqlc.narg('active')::boolean IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('q')::text IS NULL OR strpos(lower(app_links.name), lower(sqlc.narg('q'))) > 0);

-- name: GetAppLink :one
SELECT * FROM app_links WHERE id = @id AND deleted_at IS NULL;

-- name: GetAppLinkForUpdate :one
SELECT * FROM app_links WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- A link created without a sort order goes last.
-- name: InsertAppLink :one
INSERT INTO app_links (name, icon_key, android_package, ios_url_scheme, url, sort_order, active, created_at, updated_at)
VALUES (@name, @icon_key, sqlc.narg('android_package'), sqlc.narg('ios_url_scheme'), sqlc.narg('url'),
        COALESCE(sqlc.narg('sort_order')::int, (SELECT COALESCE(max(a.sort_order) + 1, 0) FROM app_links a)),
        @active, @now, @now)
RETURNING *;

-- Writes every editable column (the caller has worked out the new row).
-- name: UpdateAppLink :one
UPDATE app_links SET name = @name, icon_key = @icon_key, android_package = sqlc.narg('android_package'),
    ios_url_scheme = sqlc.narg('ios_url_scheme'), url = sqlc.narg('url'), sort_order = @sort_order,
    active = @active, updated_at = @now
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteAppLink :execrows
UPDATE app_links SET active = false, deleted_at = @now, updated_at = @now
WHERE id = @id AND deleted_at IS NULL;

-- name: AppLinkNameClash :one
SELECT EXISTS (
    SELECT 1 FROM app_links WHERE id <> @id AND deleted_at IS NULL AND lower(app_links.name) = lower(@name::text)
)::boolean;

-- name: ListActiveAppLinks :many
SELECT * FROM app_links WHERE active AND deleted_at IS NULL ORDER BY sort_order, name, id;

-- name: ListAppLinkChanges :many
SELECT * FROM app_links WHERE updated_at > @after::timestamptz ORDER BY updated_at, id;

-- The two placeholders go in only while the organization has no app link at all, counting
-- inactive and deleted ones; they are inactive until the real links exist (DL-16).
-- name: InsertDefaultAppLinks :execrows
INSERT INTO app_links (name, icon_key, sort_order, active, created_at, updated_at)
SELECT d.name, d.icon_key, d.ord, false, @now::timestamptz, @now::timestamptz
FROM (SELECT unnest(@names::text[]) AS name, unnest(@icon_keys::text[]) AS icon_key,
             unnest(@orders::int[]) AS ord) AS d
WHERE NOT EXISTS (SELECT 1 FROM app_links)
ON CONFLICT DO NOTHING;
