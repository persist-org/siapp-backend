-- +goose Up

-- Phase 06.2: stakeholder types (the phone book of the panic flow), letterheads for exports and
-- the home-screen links to other apps.

-- A letterhead's logo is a photo like any other: registered, uploaded, then attached to the
-- letterhead (media.Attach), so it is checked and cleaned up by the same rules.
ALTER TYPE media_purpose ADD VALUE IF NOT EXISTS 'LETTERHEAD_LOGO';

-- ---------- stakeholder types ----------

CREATE TABLE stakeholder_types (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    code            TEXT NOT NULL,
    name            TEXT NOT NULL,
    -- The number a project falls back to when it has no contact of this type (DL-14). E.164, or
    -- one of the national three-digit emergency numbers (110, 113, ...): the usecase has the
    -- exact list, this CHECK is the backstop.
    default_phone   TEXT,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- ⚠ updated_at moves with every change, as for the other masters (DL-57).
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    -- A code stays taken after a delete, like a panic type's: a project's contact points at the
    -- type, and the type's meaning must not change under it.
    CONSTRAINT stakeholder_types_code_uq UNIQUE (organization_id, code),
    CONSTRAINT stakeholder_types_code_check CHECK (code ~ '^[A-Z][A-Z0-9_]{1,29}$'),
    CONSTRAINT stakeholder_types_name_check CHECK (name <> ''),
    CONSTRAINT stakeholder_types_phone_check CHECK (default_phone IS NULL OR default_phone ~ '^(\+[1-9][0-9]{7,14}|[1-9][0-9]{2})$'),
    CONSTRAINT stakeholder_types_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);
-- A name is taken by every type that is not deleted, active or not (the rule of module 06).
CREATE UNIQUE INDEX stakeholder_types_name_uq ON stakeholder_types (organization_id, lower(name))
    WHERE deleted_at IS NULL;

ALTER TABLE stakeholder_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE stakeholder_types FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON stakeholder_types
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON stakeholder_types TO siapp_app;

-- The panic type's who-to-call (06.1 left it open). RESTRICT: a type that panic types point at
-- is deactivated, not deleted; the usecase says MASTER_IN_USE before this ever fires.
ALTER TABLE panic_types ADD CONSTRAINT panic_types_stakeholder_type_fk
    FOREIGN KEY (stakeholder_type_id) REFERENCES stakeholder_types(id);

-- ---------- letterheads ----------

CREATE TABLE letterheads (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    name            TEXT NOT NULL,
    header_text     TEXT,
    footer_text     TEXT,
    -- A photo of purpose LETTERHEAD_LOGO, attached to this letterhead.
    logo_media_id   BIGINT REFERENCES media(id) ON DELETE SET NULL,
    is_default      BOOLEAN NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT letterheads_name_check CHECK (name <> '')
);
CREATE UNIQUE INDEX letterheads_name_uq ON letterheads (organization_id, lower(name));
-- One default letterhead per organization.
CREATE UNIQUE INDEX letterheads_default_uq ON letterheads (organization_id) WHERE is_default;

ALTER TABLE letterheads ENABLE ROW LEVEL SECURITY;
ALTER TABLE letterheads FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON letterheads
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON letterheads TO siapp_app;

-- ---------- app links ----------

-- A button on the home screen that opens another app (E-Koperasi Janusa, IKI). SiAPP only links
-- to them (DL-16): it holds no data of theirs.
CREATE TABLE app_links (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    name            TEXT NOT NULL,
    icon_key        TEXT NOT NULL,
    android_package TEXT,
    ios_url_scheme  TEXT,
    url             TEXT,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    CONSTRAINT app_links_name_check CHECK (name <> ''),
    -- At least one way to open the app. The seeded placeholders have none yet, so they are the
    -- one exception: they are inactive until the real links exist (DL-16), and an active link
    -- needs a target.
    CONSTRAINT app_links_target_check CHECK (
        NOT active OR android_package IS NOT NULL OR ios_url_scheme IS NOT NULL OR url IS NOT NULL),
    CONSTRAINT app_links_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);
CREATE UNIQUE INDEX app_links_name_uq ON app_links (organization_id, lower(name)) WHERE deleted_at IS NULL;
CREATE INDEX app_links_delta_idx ON app_links (organization_id, updated_at, id);

ALTER TABLE app_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_links FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON app_links
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON app_links TO siapp_app;

-- +goose Down
DROP TABLE app_links;
DROP TABLE letterheads;
ALTER TABLE panic_types DROP CONSTRAINT panic_types_stakeholder_type_fk;
DROP TABLE stakeholder_types;
-- An enum value cannot be dropped: LETTERHEAD_LOGO stays in media_purpose.
