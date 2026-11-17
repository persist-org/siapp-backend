-- +goose Up

-- Phase 10.9 (docs/plan/10.1-patrol-update.md): the scan moves to the checkpoint, so what the zone had for
-- it goes: its QR secret, its photo rule, and the checklist it asked (the checkpoint has its own).
--
-- ⚠ patrol_scans.answers stays until Phase 10.10: scans made before the checkpoints still hold their
-- answers there, and their views read them.

-- What the guard's phone said about itself when an action was answered (Phase 10.9, U13): battery,
-- network, GPS and so on. Evidence, not a gate: nothing is refused because of it. The table stays
-- insert-only (it is never updated), so it needs no more than the column.
ALTER TABLE patrol_scan_actions
    ADD COLUMN device_condition jsonb,
    ADD CONSTRAINT patrol_scan_actions_device_condition_check
        CHECK (device_condition IS NULL OR jsonb_typeof(device_condition) = 'object');

DROP TABLE patrol_checklist_items;
DROP TYPE checklist_answer_type;
DROP TYPE checklist_attach_mode;
DROP TYPE checklist_attach_when;

-- The zone has no QR any more (the checkpoint's sticker is the one that is scanned), and the photo rule is
-- per action (min_photos). patrol_zones_secret_check goes with its column.
ALTER TABLE patrol_zones
    DROP COLUMN qr_secret,
    DROP COLUMN photo_required;

-- The interval is per checkpoint now: patrol_scans_order_idx (Phase 10.6) serves it and the order rule.
DROP INDEX patrol_scans_interval_idx;

-- The zone's point and radius are display only from here (the map in the admin panel): nothing checks a
-- scan or a check-in against them. The geofence is the checkpoint's.
COMMENT ON COLUMN patrol_zones.latitude IS 'Display only (Phase 10.9): the geofence is the checkpoint''s';
COMMENT ON COLUMN patrol_zones.longitude IS 'Display only (Phase 10.9): the geofence is the checkpoint''s';
COMMENT ON COLUMN patrol_zones.radius_meters IS 'Display only (Phase 10.9): the geofence is the checkpoint''s';

-- +goose Down
-- The structure comes back, empty: what the dropped columns and table held is gone. A zone gets a new
-- random secret, which the zone no longer uses.
CREATE TYPE checklist_answer_type AS ENUM ('BOOLEAN', 'TEXT', 'NUMBER', 'CHOICE');
CREATE TYPE checklist_attach_mode AS ENUM ('NONE', 'OPTIONAL', 'REQUIRED');
CREATE TYPE checklist_attach_when AS ENUM ('ALWAYS', 'ON_NO');

CREATE TABLE patrol_checklist_items (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    zone_id         BIGINT NOT NULL,
    sort_order      INTEGER NOT NULL DEFAULT 0,
    label           TEXT NOT NULL,
    help_text       TEXT,
    answer_type     checklist_answer_type NOT NULL,
    options         jsonb,
    required        BOOLEAN NOT NULL DEFAULT true,
    photo_mode      checklist_attach_mode NOT NULL DEFAULT 'NONE',
    photo_max       SMALLINT NOT NULL DEFAULT 1,
    note_mode       checklist_attach_mode NOT NULL DEFAULT 'NONE',
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
ALTER TABLE patrol_checklist_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_checklist_items FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_checklist_items
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);

ALTER TABLE patrol_zones
    ADD COLUMN qr_secret TEXT NOT NULL DEFAULT substr(translate(encode(decode(md5(random()::text), 'hex'), 'base64'), '+/=', '-_'), 1, 22),
    ADD COLUMN photo_required BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE patrol_zones ALTER COLUMN qr_secret DROP DEFAULT;
ALTER TABLE patrol_zones ADD CONSTRAINT patrol_zones_secret_check CHECK (length(qr_secret) = 22);

CREATE INDEX patrol_scans_interval_idx ON patrol_scans (organization_id, personnel_id, zone_id, occurred_at);

COMMENT ON COLUMN patrol_zones.latitude IS NULL;
COMMENT ON COLUMN patrol_zones.longitude IS NULL;
COMMENT ON COLUMN patrol_zones.radius_meters IS NULL;

ALTER TABLE patrol_scan_actions DROP COLUMN device_condition;
