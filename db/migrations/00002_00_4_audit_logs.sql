-- Phase 00.4: the audit trail for sensitive writes (see internal/platform/audit).

-- +goose Up
CREATE TABLE audit_logs (
    id          CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- ⚠ No foreign key on actor_id (or entity_id): an audit row must outlive the rows it
    -- describes. A FK would either block deleting a user who has history, or cascade the
    -- delete and erase the evidence of what that user did. NULL actor_id means the system
    -- acted (a job, a seed), not a person.
    actor_id    CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    -- Dotted, module-first verb, e.g. 'attendance.absence.create'.
    action      VARCHAR(100) NOT NULL,
    entity_type VARCHAR(50)  NOT NULL,
    entity_id   CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Facts needed to understand the change. Never passwords, tokens or hashes.
    metadata    JSON NULL,
    -- Audit rows are append-only, so unlike other tables there is no updated_at.
    created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY idx_audit_logs_entity (entity_type, entity_id, created_at),
    KEY idx_audit_logs_actor (actor_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE audit_logs;
