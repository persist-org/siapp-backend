-- Exactly one of entity_id and entity_code is set (see the audit_logs_entity_check constraint).
-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_personnel_id, action, entity_type, entity_id, entity_code, metadata)
VALUES (sqlc.narg('actor_personnel_id'), @action, @entity_type, sqlc.narg('entity_id'), sqlc.narg('entity_code'), @metadata);

-- name: ListAuditLogsByEntity :many
SELECT * FROM audit_logs
WHERE entity_type = $1 AND entity_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAuditLogsByCode :many
SELECT * FROM audit_logs
WHERE entity_type = $1 AND entity_code = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAuditLogsByActor :many
SELECT * FROM audit_logs
WHERE actor_personnel_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;
