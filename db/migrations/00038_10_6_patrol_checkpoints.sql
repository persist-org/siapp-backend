-- +goose Up

-- Phase 10.6 (docs/plan/10.1-patrol-update.md): checkpoints inside zones.
--
-- A checkpoint is a place with its own QR sticker and its own checklist. A zone is an ordered list
-- of checkpoints, and one checkpoint can be in many zones, so the order lives on the link
-- (patrol_zone_checkpoints), not on the checkpoint.
--
-- Nothing is dropped here: the zone QR and the zone checklist are still read by the scan until
-- Phase 10.9 replaces it, and 10.9 drops them in the same change that deletes the code.
--
-- ⚠ The new tables use uuid ids (asked for in the plan, U2); zones, scans and routes keep BIGINT. As
-- everywhere else, a table below the hierarchy points at its project by code (D19) and every foreign
-- key carries organization_id, so a row can never be tied to another organization's row.

-- patrol_checkpoints: a place the guard scans
-- ============================================
-- * A checkpoint belongs to one project for ever (project_code never changes: the link to a zone
--   below depends on it).
-- * The geofence is the three columns together or none, like a zone's: a checkpoint without one
--   relies on its QR alone. The default radius (30 m) is applied by the usecase, not here, because
--   "no geofence" must stay representable.
-- * qr_secret is what makes the printed sticker valid: 16 random bytes in base64url (22 characters).
--   Rotating it invalidates the sticker printed before (qr_rotated_at says when).
-- * A delete keeps the row (scans point at the checkpoint), is always inactive, and is never undone.
CREATE TABLE patrol_checkpoints (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    project_code    TEXT NOT NULL,
    name            TEXT NOT NULL,
    description     TEXT,
    latitude        numeric(9,6),
    longitude       numeric(9,6),
    radius_meters   INTEGER,
    qr_secret       TEXT NOT NULL,
    qr_rotated_at   timestamptz NOT NULL DEFAULT now(),
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT patrol_checkpoints_org_id_uq UNIQUE (organization_id, id),
    -- What the zone link points at: it carries the project, so a link across projects cannot exist.
    CONSTRAINT patrol_checkpoints_org_project_id_uq UNIQUE (organization_id, project_code, id),
    CONSTRAINT patrol_checkpoints_name_check CHECK (name <> ''),
    CONSTRAINT patrol_checkpoints_secret_check CHECK (length(qr_secret) = 22),
    -- All three or none. The NOT NULLs matter: a CHECK that evaluates to NULL passes.
    CONSTRAINT patrol_checkpoints_geofence_check CHECK (
        (latitude IS NULL AND longitude IS NULL AND radius_meters IS NULL) OR
        (latitude IS NOT NULL AND longitude IS NOT NULL AND radius_meters IS NOT NULL
         AND latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180
         AND radius_meters > 0)),
    CONSTRAINT patrol_checkpoints_deleted_check CHECK (deleted_at IS NULL OR NOT active)
);
-- Two stickers called "Gerbang" in one project could not be told apart (same reason as 00036). A
-- deleted checkpoint keeps its row but frees its name.
CREATE UNIQUE INDEX patrol_checkpoints_name_uq
    ON patrol_checkpoints (organization_id, project_code, lower(name))
    WHERE deleted_at IS NULL;
CREATE INDEX patrol_checkpoints_project_idx ON patrol_checkpoints (organization_id, project_code) WHERE deleted_at IS NULL;

-- patrol_zone_checkpoints: which checkpoints a zone has, and in what order
-- ========================================================================
-- * sort_order is the position, 1..n. The server renumbers on every change, so there are no gaps and
--   no ties (the UNIQUE enforces the second). The order rule of a scan reads it (Phase 10.9).
-- * ⚠ Both foreign keys carry project_code. That is what makes a link between a zone and a checkpoint
--   of different projects impossible in the database, not only in Go: the zone and the checkpoint
--   must both be in the project written on this row.
-- * A link is not evidence (a scan keeps its own zone_id and checkpoint_id), so it can be deleted.
ALTER TABLE patrol_zones
    ADD CONSTRAINT patrol_zones_org_project_id_uq UNIQUE (organization_id, project_code, id);

