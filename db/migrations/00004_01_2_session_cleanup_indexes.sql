-- Phase 01.2: indexes for the daily session cleanup job.
--
-- Every token refresh writes a new session row, so an active user adds a row every 15
-- minutes and the table grows by tens of thousands of rows a day for a few hundred users.
-- The cleanup deletes by revoked_at and by expires_at; without these indexes each delete
-- would scan the whole table.

-- +goose Up
ALTER TABLE sessions
    ADD KEY idx_sessions_revoked_at (revoked_at),
    ADD KEY idx_sessions_expires_at (expires_at);

-- +goose Down
ALTER TABLE sessions
    DROP KEY idx_sessions_revoked_at,
    DROP KEY idx_sessions_expires_at;
