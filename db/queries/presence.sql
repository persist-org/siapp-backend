-- Everything here runs inside database.WithTenant.

-- Persists the moment a person was last seen. It only moves forward, so a late write from
-- one request never undoes a newer one, and it leaves updated_at alone: being seen is not a
-- change to the person's data.
-- name: TouchPersonnelLastSeen :exec
UPDATE personnels SET last_seen_at = @seen_at
WHERE id = @id AND (last_seen_at IS NULL OR last_seen_at < @seen_at);

-- The persisted last-seen time of some people, for those Redis no longer remembers.
-- name: ListPersonnelLastSeen :many
SELECT id, last_seen_at FROM personnels WHERE id = ANY(@ids::bigint[]);
