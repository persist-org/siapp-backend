-- Everything here runs inside database.WithTenant: organization_id is never a parameter, it
-- comes from the column default that reads the transaction's app.current_org setting.

-- Creates the person on first login and refreshes what Personalia says on every later one.
-- The upsert also takes the person's row lock until the transaction ends (an UPDATE, or an
-- INSERT that a concurrent insert of the same key has to wait for), which is what serialises
-- two logins of one person, see identity.Usecase.Login.
--
-- seen_at is the person's own activity (a login or a refresh) and feeds last_seen_at, the
-- billing active-head clock. It is NULL when SiAPP fetches someone who did nothing themselves
-- (People.Ensure): the clock is then left as it was, so a person added to a team is not
-- counted as active.
-- name: UpsertPersonnel :one
INSERT INTO personnels (
    personel_user_id, nik, company_id, company_name, client_id, client_name, full_name, email, phone, photo_url,
    status, synced_at, last_seen_at, created_at, updated_at
) VALUES (
    @personel_user_id, sqlc.narg('nik'), sqlc.narg('company_id'), sqlc.narg('company_name'),
    sqlc.narg('client_id'), sqlc.narg('client_name'),
    @full_name, sqlc.narg('email'), sqlc.narg('phone'), sqlc.narg('photo_url'),
    @status, @now, sqlc.narg('seen_at'), @now, @now
)
ON CONFLICT (organization_id, personel_user_id) DO UPDATE SET
    nik          = EXCLUDED.nik,
    company_id   = EXCLUDED.company_id,
    company_name = EXCLUDED.company_name,
    client_id    = EXCLUDED.client_id,
    client_name  = EXCLUDED.client_name,
    full_name    = EXCLUDED.full_name,
    email        = EXCLUDED.email,
    phone        = EXCLUDED.phone,
    photo_url    = EXCLUDED.photo_url,
    status       = EXCLUDED.status,
    synced_at    = EXCLUDED.synced_at,
    last_seen_at = COALESCE(EXCLUDED.last_seen_at, personnels.last_seen_at),
    updated_at   = EXCLUDED.updated_at
RETURNING *;

-- The resync's re-check under the per-person lock: what it read before asking Personalia may
-- have changed since (a login, a refresh, another deactivation).
-- name: LockPersonnelForSync :one
SELECT synced_at, status FROM personnels WHERE id = $1 FOR UPDATE;

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

-- ⚠ The person's row is the per-person lock. Every transaction that changes a person's
-- sessions, phone binding or devices takes it FIRST (login takes it through its upsert), so
-- they all queue in the same order and can neither deadlock nor miss each other's writes:
-- a logout that waits here for a refresh to commit then sees the session that refresh made.
-- name: LockPersonnel :one
SELECT id FROM personnels WHERE id = $1 FOR UPDATE;

-- Whose session a refresh token belongs to, read before any lock so that the person can be
-- locked first (see LockPersonnel).
-- name: GetSessionPersonnel :one
SELECT personnel_id FROM sessions WHERE refresh_token_hash = $1;

-- A NIK belongs to one Personalia account at a time. When Personalia hands a NIK to another
-- account (a re-hire under a new user, a corrected record), the stale row lets go of it, so
-- the unique (organization, nik) index does not turn the new owner's login into an error.
-- name: ReleaseNIK :exec
UPDATE personnels SET nik = NULL, updated_at = @now
WHERE nik = @nik AND personel_user_id <> @personel_user_id;

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
       s.expires_at, s.revoked_at, s.revoke_reason,
       p.personel_user_id, p.status AS personnel_status
FROM sessions s
JOIN personnels p ON p.id = s.personnel_id
WHERE s.refresh_token_hash = $1
FOR UPDATE OF s;

-- name: RotateSession :exec
UPDATE sessions
SET revoked_at = @now, revoke_reason = 'ROTATED', replaced_by_id = @replaced_by_id, updated_at = @now
WHERE id = @id;

-- Ends every live session of a login chain. Sessions that were already revoked keep the
-- time they were revoked.
-- name: RevokeSessionFamily :exec
UPDATE sessions SET revoked_at = @now, revoke_reason = @reason, updated_at = @now
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

-- name: GetPersonnelByNIK :one
SELECT * FROM personnels WHERE nik = $1;

-- Push-token devices ---------------------------------------------------------------------

-- A push token belongs to one device: whoever else holds it loses it first.
-- name: DeleteDevicesByPushTokenExceptSession :exec
DELETE FROM devices WHERE push_token = @push_token AND session_id <> @session_id;

