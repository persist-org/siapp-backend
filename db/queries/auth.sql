-- name: GetUserByIdentifier :one
-- The caller passes the same identifier as both arguments: an email (lowercased) or a
-- username. Usernames may not contain '@', so the two can never collide. sqlc needs two
-- arguments because the columns differ in nullability.
SELECT * FROM users
WHERE (email = sqlc.arg('email') OR username = sqlc.arg('username'))
  AND deleted_at IS NULL
LIMIT 1;

-- name: CountAdmins :one
SELECT COUNT(*) FROM users WHERE system_role = 'ADMIN' AND deleted_at IS NULL;

-- name: CreateUser :exec
INSERT INTO users (id, email, username, full_name, phone, birth_date, system_role, status, password_hash)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('email'),
    sqlc.narg('username'),
    sqlc.arg('full_name'),
    sqlc.narg('phone'),
    sqlc.narg('birth_date'),
    sqlc.arg('system_role'),
    sqlc.arg('status'),
    sqlc.arg('password_hash')
);

-- name: RecordFailedLogin :exec
-- ⚠ One atomic UPDATE, so concurrent wrong guesses are all counted: a read-modify-write
-- in Go would let five parallel attempts each read 0 and write 1.
-- MySQL evaluates SET assignments left to right and later ones see the values already
-- assigned, so in the second assignment failed_login_count is the incremented value.
-- Do not "fix" it to failed_login_count + 1.
UPDATE users
SET failed_login_count = failed_login_count + 1,
    locked_until = IF(failed_login_count >= sqlc.arg('max_failures'), CAST(sqlc.arg('locked_until') AS DATETIME(3)), locked_until)
WHERE id = sqlc.arg('id');

-- name: RecordSuccessfulLogin :exec
UPDATE users
SET failed_login_count = 0,
    locked_until = NULL,
    last_login_at = sqlc.arg('now')
WHERE id = sqlc.arg('id');

-- name: CreateSession :exec
INSERT INTO sessions (id, user_id, family_id, refresh_token_hash, remember_me, expires_at, user_agent, ip, last_used_at)
VALUES (
    sqlc.arg('id'),
    sqlc.arg('user_id'),
    sqlc.arg('family_id'),
    sqlc.arg('refresh_token_hash'),
    sqlc.arg('remember_me'),
    sqlc.arg('expires_at'),
    sqlc.narg('user_agent'),
    sqlc.narg('ip'),
    sqlc.arg('last_used_at')
);
