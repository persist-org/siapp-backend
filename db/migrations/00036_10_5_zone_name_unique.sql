-- +goose Up

-- A zone is told apart by its name inside one project and type: two "Pos Depan" PATROL zones in
-- the same project would print two stickers a guard cannot tell apart. lower(name) makes the check
-- case-insensitive; a deleted zone keeps its row (scans point at it) but frees its name.
-- If this fails on an existing database, rename the duplicate zones first.
CREATE UNIQUE INDEX patrol_zones_name_uq
    ON patrol_zones (organization_id, project_code, type, lower(name))
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX patrol_zones_name_uq;
