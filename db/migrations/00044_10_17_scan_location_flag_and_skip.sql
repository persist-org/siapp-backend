-- +goose Up
-- Phase 10.17 (docs/plan/10.3-patrol-mobile-feedback.md): a scan outside the geofence is stored with a flag
-- instead of being refused, and a checkpoint that cannot be scanned can be skipped with a reason.
--
-- Why. The order rule counts the scans that were stored. A scan that was refused (GPS drift in a basement)
-- is not stored, so every later checkpoint of an offline round was refused for good, after the guard had
-- left. The geofence is now a mark for the DANRU, who decides at approval; a skip is a row of its own kind
-- that the order rule counts like any other.

CREATE TYPE patrol_location_flag AS ENUM ('OK', 'OUTSIDE_RADIUS', 'LOW_ACCURACY', 'NO_FIX');
CREATE TYPE patrol_scan_outcome AS ENUM ('SCANNED', 'SKIPPED');
CREATE TYPE patrol_skip_reason AS ENUM ('STICKER_DAMAGED', 'ACCESS_BLOCKED', 'OTHER');

-- ⚠ A skip is a patrol_scans row on purpose: the order rule (every status counts), approval, the offline
-- local_id and the join to the patrol already work for a row of this table. A SKIPPED row has no
-- patrol_scan_actions, and the progress counters and the patrol interval leave it out (it is not a visit).
ALTER TABLE patrol_scans
    ALTER COLUMN latitude DROP NOT NULL,
    ALTER COLUMN longitude DROP NOT NULL,
    ALTER COLUMN accuracy_meters DROP NOT NULL,
    ADD COLUMN location_flag patrol_location_flag NOT NULL DEFAULT 'OK',
    ADD COLUMN outcome patrol_scan_outcome NOT NULL DEFAULT 'SCANNED',
    ADD COLUMN skip_reason patrol_skip_reason,
    -- The photos of a skip (a broken sticker, a locked door). A scan's photos belong to its actions
    -- (patrol_scan_actions.media_ids); a skip has no actions, so its photos are here. Empty for a scan.
    ADD COLUMN skip_media_ids BIGINT[] NOT NULL DEFAULT '{}',
    -- The position is all there or not there at all; no position is exactly NO_FIX.
    ADD CONSTRAINT patrol_scans_position_check CHECK (
        (latitude IS NULL AND longitude IS NULL AND accuracy_meters IS NULL) OR
        (latitude IS NOT NULL AND longitude IS NOT NULL AND accuracy_meters IS NOT NULL)),
    ADD CONSTRAINT patrol_scans_no_fix_check CHECK ((location_flag = 'NO_FIX') = (latitude IS NULL)),
    -- A reason exactly when the checkpoint was skipped.
    ADD CONSTRAINT patrol_scans_skip_check CHECK ((outcome = 'SKIPPED') = (skip_reason IS NOT NULL)),
    ADD CONSTRAINT patrol_scans_skip_media_check CHECK (outcome = 'SKIPPED' OR cardinality(skip_media_ids) = 0);

-- +goose Down
-- The structure comes back; rows with no position (a scan with no fix, a skip) do not: the old columns
-- cannot hold them. Dev data only, like 00041.
ALTER TABLE patrol_scans NO FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_scan_actions NO FORCE ROW LEVEL SECURITY;
DELETE FROM patrol_scan_actions WHERE scan_id IN (SELECT id FROM patrol_scans WHERE latitude IS NULL OR outcome = 'SKIPPED');
DELETE FROM patrol_scans WHERE latitude IS NULL OR outcome = 'SKIPPED';
ALTER TABLE patrol_scan_actions FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_scans FORCE ROW LEVEL SECURITY;

ALTER TABLE patrol_scans
    DROP CONSTRAINT patrol_scans_skip_media_check,
    DROP CONSTRAINT patrol_scans_skip_check,
    DROP CONSTRAINT patrol_scans_no_fix_check,
    DROP CONSTRAINT patrol_scans_position_check,
    DROP COLUMN skip_media_ids,
    DROP COLUMN skip_reason,
    DROP COLUMN outcome,
    DROP COLUMN location_flag,
    ALTER COLUMN accuracy_meters SET NOT NULL,
    ALTER COLUMN longitude SET NOT NULL,
    ALTER COLUMN latitude SET NOT NULL;

DROP TYPE patrol_skip_reason;
DROP TYPE patrol_scan_outcome;
DROP TYPE patrol_location_flag;
