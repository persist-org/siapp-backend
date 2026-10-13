-- name: InsertAuditLog :exec
INSERT INTO audit_logs (actor_personnel_id, action, entity_type, entity_id, metadata)
VALUES (sqlc.narg('actor_personnel_id'), $1, $2, $3, $4);

-- name: ListAuditLogsByEntity :many
SELECT * FROM audit_logs
WHERE entity_type = $1 AND entity_id = $2
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: ListAuditLogsByActor :many
SELECT * FROM audit_logs
WHERE actor_personnel_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;
