-- +goose Up

CREATE TYPE web_role        AS ENUM ('ORG_ADMIN', 'PROJECT_ADMIN');
CREATE TYPE device_platform AS ENUM ('ANDROID', 'IOS', 'WEB');

-- web_role_assignments: who may use the web admin, and over what
-- ================================================================
-- ⚠ This lives in SiAPP even though identity is Personalia's. Personalia says WHO someone
-- is; authorization inside the Security add-on ("RBAC scope-project", LOCK/03 §1) is the
-- add-on's own data, and Personalia has no notion of it. A person with no row here is field
-- staff: they cannot sign in on the web and get 403 NOT_ADMIN on every /admin route.
--
-- * ORG_ADMIN sees the whole organization and has no project list.
-- * PROJECT_ADMIN is limited to project_ids, which must not be empty. The CHECK keeps the
--   two shapes apart, so a PROJECT_ADMIN without projects (which would silently mean "can
--   do nothing" or, worse, be read as "everything") cannot exist.
-- * project_ids are Personalia project ids (module 02 maps them); they are not foreign keys.
-- * granted_by_personnel_id is NULL for the first admin, created from the command line.
CREATE TABLE web_role_assignments (
    id                      BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id         BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                            REFERENCES organizations(id),
    personnel_id            BIGINT NOT NULL REFERENCES personnels(id) ON DELETE CASCADE,
    role                    web_role NOT NULL,
    project_ids             BIGINT[] NOT NULL DEFAULT '{}',
    granted_by_personnel_id BIGINT REFERENCES personnels(id) ON DELETE SET NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT web_role_assignments_org_personnel_key UNIQUE (organization_id, personnel_id),
    CONSTRAINT web_role_assignments_projects_check CHECK (
        (role = 'ORG_ADMIN'     AND cardinality(project_ids) = 0) OR
        (role = 'PROJECT_ADMIN' AND cardinality(project_ids) > 0)
    )
);

ALTER TABLE web_role_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE web_role_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON web_role_assignments
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON web_role_assignments TO siapp_app;

-- devices: where push notifications go
-- =====================================
-- SiAPP's push-token registry. It is NOT the one-phone binding (device_bindings, 01.1):
-- that is the admin's record of which phone a person may use, this is the token of the app
-- install that is signed in right now.
--
-- * A device follows its login chain: a refresh moves it to the new session.
-- * A push token belongs to one device: registering a token that another device holds
--   removes the other one first (a phone that changed hands, or an app reinstalled).
-- * A device never outlives its login: it goes with its session (ON DELETE CASCADE), and
--   whatever ends a login chain (logout, reuse, a device reset) deletes it explicitly,
--   because a revoked session is not a deleted one.
CREATE TABLE devices (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id) ON DELETE CASCADE,
    session_id      BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    platform        device_platform NOT NULL,
    push_token      TEXT NOT NULL,
    app_version     TEXT,
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT devices_session_key UNIQUE (session_id),
    CONSTRAINT devices_org_push_token_key UNIQUE (organization_id, push_token)
);

CREATE INDEX devices_personnel_idx ON devices (organization_id, personnel_id);

ALTER TABLE devices ENABLE ROW LEVEL SECURITY;
ALTER TABLE devices FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON devices
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON devices TO siapp_app;

-- +goose Down
DROP TABLE devices;
DROP TABLE web_role_assignments;
DROP TYPE device_platform;
DROP TYPE web_role;
