-- +goose Up

-- Phase 10.3: the DANRU's queue is every SUBMITTED scan of their regu, oldest first. Almost every
-- scan is decided soon after it is made, so a partial index holds only the few that wait and keeps
-- the queue cheap however many scans the table has.
CREATE INDEX patrol_scans_pending_idx ON patrol_scans (organization_id, team_code, occurred_at, id)
    WHERE status = 'SUBMITTED';

-- +goose Down
DROP INDEX patrol_scans_pending_idx;
