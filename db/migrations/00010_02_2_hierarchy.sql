-- +goose Up

-- Phase 02.2: the rest of the hierarchy: kantor cabang, klien, proyek and regu.
--
-- ⚠ D19. The code is the primary key, and a child table points at its parent by
-- (organization_id, <parent>_code). A code contains its parent's code (1K-IBMI-S1 sits under
-- 1K-IBMI-S, 1K-IBMI and 1K), so it is NEVER edited after creation: a change would have to
-- rename every child, and every past attendance and report row that names it. The CHECK on
-- each table keeps the code and the columns it is built from in step, so neither can drift.
--
-- Soft deleted rows keep their row and their code, so a code is never handed out twice.
-- Nothing here has ON DELETE CASCADE: a parent with children cannot vanish, and the usecases
-- refuse a delete with HAS_ACTIVE_CHILDREN before the database has to.

CREATE TYPE business_line AS ENUM ('security', 'cleaning');

-- branches: kantor cabang
-- ========================
-- The code is {region number}{company letter}: 1K sits under OS1 and is a branch of PT KPI
-- (K), IDS (I) or PCD (P). The client sends region_code and company_letter, and the server
-- builds the code.
CREATE TABLE branches (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    code            TEXT NOT NULL,
    region_code     TEXT NOT NULL,
    company_letter  CHAR(1) NOT NULL,
    manager_name    TEXT,
    city            TEXT,
    phone           TEXT,
    fax             TEXT,
    email           TEXT,
    address         TEXT,
    zip_code        TEXT,
    description     TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    PRIMARY KEY (organization_id, code),
    FOREIGN KEY (organization_id, region_code) REFERENCES regions (organization_id, code),
    CONSTRAINT branches_letter_check CHECK (company_letter IN ('K', 'I', 'P')),
    CONSTRAINT branches_code_check CHECK (code = substring(region_code from 3) || company_letter)
);
CREATE INDEX branches_region_idx ON branches (organization_id, region_code);

-- clients: klien
-- ===============
-- The columns of PJ48's m_companies, without company_type (the table says it) and without
-- company_name (the description is the name). The code is {branch_code}-{company_prefix}.
CREATE TABLE clients (
    organization_id      BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                         REFERENCES organizations(id),
    code                 TEXT NOT NULL,
    branch_code          TEXT NOT NULL,
    description          TEXT NOT NULL,
    company_code         TEXT,
    company_prefix       TEXT NOT NULL,
    company_bidang_usaha TEXT,
    director_name        TEXT,
    city                 TEXT,
    phone                TEXT,
    fax                  TEXT,
    address              TEXT,
    zip_code             TEXT,
    email                TEXT,
    active               BOOLEAN NOT NULL DEFAULT true,
    created_by           BIGINT REFERENCES personnels(id),
    modify_by            BIGINT REFERENCES personnels(id),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    PRIMARY KEY (organization_id, code),
    FOREIGN KEY (organization_id, branch_code) REFERENCES branches (organization_id, code),
    CONSTRAINT clients_prefix_check CHECK (company_prefix ~ '^[A-Z0-9]{2,10}$'),
    CONSTRAINT clients_code_check CHECK (code = branch_code || '-' || company_prefix)
);
CREATE INDEX clients_branch_idx ON clients (organization_id, branch_code);

