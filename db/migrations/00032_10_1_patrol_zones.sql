-- +goose Up

-- Phase 10.1: patrol and guard zones, and the checklist each zone asks of the guard.
--
-- ⚠ The plan (docs/plan/10-patrol.md) keys a zone to numeric project_id and team_id. The built
-- hierarchy is keyed by code (D19), so a zone points at its project, and optionally its regu, by
-- (organization_id, <parent>_code), like every table below the hierarchy does.

CREATE TYPE patrol_zone_type AS ENUM ('PATROL', 'GUARD');
CREATE TYPE checklist_answer_type AS ENUM ('BOOLEAN', 'TEXT', 'NUMBER', 'CHOICE');
-- Used for the photo and the note slot of a question.
CREATE TYPE checklist_attach_mode AS ENUM ('NONE', 'OPTIONAL', 'REQUIRED');
CREATE TYPE checklist_attach_when AS ENUM ('ALWAYS', 'ON_NO');

-- patrol_zones: Zona patroli (PATROL) and pos jaga (GUARD, DL-11); one flow serves both.
-- =======================================================================================
-- * team_code NULL is a zone for the whole project; set, a zone for one regu. The CHECK makes
--   sure the regu belongs to the project (a regu code is its project's code and a number).
-- * The geofence is the three columns together or none: a zone without one relies on its QR
--   alone (web spec 08). The default radius of 50 m is applied by the usecase when a point is
--   sent without a radius, not here, because "no geofence" must stay representable.
-- * qr_secret is what makes the printed sticker valid. It is 16 random bytes in base64url
--   (22 characters). Rotating it invalidates every sticker printed before.
-- * A delete keeps the row (scans point at the zone), is always inactive, and is never undone.
CREATE TABLE patrol_zones (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    project_code    TEXT NOT NULL,
    team_code       TEXT,
    type            patrol_zone_type NOT NULL,
    name            TEXT NOT NULL,
    alias           TEXT,
    description     TEXT,
    popup_text      TEXT,
    latitude        numeric(9,6),
    longitude       numeric(9,6),
    radius_meters   INTEGER,
    min_visit       INTEGER NOT NULL DEFAULT 1,
    start_date      DATE,
    photo_required  BOOLEAN NOT NULL DEFAULT true,
    qr_secret       TEXT NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    -- Later tables point at (organization_id, id), so a row can never be tied to a zone of
    -- another organization even by a bug.
    CONSTRAINT patrol_zones_org_id_uq UNIQUE (organization_id, id),
    CONSTRAINT patrol_zones_name_check CHECK (name <> ''),
    CONSTRAINT patrol_zones_min_visit_check CHECK (min_visit >= 1),
    CONSTRAINT patrol_zones_secret_check CHECK (length(qr_secret) = 22),
    CONSTRAINT patrol_zones_team_check CHECK (
        team_code IS NULL OR (
            left(team_code, length(project_code)) = project_code
            AND substring(team_code from length(project_code) + 1) ~ '^[1-9][0-9]*$')),
    -- All three or none. The NOT NULLs matter: a CHECK that evaluates to NULL passes.
    CONSTRAINT patrol_zones_geofence_check CHECK (
        (latitude IS NULL AND longitude IS NULL AND radius_meters IS NULL) OR
        (latitude IS NOT NULL AND longitude IS NOT NULL AND radius_meters IS NOT NULL
         AND latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180
         AND radius_meters > 0)),
    CONSTRAINT patrol_zones_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);
CREATE INDEX patrol_zones_project_idx ON patrol_zones (organization_id, project_code, type) WHERE deleted_at IS NULL;

-- patrol_checklist_items: the questions of a zone, as the admin configured them.
-- ================================================================================
-- Items are deactivated, never deleted: a scan keeps a snapshot of the question it answered
-- (Phase 10.2), but the admin's history of the form is worth keeping too. siapp_app gets no
-- DELETE for that reason.
CREATE TABLE patrol_checklist_items (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    zone_id         BIGINT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    label           TEXT NOT NULL,
    help_text       TEXT,
    answer_type     checklist_answer_type NOT NULL,
    -- Only a CHOICE has options: [{ "value": "...", "label": "..." }], at least two.
    options         jsonb,
    required        BOOLEAN NOT NULL DEFAULT true,
    photo_mode      checklist_attach_mode NOT NULL DEFAULT 'NONE',
    photo_max       SMALLINT NOT NULL DEFAULT 1,
    note_mode       checklist_attach_mode NOT NULL DEFAULT 'NONE',
    -- ON_NO: a REQUIRED photo or note is demanded only when a BOOLEAN is answered "no", the
    -- common "report a problem with proof" case.
    attach_when     checklist_attach_when NOT NULL DEFAULT 'ALWAYS',
    active          BOOLEAN NOT NULL DEFAULT true,
    deactivated_at  timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, zone_id) REFERENCES patrol_zones (organization_id, id),
    CONSTRAINT patrol_checklist_items_label_check CHECK (label <> ''),
    CONSTRAINT patrol_checklist_items_photo_max_check CHECK (photo_max BETWEEN 1 AND 5),
    CONSTRAINT patrol_checklist_items_options_check CHECK (
        (answer_type = 'CHOICE') = (options IS NOT NULL)
        AND (options IS NULL OR (jsonb_typeof(options) = 'array' AND jsonb_array_length(options) >= 2))),
    CONSTRAINT patrol_checklist_items_attach_when_check CHECK (attach_when = 'ALWAYS' OR answer_type = 'BOOLEAN'),
    CONSTRAINT patrol_checklist_items_active_check CHECK (active = (deactivated_at IS NULL))
);
CREATE INDEX patrol_checklist_items_zone_idx ON patrol_checklist_items (organization_id, zone_id, sort_order, id) WHERE active;

ALTER TABLE patrol_zones ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_zones FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_zones
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- A zone is soft deleted, so there is no DELETE.
GRANT SELECT, INSERT, UPDATE ON patrol_zones TO siapp_app;

ALTER TABLE patrol_checklist_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_checklist_items FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_checklist_items
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE ON patrol_checklist_items TO siapp_app;

-- +goose Down
DROP TABLE patrol_checklist_items;
DROP TABLE patrol_zones;
DROP TYPE checklist_attach_when;
DROP TYPE checklist_attach_mode;
DROP TYPE checklist_answer_type;
DROP TYPE patrol_zone_type;
