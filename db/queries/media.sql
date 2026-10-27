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

-- PENDING to UPLOADED, by the owner only. The conditions are in the statement, not in Go, so
-- two confirmations racing cannot both succeed: the second finds no PENDING row and re-reads.
-- name: MarkMediaUploaded :one
UPDATE media SET status = 'UPLOADED', updated_at = @now
WHERE id = @id AND owner_personnel_id = @owner_personnel_id AND status = 'PENDING'
RETURNING *;
