-- +goose Up

-- Phase 03.1: what each field role (DANRU, MEMBER) may do, as data per organization.
--
-- A grant is (team role, permission, scope). The permission keys are the catalog in
-- pkg/access/catalog.go; the table does not repeat them in a CHECK, because a new feature
-- adds a key in Go and must not need a migration.
--
-- ⚠ NONE is a stored scope, not an absent row. "This role may not do this" has to survive a
-- re-seed: the seed only inserts rows that are missing (ON CONFLICT DO NOTHING), so if "no
-- grant" were the absence of a row, an admin who took a permission away would get it back at
-- the next login or seed. With NONE every (role, key) has a row once the organization is
-- seeded, a row that is missing means "not seeded yet", and readers treat a missing row as
-- no grant (fail closed).
CREATE TYPE permission_scope AS ENUM ('NONE', 'SELF', 'TEAM', 'PROJECT');

CREATE TABLE role_permissions (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    team_role       team_role NOT NULL,
    permission      VARCHAR(64) NOT NULL,
    scope           permission_scope NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, team_role, permission),
    -- lower-case words joined by dots and underscores: attendance.leave.approve
    CONSTRAINT role_permissions_key_check CHECK (permission ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$')
);

ALTER TABLE role_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_permissions FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON role_permissions
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON role_permissions TO siapp_app;

-- +goose Down
DROP TABLE role_permissions;
DROP TYPE permission_scope;
