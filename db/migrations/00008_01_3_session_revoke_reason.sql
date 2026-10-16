-- +goose Up

-- Why a session was ended, so that a refresh with its token can say so.
--
-- Without it every revoked session looked the same, and every refresh with a revoked token
-- was treated as a replayed one: the old phone after an admin's device reset, a person the
-- resync deactivated, and a refresh after logout all got AUTH_REFRESH_REUSED ("the token was
-- used twice", which reads as theft) instead of AUTH_DEVICE_CHANGED, AUTH_ACCOUNT_DISABLED or
-- a plain "sign in again". Only ROTATED means the token was already exchanged for a newer one,
-- so only ROTATED triggers reuse detection.
CREATE TYPE session_revoke_reason AS ENUM (
    'ROTATED',          -- exchanged for the next session of the chain by a refresh
    'LOGOUT',           -- the person signed out
    'REUSE_DETECTED',   -- the chain was ended because a rotated token was replayed
    'DEVICE_RESET',     -- an admin freed the person's phone slot
    'DEVICE_CHANGED',   -- a refresh found the session's phone is no longer the bound one
    'ACCOUNT_DISABLED', -- the person is inactive in Personalia (refresh or daily resync)
    'WEB_ROLE_REVOKED'  -- a WEB session whose person no longer has a web role
);

ALTER TABLE sessions ADD COLUMN revoke_reason session_revoke_reason;

-- Backfill the sessions revoked before this migration. A row with a successor was rotated;
-- for the rest the precise cause is unknown, and LOGOUT ("sign in again") is the neutral answer.
--
-- FORCE ROW LEVEL SECURITY applies the tenant policy to the table owner too, and a migration
-- has no tenant: the UPDATE would see no rows. FORCE is lifted for this one statement and put
-- back before the migration's transaction commits, so no other session ever sees the table
-- without it (the guard test in 00.2 checks it is set).
ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY;
UPDATE sessions
SET revoke_reason = CASE WHEN replaced_by_id IS NOT NULL THEN 'ROTATED' ELSE 'LOGOUT' END::session_revoke_reason
WHERE revoked_at IS NOT NULL;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

-- Every revoke must say why, and only a revoked session has a reason.
ALTER TABLE sessions ADD CONSTRAINT sessions_revoke_reason_check
    CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL));

-- +goose Down
ALTER TABLE sessions DROP CONSTRAINT sessions_revoke_reason_check;
ALTER TABLE sessions DROP COLUMN revoke_reason;
DROP TYPE session_revoke_reason;
