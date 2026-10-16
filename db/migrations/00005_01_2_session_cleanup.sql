-- +goose Up

-- A refresh retires the old session and points it at its successor (replaced_by_id). The
-- daily cleanup deletes sessions by age, in batches, so a successor can be deleted while an
-- older session in the chain is still waiting for its own batch. With the default
-- (NO ACTION) that delete would fail on the foreign key and the cleanup would stall; the
-- link is only bookkeeping, so it is cleared instead.
ALTER TABLE sessions DROP CONSTRAINT sessions_replaced_by_id_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_replaced_by_id_fkey
    FOREIGN KEY (replaced_by_id) REFERENCES sessions(id) ON DELETE SET NULL;

-- Refresh looks a session up by the hash alone (UNIQUE), but the cleanup scans by age.
CREATE INDEX sessions_revoked_idx ON sessions (organization_id, revoked_at) WHERE revoked_at IS NOT NULL;

-- +goose Down
DROP INDEX sessions_revoked_idx;
ALTER TABLE sessions DROP CONSTRAINT sessions_replaced_by_id_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_replaced_by_id_fkey
    FOREIGN KEY (replaced_by_id) REFERENCES sessions(id);
