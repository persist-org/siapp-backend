-- Everything here runs inside database.WithTenant: organization_id is never a parameter, it
-- comes from the column default that reads the transaction's app.current_org setting.

-- Creates the person on first login and refreshes what Personalia says on every later one.
-- The upsert also takes the person's row lock until the transaction ends (an UPDATE, or an
-- INSERT that a concurrent insert of the same key has to wait for), which is what serialises
-- two logins of one person, see identity.Usecase.Login.
-- name: UpsertPersonnel :one
INSERT INTO personnels (
    personel_user_id, nik, client_id, client_name, full_name, email, phone, photo_url,
    status, synced_at, last_seen_at, created_at, updated_at
) VALUES (
    @personel_user_id, sqlc.narg('nik'), sqlc.narg('client_id'), sqlc.narg('client_name'),
    @full_name, sqlc.narg('email'), sqlc.narg('phone'), sqlc.narg('photo_url'),
    @status, @now, @now, @now, @now
)
ON CONFLICT (organization_id, personel_user_id) DO UPDATE SET
    nik          = EXCLUDED.nik,
    client_id    = EXCLUDED.client_id,
    client_name  = EXCLUDED.client_name,
    full_name    = EXCLUDED.full_name,
    email        = EXCLUDED.email,
    phone        = EXCLUDED.phone,
    photo_url    = EXCLUDED.photo_url,
    status       = EXCLUDED.status,
    synced_at    = EXCLUDED.synced_at,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at   = EXCLUDED.updated_at
RETURNING *;

-- name: GetDeviceBindingForUpdate :one
SELECT * FROM device_bindings WHERE personnel_id = $1 FOR UPDATE;

-- name: CreateDeviceBinding :one
INSERT INTO device_bindings (personnel_id, device_token_hash, bound_at, last_seen_at, created_at, updated_at)
VALUES (@personnel_id, @device_token_hash, @now, @now, @now, @now)
RETURNING *;

-- name: TouchDeviceBinding :exec
UPDATE device_bindings SET last_seen_at = @now, updated_at = @now WHERE id = @id;

-- name: CreateSession :one
INSERT INTO sessions (
    personnel_id, family_id, refresh_token_hash, device_token_hash, remember_me, client,
    expires_at, user_agent, ip, last_used_at, created_at, updated_at
) VALUES (
    @personnel_id, @family_id, @refresh_token_hash, sqlc.narg('device_token_hash'), @remember_me, @client,
    @expires_at, sqlc.narg('user_agent'), sqlc.narg('ip'), @now, @now, @now
)
RETURNING *;

-- name: GetPersonnel :one
SELECT * FROM personnels WHERE id = $1;

-- name: MarkPersonnelInactive :exec
UPDATE personnels SET status = 'INACTIVE', synced_at = @now, updated_at = @now WHERE id = @id;

-- Refresh reads the session without a lock first, only to decide whether Personalia has to
-- be asked (which must not happen while a row lock is held). Everything it learns here is
-- read again under the lock by GetSessionForRefresh.
-- name: PeekSessionForRefresh :one
SELECT s.revoked_at, s.expires_at, p.personel_user_id, p.nik, p.status AS personnel_status, p.synced_at
FROM sessions s
JOIN personnels p ON p.id = s.personnel_id
WHERE s.refresh_token_hash = $1;

-- ⚠ FOR UPDATE OF s: two refreshes with the same token must not both succeed. With a plain
-- read both would see "not revoked" and both would create a new session, leaving two live
-- chains from one token. With the lock the second waits for the first to commit, then reads
-- the row as revoked and is treated as reuse.
-- name: GetSessionForRefresh :one
SELECT s.id, s.personnel_id, s.family_id, s.device_token_hash, s.remember_me, s.client,
       s.expires_at, s.revoked_at,
       p.personel_user_id, p.status AS personnel_status
FROM sessions s
JOIN personnels p ON p.id = s.personnel_id
WHERE s.refresh_token_hash = $1
FOR UPDATE OF s;

-- name: RotateSession :exec
UPDATE sessions
SET revoked_at = @now, replaced_by_id = @replaced_by_id, updated_at = @now
WHERE id = @id;

-- Ends every live session of a login chain. Sessions that were already revoked keep the
-- time they were revoked.
-- name: RevokeSessionFamily :exec
UPDATE sessions SET revoked_at = @now, updated_at = @now
WHERE family_id = @family_id AND revoked_at IS NULL;

-- The sessions of a chain whose access tokens can still be alive (created within the last
-- access-token lifetime), so that they can be put on the revocation list.
-- name: ListRecentSessionIDsByFamily :many
SELECT id FROM sessions WHERE family_id = @family_id AND created_at >= @since;

-- The chain a session belongs to, only if it is the person's own.
-- name: GetSessionFamily :one
SELECT family_id FROM sessions WHERE id = @id AND personnel_id = @personnel_id;

-- Deleted in batches: one huge DELETE would hold locks on a table that every login and
-- refresh writes to.
-- name: DeleteSessionsRevokedBefore :execrows
DELETE FROM sessions WHERE id IN (
    SELECT old.id FROM sessions old WHERE old.revoked_at < @cutoff ORDER BY old.id LIMIT @batch
);

-- name: DeleteSessionsExpiredBefore :execrows
DELETE FROM sessions WHERE id IN (
    SELECT old.id FROM sessions old WHERE old.expires_at < @cutoff ORDER BY old.id LIMIT @batch
);
