-- All statements run inside WithTenant: RLS limits them to the current organization.

-- Claims a key by inserting its in-flight row. No row comes back when someone else holds it.
-- name: ClaimIdempotencyKey :one
INSERT INTO idempotency_keys (actor_personnel_id, key, method, path, request_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE actor_personnel_id = $1 AND key = $2;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys
SET status_code = $3, response_body = $4
WHERE actor_personnel_id = $1 AND key = $2 AND status_code IS NULL;

-- Drops an in-flight claim: the response is not stored (5xx, 409, 429, a panic), so a
-- retry must run again.
-- name: ReleaseIdempotencyKey :exec
DELETE FROM idempotency_keys
WHERE actor_personnel_id = $1 AND key = $2 AND status_code IS NULL;

-- Drops an in-flight claim that is older than the given number of seconds: its owner died.
-- name: ReleaseStaleIdempotencyKey :execrows
DELETE FROM idempotency_keys
WHERE actor_personnel_id = $1 AND key = $2 AND status_code IS NULL
  AND created_at < now() - make_interval(secs => sqlc.arg('stale_after_seconds')::float8);

-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM idempotency_keys
WHERE created_at < now() - make_interval(secs => sqlc.arg('older_than_seconds')::float8);
