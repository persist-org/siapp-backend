-- Uploaded photos (Phase 04.1). Everything runs inside database.WithTenant, so row-level
-- security keeps another organization's media invisible: a lookup of a foreign id finds no row.

-- The id is taken BEFORE the insert because the storage key contains it. nextval() is exactly
-- what the identity column calls, so ids stay one sequence; a number lost to a rollback is
-- only a gap.
-- name: NextMediaID :one
SELECT nextval(pg_get_serial_sequence('media', 'id'))::bigint AS id;

-- name: CreateMedia :one
INSERT INTO media (id, owner_personnel_id, purpose, content_type, size_bytes, storage_key,
                   captured_at, capture_latitude, capture_longitude, capture_source,
                   created_at, updated_at)
OVERRIDING SYSTEM VALUE
VALUES (@id, @owner_personnel_id, @purpose, @content_type, @size_bytes, @storage_key,
        sqlc.narg('captured_at'), sqlc.narg('capture_latitude'), sqlc.narg('capture_longitude'),
        @capture_source, @now, @now)
RETURNING *;

-- name: GetMedia :one
SELECT * FROM media WHERE id = $1;

-- The photos of a record, or the rows behind an Attach that came up short. Several ids in one
-- statement; an id that is not visible (another organization's, or nonexistent) is simply
-- absent from the answer.
-- name: GetMediaByIDs :many
SELECT * FROM media WHERE id = ANY(@ids::bigint[]);

-- Attaches every item in ONE statement. The conditions are the rule: the photo is the owner's,
-- it was confirmed, it is for this kind of record, and nobody attached it yet. The caller
-- compares the affected row count with the number of ids it sent; a short count means at
-- least one condition failed, and it re-reads to find out which.
-- name: AttachMedia :execrows
UPDATE media SET attached_entity_type = @entity_type, attached_entity_id = @entity_id, updated_at = @now
WHERE id = ANY(@ids::bigint[])
  AND owner_personnel_id = @owner_personnel_id
  AND status = 'UPLOADED'
  AND purpose = @purpose
  AND attached_entity_id IS NULL;

-- Photos nobody attached, a day after they were registered: PENDING ones that were never
-- uploaded or confirmed, and UPLOADED ones no record claimed. The rows are LOCKED, skipping
-- those another transaction holds: an Attach that runs at the same moment waits for the
-- delete, then finds no row, instead of attaching a photo whose file is already gone.
-- name: ListStaleMediaForUpdate :many
SELECT id, storage_key FROM media
WHERE attached_entity_id IS NULL AND created_at < @cutoff
ORDER BY created_at, id
LIMIT @batch
FOR UPDATE SKIP LOCKED;

-- The attached_entity_id condition repeats the rule that attached media is never deleted, so
-- it holds even if a caller passes the wrong ids.
-- name: DeleteMediaByIDs :execrows
DELETE FROM media WHERE id = ANY(@ids::bigint[]) AND attached_entity_id IS NULL;

-- PENDING to UPLOADED, by the owner only. The conditions are in the statement, not in Go, so
-- two confirmations racing cannot both succeed: the second finds no PENDING row and re-reads.
-- name: MarkMediaUploaded :one
UPDATE media SET status = 'UPLOADED', updated_at = @now
WHERE id = @id AND owner_personnel_id = @owner_personnel_id AND status = 'PENDING'
RETURNING *;
