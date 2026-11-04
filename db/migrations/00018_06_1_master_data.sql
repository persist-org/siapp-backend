-- +goose Up

-- Phase 06.1: report and instruction categories, and panic types.

CREATE TYPE category_kind AS ENUM ('REPORT', 'INSTRUCTION');
CREATE TYPE urgency AS ENUM ('LOW', 'NORMAL', 'HIGH');

-- ⚠ One table serves two contract resources (/report-categories and /instruction-categories):
-- they have the same shape and the same rules, and `kind` tells them apart. The handlers fix the
-- kind from the path, so a client never sends it and cannot move a category to the other kind.
CREATE TABLE categories (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    kind            category_kind NOT NULL,
    name            TEXT NOT NULL,
    -- Only an instruction has an urgency (manual §2.8, "high urgent"). The usecase gives the
    -- friendly error; the CHECK below is the backstop.
    urgency         urgency,
    -- The business lines this category is offered to; empty means all of them, so cleaning can
    -- have categories of its own.
    -- ⚠ text[] with a CHECK, not business_line[]: the pool runs pgx in exec mode, which cannot
    -- encode or decode an array of an enum type. The CHECK keeps the same two values.
    business_lines  TEXT[] NOT NULL DEFAULT '{}',
    sort_order      INTEGER NOT NULL DEFAULT 0,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- ⚠ updated_at is the delta-sync watermark (DL-34, DL-57). Every change, a deactivation and a
    -- delete too, sets it in the same statement, or the offline cache never hears of the change.
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- A delete keeps the row, so a delta pull can send it as a tombstone. It is always inactive.
    deleted_at      timestamptz,
    CONSTRAINT categories_name_check CHECK (name <> ''),
    CONSTRAINT categories_lines_check CHECK (business_lines <@ ARRAY['security', 'cleaning']::text[]),
    CONSTRAINT categories_urgency_check CHECK ((kind = 'INSTRUCTION') = (urgency IS NOT NULL)),
    CONSTRAINT categories_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);

-- A name is taken only by an active category: a deactivated one can be re-created.
CREATE UNIQUE INDEX categories_name_uq ON categories (organization_id, kind, name) WHERE active;
CREATE INDEX categories_delta_idx ON categories (organization_id, kind, updated_at, id);

CREATE TABLE panic_types (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                      REFERENCES organizations(id),
    code              TEXT NOT NULL,
    name              TEXT NOT NULL,
    -- Phase 06.2 creates stakeholder_types and adds the foreign key in its migration.
    stakeholder_type_id BIGINT,
    sort_order        INTEGER NOT NULL DEFAULT 0,
    active            BOOLEAN NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    -- A code stays taken after a delete, as a region's does (D19): a panic that was raised with
    -- it keeps its meaning.
    CONSTRAINT panic_types_code_uq UNIQUE (organization_id, code),
    CONSTRAINT panic_types_code_check CHECK (code ~ '^[A-Z][A-Z0-9_]{1,29}$'),
    CONSTRAINT panic_types_name_check CHECK (name <> ''),
    CONSTRAINT panic_types_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);
CREATE INDEX panic_types_delta_idx ON panic_types (organization_id, updated_at, id);

ALTER TABLE categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE categories FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON categories
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON categories TO siapp_app;

ALTER TABLE panic_types ENABLE ROW LEVEL SECURITY;
ALTER TABLE panic_types FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON panic_types
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON panic_types TO siapp_app;

-- +goose Down
DROP TABLE panic_types;
DROP TABLE categories;
DROP TYPE urgency;
DROP TYPE category_kind;