-- projects: proyek
-- =================
-- One proyek has one address, timezone and GPS point, and all its personnel work there (D20):
-- the regu below it have no geofence of their own. The code is {client_code}-{S|C}, so a
-- client has at most one proyek per business line.
--
-- ⚠ A proyek without coordinates is NOT configured: a feature used there answers
-- PROJECT_NOT_CONFIGURED, so a guard cannot check in at a site whose location is unknown.
-- The master data sheets carry no coordinates, so an imported proyek starts unconfigured.
--
-- next_team_number is the counter the regu codes are built from. It only ever goes up.
CREATE TABLE projects (
    organization_id            BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                               REFERENCES organizations(id),
    code                       TEXT NOT NULL,
    client_code                TEXT NOT NULL,
    description                TEXT NOT NULL,
    company_code               TEXT,
    company_prefix             TEXT,
    company_bidang_usaha       TEXT,
    business_line              business_line NOT NULL,
    city                       TEXT,
    address                    TEXT,
    phone                      TEXT,
    email                      TEXT,
    timezone                   TEXT NOT NULL,
    latitude                   NUMERIC(9, 6),
    longitude                  NUMERIC(9, 6),
    attendance_radius_meters   INTEGER NOT NULL DEFAULT 150,
    strict_attendance_geofence BOOLEAN NOT NULL DEFAULT false,
    gps_tracking_enabled       BOOLEAN NOT NULL DEFAULT true,
    patrol_interval_minutes    INTEGER,
    emergency_contacts         jsonb NOT NULL DEFAULT '[]',
    next_team_number           INTEGER NOT NULL DEFAULT 1,
    active                     BOOLEAN NOT NULL DEFAULT true,
    created_by                 BIGINT REFERENCES personnels(id),
    modify_by                  BIGINT REFERENCES personnels(id),
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    deleted_at                 timestamptz,
    PRIMARY KEY (organization_id, code),
    FOREIGN KEY (organization_id, client_code) REFERENCES clients (organization_id, code),
    CONSTRAINT projects_prefix_check CHECK (company_prefix IS NULL OR company_prefix ~ '^[A-Z0-9]{2,10}$'),
    CONSTRAINT projects_code_check CHECK (
        code = client_code || '-' || CASE business_line WHEN 'security' THEN 'S' ELSE 'C' END),
    CONSTRAINT projects_timezone_check CHECK (timezone IN ('Asia/Jakarta', 'Asia/Makassar', 'Asia/Jayapura')),
    -- Both or neither. The NOT NULLs matter: a CHECK that evaluates to NULL passes, so a lone
    -- latitude would otherwise slip through the BETWEEN branch.
    CONSTRAINT projects_point_check CHECK (
        (latitude IS NULL AND longitude IS NULL) OR
        (latitude IS NOT NULL AND longitude IS NOT NULL
         AND latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180)),
    CONSTRAINT projects_radius_check CHECK (attendance_radius_meters > 0),
    CONSTRAINT projects_patrol_check CHECK (patrol_interval_minutes IS NULL OR patrol_interval_minutes > 0),
    CONSTRAINT projects_next_team_check CHECK (next_team_number >= 1)
);
CREATE INDEX projects_client_idx ON projects (organization_id, client_code);

-- teams: regu
-- ============
-- The code is {project_code}{n}. n comes from projects.next_team_number, bumped in the same
-- transaction as the insert, and is NEVER reused: after S2 is deleted the next regu is still
-- S3 (or S5 if S4 exists), because past attendance and reports keep pointing at the old
-- code.
CREATE TABLE teams (
    organization_id                   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                                      REFERENCES organizations(id),
    code                              TEXT NOT NULL,
    project_code                      TEXT NOT NULL,
    description                       TEXT NOT NULL,
    phone                             TEXT,
    min_personnel                     INTEGER NOT NULL DEFAULT 0,
    notes                             TEXT,
    validate_shift                    BOOLEAN NOT NULL DEFAULT true,
    reject_attendance_after_tolerance BOOLEAN NOT NULL DEFAULT false,
    relationship_officer_personnel_id BIGINT REFERENCES personnels(id),
    active                            BOOLEAN NOT NULL DEFAULT true,
    created_at                        timestamptz NOT NULL DEFAULT now(),
    updated_at                        timestamptz NOT NULL DEFAULT now(),
    deleted_at                        timestamptz,
    PRIMARY KEY (organization_id, code),
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT teams_min_personnel_check CHECK (min_personnel >= 0),
    CONSTRAINT teams_code_check CHECK (
        left(code, length(project_code)) = project_code
        AND substring(code from length(project_code) + 1) ~ '^[1-9][0-9]*$')
);
CREATE INDEX teams_project_idx ON teams (organization_id, project_code);

ALTER TABLE branches ENABLE ROW LEVEL SECURITY;
ALTER TABLE branches FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON branches
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON branches TO siapp_app;

ALTER TABLE clients ENABLE ROW LEVEL SECURITY;
ALTER TABLE clients FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON clients
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON clients TO siapp_app;

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON projects
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON projects TO siapp_app;

ALTER TABLE teams ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON teams
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON teams TO siapp_app;

-- +goose Down
DROP TABLE teams;
DROP TABLE projects;
DROP TABLE clients;
DROP TABLE branches;
DROP TYPE business_line;