-- name: UpsertDevice :one
INSERT INTO devices (personnel_id, session_id, platform, push_token, app_version, last_seen_at, created_at, updated_at)
VALUES (@personnel_id, @session_id, @platform, @push_token, sqlc.narg('app_version'), @now, @now, @now)
ON CONFLICT (session_id) DO UPDATE SET
    platform     = EXCLUDED.platform,
    push_token   = EXCLUDED.push_token,
    app_version  = EXCLUDED.app_version,
    last_seen_at = EXCLUDED.last_seen_at,
    updated_at   = EXCLUDED.updated_at
RETURNING *;

-- The live session of the login chain that a session belongs to. The access token of a
-- request can name an older session of its chain (it was issued before a refresh), and the
-- device must hang on the session that is alive.
-- name: GetLiveSessionOfChain :one
SELECT live.id
FROM sessions s
JOIN sessions live ON live.family_id = s.family_id AND live.revoked_at IS NULL
WHERE s.id = @session_id AND s.personnel_id = @personnel_id;

-- name: DeleteChainDeviceByPushToken :execrows
DELETE FROM devices WHERE push_token = @push_token AND session_id IN (
    SELECT x.id FROM sessions x WHERE x.family_id = (
        SELECT f.family_id FROM sessions f WHERE f.id = @session_id AND f.personnel_id = @personnel_id
    )
);

-- A device follows its login chain: a refresh moves it to the new session.
-- name: MoveDevice :exec
UPDATE devices SET session_id = @new_session_id, updated_at = @now WHERE session_id = @old_session_id;

-- A device never outlives its login: whatever ends a chain deletes the chain devices.
-- name: DeleteDevicesOfFamily :exec
DELETE FROM devices WHERE session_id IN (SELECT id FROM sessions WHERE family_id = $1);

-- Device reset (admin) --------------------------------------------------------------------

-- name: DeleteDeviceBinding :execrows
DELETE FROM device_bindings WHERE personnel_id = $1;

-- name: ListRecentMobileSessionIDs :many
SELECT id FROM sessions WHERE personnel_id = @personnel_id AND client = 'MOBILE' AND created_at >= @since;

-- name: RevokeMobileSessions :exec
UPDATE sessions SET revoked_at = @now, revoke_reason = 'DEVICE_RESET', updated_at = @now
WHERE personnel_id = @personnel_id AND client = 'MOBILE' AND revoked_at IS NULL;

-- name: DeleteMobileDevices :exec
DELETE FROM devices WHERE session_id IN (SELECT s.id FROM sessions s WHERE s.personnel_id = $1 AND s.client = 'MOBILE');

-- People sync -----------------------------------------------------------------------------

-- Active people with a NIK whose cached copy is older than the cutoff, in id order so that a
-- batch can continue after the last id it saw.
-- name: ListStalePersonnel :many
SELECT * FROM personnels
WHERE status = 'ACTIVE' AND nik IS NOT NULL AND synced_at < @cutoff AND id > @after_id
ORDER BY id
LIMIT @batch;

-- name: ListRecentSessionIDsOfPersonnel :many
SELECT id FROM sessions WHERE personnel_id = @personnel_id AND created_at >= @since;

-- Ends every session of a person whom the resync found no longer active in Personalia.
-- name: RevokeSessionsOfPersonnel :exec
UPDATE sessions SET revoked_at = @now, revoke_reason = 'ACCOUNT_DISABLED', updated_at = @now
WHERE personnel_id = @personnel_id AND revoked_at IS NULL;

-- name: DeleteDevicesOfPersonnel :exec
DELETE FROM devices WHERE personnel_id = $1;

-- Refreshes the cached copy from Personalia without counting as activity: last_seen_at (the
-- billing active-head clock) is left alone, unlike UpsertPersonnel which a login or a
-- refresh uses.
-- name: RefreshPersonnel :exec
UPDATE personnels SET
    nik = sqlc.narg('nik'), company_id = sqlc.narg('company_id'), company_name = sqlc.narg('company_name'),
    client_id = sqlc.narg('client_id'), client_name = sqlc.narg('client_name'),
    full_name = @full_name, email = sqlc.narg('email'), phone = sqlc.narg('phone'),
    photo_url = sqlc.narg('photo_url'), status = 'ACTIVE', synced_at = @now, updated_at = @now
WHERE id = @id;
