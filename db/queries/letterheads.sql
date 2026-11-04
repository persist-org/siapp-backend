-- Letterheads (Phase 06.2). Everything runs inside database.WithTenant. A letterhead is deleted
-- for real: nothing syncs it to the apps. A module that points at one (exports, module 14)
-- registers a usage check, and the delete then answers MASTER_IN_USE.

-- name: ListLetterheads :many
SELECT * FROM letterheads
WHERE (sqlc.narg('q')::text IS NULL OR strpos(lower(letterheads.name), lower(sqlc.narg('q'))) > 0)
ORDER BY is_default DESC, name, id
LIMIT @page_limit OFFSET @page_offset;

-- name: CountLetterheads :one
SELECT count(*) FROM letterheads
WHERE (sqlc.narg('q')::text IS NULL OR strpos(lower(letterheads.name), lower(sqlc.narg('q'))) > 0);

-- name: GetLetterhead :one
SELECT * FROM letterheads WHERE id = @id;

-- name: GetLetterheadForUpdate :one
SELECT * FROM letterheads WHERE id = @id FOR UPDATE;

-- name: GetDefaultLetterhead :one
SELECT * FROM letterheads WHERE is_default;

-- name: InsertLetterhead :one
INSERT INTO letterheads (name, header_text, footer_text, is_default, created_at, updated_at)
VALUES (@name, sqlc.narg('header_text'), sqlc.narg('footer_text'), @is_default, @now, @now)
RETURNING *;

-- Writes every editable column (the caller has worked out the new row).
-- name: UpdateLetterhead :one
UPDATE letterheads SET name = @name, header_text = sqlc.narg('header_text'), footer_text = sqlc.narg('footer_text'),
    logo_media_id = sqlc.narg('logo_media_id'), is_default = @is_default, updated_at = @now
WHERE id = @id
RETURNING *;

-- The logo of a new letterhead is set after the insert, because attaching a photo needs the
-- letterhead's id.
-- name: SetLetterheadLogo :one
UPDATE letterheads SET logo_media_id = sqlc.narg('logo_media_id') WHERE id = @id
RETURNING *;

-- Makes room for a new default: the old one stops being it, in the same transaction.
-- name: ClearOtherDefaultLetterheads :exec
UPDATE letterheads SET is_default = false, updated_at = @now WHERE is_default AND id <> @id;

-- name: DeleteLetterhead :execrows
DELETE FROM letterheads WHERE id = @id;

-- name: LetterheadNameClash :one
SELECT EXISTS (
    SELECT 1 FROM letterheads WHERE id <> @id AND lower(letterheads.name) = lower(@name::text)
)::boolean;
