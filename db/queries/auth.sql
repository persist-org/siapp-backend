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

-- name: GetSessionForRefresh :one
-- ⚠ FOR UPDATE OF s locks the session row until the transaction ends, so two requests
-- presenting the same refresh token are serialised: the second one waits, then sees the
-- row already revoked by the first. Without the lock both could read "not revoked" and
-- both rotate, handing out two valid token chains from one token.
-- The user's current role and status come along: a refresh reflects changes made since
-- login, and a disabled or deleted user cannot refresh.
SELECT s.id, s.user_id, s.family_id, s.remember_me, s.expires_at, s.revoked_at,
       u.system_role, u.status AS user_status, u.deleted_at AS user_deleted_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.refresh_token_hash = sqlc.arg('refresh_token_hash')
FOR UPDATE OF s;

-- name: RotateSession :exec
-- Retires a session that has been exchanged for a new one.
UPDATE sessions
SET revoked_at = sqlc.arg('now'), replaced_by_id = sqlc.arg('replaced_by_id')
WHERE id = sqlc.arg('id');

-- name: RevokeSessionFamily :exec
UPDATE sessions
SET revoked_at = sqlc.arg('now')
WHERE family_id = sqlc.arg('family_id') AND revoked_at IS NULL;

-- name: RevokeSession :exec
-- The user_id condition means a token can only ever end its owner's session.
UPDATE sessions
SET revoked_at = sqlc.arg('now')
WHERE id = sqlc.arg('id') AND user_id = sqlc.arg('user_id') AND revoked_at IS NULL;

-- name: RevokeUserSessions :execrows
UPDATE sessions
SET revoked_at = sqlc.arg('now')
WHERE user_id = sqlc.arg('user_id') AND revoked_at IS NULL;

-- name: DeleteSessionsRevokedBefore :execrows
-- Deleted in batches (the caller loops) so one run never holds a huge lock.
DELETE FROM sessions
WHERE revoked_at IS NOT NULL AND revoked_at < sqlc.arg('cutoff')
LIMIT ?;

-- name: DeleteSessionsExpiredBefore :execrows
DELETE FROM sessions
WHERE expires_at < sqlc.arg('cutoff')
LIMIT ?;

-- name: ListRecentSessionIDsByUser :many
-- Sessions whose access token can still be alive. Access tokens are issued only when a
-- session is created (login or refresh), at last_used_at, and live 15 minutes, so a token
-- that is still valid always belongs to a session created within that window, whether or
-- not the session has since been rotated or revoked. FOR UPDATE locks the range so a login
-- racing with logout-all cannot slip a new session in between this read and the revoke.
SELECT id FROM sessions
WHERE user_id = sqlc.arg('user_id') AND last_used_at > sqlc.arg('since')
FOR UPDATE;

-- name: ListRecentSessionIDsByFamily :many
-- Same, for one login chain: what reuse detection has to cut off.
SELECT id FROM sessions
WHERE family_id = sqlc.arg('family_id') AND last_used_at > sqlc.arg('since');

-- name: ListRecentSessionIDsOfSessionFamily :many
-- Same, for the login chain of one session, for logout. The user_id condition keeps it to
-- the caller's own sessions.
SELECT s2.id
FROM sessions s1
JOIN sessions s2 ON s2.family_id = s1.family_id
WHERE s1.id = sqlc.arg('session_id') AND s1.user_id = sqlc.arg('user_id')
  AND s2.last_used_at > sqlc.arg('since');

-- name: GetUserByID :one
SELECT * FROM users WHERE id = sqlc.arg('id') AND deleted_at IS NULL;

-- name: GetUserByIDForUpdate :one
-- Locks the row: a change to a user reads it, decides, and writes inside one transaction,
-- so two admins editing the same user cannot overwrite each other unnoticed.
SELECT * FROM users WHERE id = sqlc.arg('id') AND deleted_at IS NULL FOR UPDATE;

-- name: GetActiveUserByEmail :one
-- Only an ACTIVE user can ask for a password reset link.
SELECT * FROM users
WHERE email = sqlc.arg('email') AND status = 'ACTIVE' AND deleted_at IS NULL;

-- name: ListUsers :many
-- Newest first. Keyset pagination in the form the README requires: MySQL uses the index
-- reliably only with the expanded OR, not with a row-constructor comparison.
-- search is a LIKE pattern built by the caller (wildcards in user input already escaped).
SELECT * FROM users
WHERE deleted_at IS NULL
  AND (sqlc.narg('status') IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('search') IS NULL
       OR full_name LIKE sqlc.narg('search')
       OR email LIKE sqlc.narg('search')
       OR username LIKE sqlc.narg('search'))
  AND (sqlc.narg('cursor_time') IS NULL
       OR created_at < sqlc.narg('cursor_time')
       OR (created_at = sqlc.narg('cursor_time') AND id < sqlc.narg('cursor_id')))
ORDER BY created_at DESC, id DESC
LIMIT ?;

-- name: UpdateUser :exec
UPDATE users
SET full_name = sqlc.arg('full_name'),
    phone = sqlc.narg('phone'),
    birth_date = sqlc.narg('birth_date'),
    username = sqlc.narg('username'),
    system_role = sqlc.arg('system_role')
WHERE id = sqlc.arg('id');

-- name: SetUserStatus :exec
UPDATE users SET status = sqlc.arg('status') WHERE id = sqlc.arg('id');

-- name: UpdateUserPassword :exec
-- A new password also clears the lockout: a user who was locked out and used the reset link
-- has proven they control the mailbox, and should not have to wait out the lock.
UPDATE users
SET password_hash = sqlc.arg('password_hash'),
    failed_login_count = 0,
    locked_until = NULL
WHERE id = sqlc.arg('id');

-- name: InvalidateUnusedResetTokens :exec
-- Only the newest link may work: asking again replaces the earlier one.
UPDATE password_reset_tokens
SET used_at = sqlc.arg('now')
WHERE user_id = sqlc.arg('user_id') AND used_at IS NULL;

-- name: CreateResetToken :exec
INSERT INTO password_reset_tokens (id, user_id, token_hash, expires_at)
VALUES (sqlc.arg('id'), sqlc.arg('user_id'), sqlc.arg('token_hash'), sqlc.arg('expires_at'));

-- name: GetResetTokenForUpdate :one
-- ⚠ FOR UPDATE OF t: two requests with the same link are serialised, so the link works once.
SELECT t.id, t.user_id, t.expires_at, t.used_at,
       u.email, u.username, u.status AS user_status, u.deleted_at AS user_deleted_at
FROM password_reset_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = sqlc.arg('token_hash')
FOR UPDATE OF t;

-- name: MarkResetTokenUsed :exec
UPDATE password_reset_tokens SET used_at = sqlc.arg('now') WHERE id = sqlc.arg('id');