CREATE TABLE patrol_zone_checkpoints (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    project_code    TEXT NOT NULL,
    zone_id         BIGINT NOT NULL,
    checkpoint_id   uuid NOT NULL,
    sort_order      INTEGER NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, zone_id, checkpoint_id),
    FOREIGN KEY (organization_id, project_code, zone_id)
        REFERENCES patrol_zones (organization_id, project_code, id),
    FOREIGN KEY (organization_id, project_code, checkpoint_id)
        REFERENCES patrol_checkpoints (organization_id, project_code, id),
    CONSTRAINT patrol_zone_checkpoints_order_uq UNIQUE (organization_id, zone_id, sort_order),
    CONSTRAINT patrol_zone_checkpoints_order_check CHECK (sort_order >= 1)
);
-- Which zones is this checkpoint in.
CREATE INDEX patrol_zone_checkpoints_checkpoint_idx ON patrol_zone_checkpoints (organization_id, checkpoint_id);

-- patrol_checkpoint_actions: the checklist of a checkpoint
-- =========================================================
-- An action is one thing the guard does and shows at the checkpoint ("Periksa gembok"). Every active
-- action must be answered on every scan; the answer is photos and a note, nothing else.
-- * min_photos > 0 makes the photo required; max_photos = 0 means there is no photo slot.
-- * is_note_required is about the guard's note. `instruction` is the admin's text shown to the guard.
-- Actions are deactivated, never deleted: a scan keeps a snapshot of the action it answered, but the
-- admin's history of the form is worth keeping too. siapp_app is refused DELETE for that reason.
CREATE TABLE patrol_checkpoint_actions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                     REFERENCES organizations(id),
    checkpoint_id    uuid NOT NULL,
    sort_order       INTEGER NOT NULL DEFAULT 0,
    action           TEXT NOT NULL,
    instruction      TEXT,
    min_photos       SMALLINT NOT NULL DEFAULT 0,
    max_photos       SMALLINT NOT NULL DEFAULT 1,
    is_note_required BOOLEAN NOT NULL DEFAULT false,
    active           BOOLEAN NOT NULL DEFAULT true,
    deactivated_at   timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, checkpoint_id) REFERENCES patrol_checkpoints (organization_id, id),
    CONSTRAINT patrol_checkpoint_actions_org_id_uq UNIQUE (organization_id, id),
    CONSTRAINT patrol_checkpoint_actions_action_check CHECK (action <> ''),
    CONSTRAINT patrol_checkpoint_actions_photos_check CHECK (
        min_photos >= 0 AND min_photos <= max_photos AND max_photos <= 5),
    CONSTRAINT patrol_checkpoint_actions_active_check CHECK (active = (deactivated_at IS NULL))
);
CREATE INDEX patrol_checkpoint_actions_checkpoint_idx
    ON patrol_checkpoint_actions (organization_id, checkpoint_id, sort_order, id) WHERE active;

-- The scan header is patrol_scans, which already holds the offline local_id, the work date, the
-- distance, the decision and the patrol route. It now also says which checkpoint was scanned.
-- * checkpoint_id is nullable only for rows written before Phase 10.9; every new scan sets it (the
--   usecase enforces it) and Phase 10.10 makes it NOT NULL.
-- * (organization_id, id) must be unique for patrol_scan_actions to point at it with both columns.
ALTER TABLE patrol_scans
    ADD COLUMN checkpoint_id uuid,
    ADD CONSTRAINT patrol_scans_org_id_uq UNIQUE (organization_id, id),
    ADD CONSTRAINT patrol_scans_checkpoint_fk FOREIGN KEY (organization_id, checkpoint_id)
        REFERENCES patrol_checkpoints (organization_id, id);
-- The order rule and the interval rule both read one person's scans of one checkpoint in one zone
-- and work date, in time order.
CREATE INDEX patrol_scans_order_idx
    ON patrol_scans (organization_id, personnel_id, zone_id, work_date, checkpoint_id, occurred_at);
-- Progress counts the scans of each checkpoint of a zone per regu and day.
CREATE INDEX patrol_scans_checkpoint_idx
    ON patrol_scans (organization_id, zone_id, checkpoint_id, team_code, work_date);

