-- +goose Up

-- Phase 04.2 review: the cleanup job asks for the photos nobody attached, oldest first
-- (attached_entity_id IS NULL AND created_at < cutoff ORDER BY created_at, id). The index of
-- 00015, (organization_id, status, created_at), cannot serve that: the query has no status, and
-- Postgres 16 has no skip scan, so it could only narrow to the organization and then read every
-- row of it, attached ones included, which only ever grow. This partial index holds the
-- unattached rows alone, in the job's order, so the hourly job reads what it may delete and
-- nothing else. Nothing else read the old index.
DROP INDEX media_status_created_idx;
CREATE INDEX media_unattached_created_idx ON media (organization_id, created_at, id)
    WHERE attached_entity_id IS NULL;

-- +goose Down
DROP INDEX media_unattached_created_idx;
CREATE INDEX media_status_created_idx ON media (organization_id, status, created_at);
