-- name: Ping :one
SELECT 1;

-- name: InsertAuditLog :exec
-- created_at is optional: services pass clock.Now(), and NULL falls back to the database
-- clock (always UTC, see the DSN rules).
INSERT INTO audit_logs (id, actor_id, action, entity_type, entity_id, metadata, created_at)
VALUES (
    sqlc.arg('id'),
    sqlc.narg('actor_id'),
    sqlc.arg('action'),
    sqlc.arg('entity_type'),
    sqlc.arg('entity_id'),
    sqlc.narg('metadata'),
    COALESCE(CAST(sqlc.narg('created_at') AS DATETIME(3)), CURRENT_TIMESTAMP(3))
);
