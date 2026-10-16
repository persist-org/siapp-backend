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