-- Check-in and check-out use a checkpoint QR once the zone has none (Phase 10.9). Nullable for the
-- same reason as patrol_scans.checkpoint_id.
ALTER TABLE zone_checkins
    ADD COLUMN checkpoint_id uuid,
    ADD CONSTRAINT zone_checkins_checkpoint_fk FOREIGN KEY (organization_id, checkpoint_id)
        REFERENCES patrol_checkpoints (organization_id, id);

-- patrol_scan_actions: one answered action of a scan
-- ===================================================
-- The detail of patrol_scans: one row per action of the checkpoint, with the guard's note and photos.
-- * It is evidence: written with the scan, never changed, never deleted.
-- * personnel_id is copied from the scan on purpose (denormalized): per-person action reports then
--   need no join with the header.
-- * `note` and `media_ids` are what the code reads (validation, attaching the photos, photo URLs,
--   reports). The same two values are also inside `snapshot`, so the row alone says everything about
--   the answer for an export or an audit. Both are written by one insert from the same values and the
--   row is never updated, so they cannot drift apart.
-- * `snapshot` is the action as it was at scan time, with the answer:
--   { action, instruction, min_photos, max_photos, is_note_required, sort_order, note, media_ids }.
--   A later edit of the action never rewrites it.
-- * The photos are rows of media attached to the scan itself (purpose PATROL_SCAN), so the existing
--   media views and access rules keep working; media_ids says which belong to this action.
CREATE TABLE patrol_scan_actions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    scan_id         BIGINT NOT NULL,
    checkpoint_id   uuid NOT NULL,
    action_id       uuid NOT NULL,
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    note            TEXT,
    media_ids       BIGINT[] NOT NULL DEFAULT '{}',
    snapshot        jsonb NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, scan_id) REFERENCES patrol_scans (organization_id, id),
    FOREIGN KEY (organization_id, checkpoint_id) REFERENCES patrol_checkpoints (organization_id, id),
    FOREIGN KEY (organization_id, action_id) REFERENCES patrol_checkpoint_actions (organization_id, id),
    -- An action is answered once per scan.
    CONSTRAINT patrol_scan_actions_scan_action_uq UNIQUE (organization_id, scan_id, action_id),
    CONSTRAINT patrol_scan_actions_snapshot_check CHECK (
        jsonb_typeof(snapshot) = 'object' AND snapshot ?& ARRAY['action', 'note', 'media_ids'])
);
CREATE INDEX patrol_scan_actions_scan_idx ON patrol_scan_actions (organization_id, scan_id);
CREATE INDEX patrol_scan_actions_person_idx ON patrol_scan_actions (organization_id, personnel_id, created_at);

ALTER TABLE patrol_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_checkpoints
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- A checkpoint is soft deleted, so there is no DELETE. ⚠ It is a REVOKE: default privileges already give
-- siapp_app every right on a new table, so a GRANT of less would not take anything away.
REVOKE DELETE ON patrol_checkpoints FROM siapp_app;

ALTER TABLE patrol_zone_checkpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_zone_checkpoints FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_zone_checkpoints
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- A link is replaced as a whole when the admin reorders a zone, so it keeps the default rights,
-- DELETE included.

ALTER TABLE patrol_checkpoint_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_checkpoint_actions FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_checkpoint_actions
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- Actions are deactivated, never deleted (see the table).
REVOKE DELETE ON patrol_checkpoint_actions FROM siapp_app;

ALTER TABLE patrol_scan_actions ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_scan_actions FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_scan_actions
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- Evidence: written with the scan and never changed.
REVOKE UPDATE, DELETE ON patrol_scan_actions FROM siapp_app;

-- +goose Down
DROP TABLE patrol_scan_actions;
ALTER TABLE zone_checkins DROP COLUMN checkpoint_id;
DROP INDEX patrol_scans_checkpoint_idx;
DROP INDEX patrol_scans_order_idx;
ALTER TABLE patrol_scans
    DROP COLUMN checkpoint_id,
    DROP CONSTRAINT patrol_scans_org_id_uq;
DROP TABLE patrol_checkpoint_actions;
DROP TABLE patrol_zone_checkpoints;
ALTER TABLE patrol_zones DROP CONSTRAINT patrol_zones_org_project_id_uq;
DROP TABLE patrol_checkpoints;
