-- +goose Up

-- Phase 02.1: the group is one tenant, regions exist, and a web admin is scoped to a region.
--
-- ⚠ Existing data. Before this phase every employing company (PT) was its own organization,
-- and now KPI, IDS and PCD share one (D17). Rows of the old per-PT organizations are NOT
-- moved: nothing is live yet, so a development database is reset (make reset-db) instead of
-- migrated. The only data this migration touches itself is the PROJECT_ADMIN assignments,
-- which cannot be translated (a project list is not a region) and are deleted.

-- The person's own PT, shown next to their name. It is information only: the tenant is the
-- group, no longer the PT (client_id/client_name beside it work the same way).
ALTER TABLE personnels
    ADD COLUMN company_id   BIGINT,
    ADD COLUMN company_name TEXT;

-- regions: the koordinator wilayah (OS1, OS2, ...)
-- ================================================
-- Master data owned by SiAPP (D17). The code is the id (D19): there is no numeric id, and it
-- is never edited, because kantor cabang codes embed it ('1K' sits under 'OS1'). Soft
-- deleted rows keep their code, so a code is never handed out twice.
CREATE TABLE regions (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    code            TEXT NOT NULL,
    description     TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    PRIMARY KEY (organization_id, code),
    CONSTRAINT regions_code_check CHECK (code ~ '^OS[1-9][0-9]*$')
);

ALTER TABLE regions ENABLE ROW LEVEL SECURITY;
ALTER TABLE regions FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON regions
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON regions TO siapp_app;

-- web_role_assignments: ORG_ADMIN or REGION_ADMIN
-- ================================================
-- * ORG_ADMIN (super admin) sees everything and has no region.
-- * REGION_ADMIN sees everything under one koordinator wilayah, across all three PTs. The
--   CHECK keeps the two shapes apart, so a REGION_ADMIN without a region (which would
--   silently mean "can do nothing" or, worse, be read as "everything") cannot exist.
-- * The foreign key is composite and has no cascade: a region with an admin cannot vanish.
--   A NULL region_code skips it (MATCH SIMPLE), which is what ORG_ADMIN needs.
--
-- FORCE ROW LEVEL SECURITY applies the tenant policy to the table owner too, and a
-- migration has no tenant: the DELETE would see no rows. FORCE is lifted for this one
-- statement and put back before the migration's transaction commits.
ALTER TABLE web_role_assignments NO FORCE ROW LEVEL SECURITY;
DELETE FROM web_role_assignments WHERE role = 'PROJECT_ADMIN';
ALTER TABLE web_role_assignments FORCE ROW LEVEL SECURITY;

ALTER TABLE web_role_assignments DROP CONSTRAINT web_role_assignments_projects_check;
ALTER TABLE web_role_assignments DROP COLUMN project_ids;

-- A value cannot be removed from an enum, so the type is replaced.
ALTER TYPE web_role RENAME TO web_role_old;
CREATE TYPE web_role AS ENUM ('ORG_ADMIN', 'REGION_ADMIN');
ALTER TABLE web_role_assignments ALTER COLUMN role TYPE web_role USING role::text::web_role;
DROP TYPE web_role_old;

ALTER TABLE web_role_assignments ADD COLUMN region_code TEXT;
ALTER TABLE web_role_assignments
    ADD CONSTRAINT web_role_assignments_region_fkey
        FOREIGN KEY (organization_id, region_code) REFERENCES regions (organization_id, code),
    ADD CONSTRAINT web_role_assignments_region_check CHECK (
        (role = 'ORG_ADMIN'    AND region_code IS NULL) OR
        (role = 'REGION_ADMIN' AND region_code IS NOT NULL)
    );

-- audit_logs: entities that have a code, not a number
-- =====================================================
-- The hierarchy's rows are identified by their code (D19), so an audit row about one cannot
-- name it by a BIGINT. entity_code carries the code and entity_id is empty then; every row
-- still names exactly one of the two.
ALTER TABLE audit_logs ADD COLUMN entity_code TEXT;
ALTER TABLE audit_logs ALTER COLUMN entity_id DROP NOT NULL;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_entity_check
    CHECK ((entity_id IS NULL) <> (entity_code IS NULL));
CREATE INDEX audit_logs_entity_code_idx ON audit_logs (organization_id, entity_type, entity_code, created_at);

-- +goose Down
DROP INDEX audit_logs_entity_code_idx;
-- A row about a coded entity has no number to go back to: those rows are dropped, as the
-- audit trail of a feature that no longer exists. REVOKE'd DELETE does not bind the owner,
-- but FORCE ROW LEVEL SECURITY does, so it is lifted for this one statement.
ALTER TABLE audit_logs NO FORCE ROW LEVEL SECURITY;
DELETE FROM audit_logs WHERE entity_id IS NULL;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_entity_check;
ALTER TABLE audit_logs ALTER COLUMN entity_id SET NOT NULL;
ALTER TABLE audit_logs DROP COLUMN entity_code;

ALTER TABLE web_role_assignments NO FORCE ROW LEVEL SECURITY;
DELETE FROM web_role_assignments WHERE role = 'REGION_ADMIN';
ALTER TABLE web_role_assignments FORCE ROW LEVEL SECURITY;

ALTER TABLE web_role_assignments DROP CONSTRAINT web_role_assignments_region_check;
ALTER TABLE web_role_assignments DROP CONSTRAINT web_role_assignments_region_fkey;
ALTER TABLE web_role_assignments DROP COLUMN region_code;

ALTER TYPE web_role RENAME TO web_role_new;
CREATE TYPE web_role AS ENUM ('ORG_ADMIN', 'PROJECT_ADMIN');
ALTER TABLE web_role_assignments ALTER COLUMN role TYPE web_role USING role::text::web_role;
DROP TYPE web_role_new;

ALTER TABLE web_role_assignments ADD COLUMN project_ids BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE web_role_assignments ADD CONSTRAINT web_role_assignments_projects_check CHECK (
    (role = 'ORG_ADMIN'     AND cardinality(project_ids) = 0) OR
    (role = 'PROJECT_ADMIN' AND cardinality(project_ids) > 0)
);

DROP TABLE regions;
ALTER TABLE personnels DROP COLUMN company_name, DROP COLUMN company_id;
