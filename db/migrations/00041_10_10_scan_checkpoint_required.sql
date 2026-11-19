-- +goose Up

-- Phase 10.10 (docs/plan/10.1-patrol-update.md): every scan is of a checkpoint, and the old answers column
-- goes (a scan's answers are rows of patrol_scan_actions since Phase 10.9).
--
-- ⚠ DESTRUCTIVE, and safe only because there was no production data when this shipped (the plan's
-- decision: the checkpoints replaced the zone's QR and checklist before any sticker was printed). A scan
-- made before the checkpoints has no checkpoint and answers a checklist that no longer exists: it cannot
-- be shown, approved or counted by the new views, so it is deleted, with the photos attached to it. A dev
-- database loses its pre-checkpoint scans; run the seed again to get new ones. Do NOT ship this migration
-- over a database that holds real scans without migrating them first.
--
-- The photo rows go with their scans so nothing points at a record that is gone. The files they named in
-- storage are not touched here (a migration cannot reach the object store): they are orphans for the
-- media cleanup, and on a dev server nothing to worry about.
-- ⚠ FORCE ROW LEVEL SECURITY applies the tenant policy to the table owner too, and a migration has no
-- tenant: on a database whose owner is not a superuser or BYPASSRLS the three statements below would see no
-- rows, and SET NOT NULL would then fail on any old scan. FORCE is lifted for them and put back before the
-- migration's transaction commits (the same pattern as 00008 and 00019), so no other session ever sees
-- these tables without it.
ALTER TABLE media NO FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_scans NO FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_routes NO FORCE ROW LEVEL SECURITY;

DELETE FROM media
WHERE attached_entity_type = 'PATROL_SCAN'
  AND attached_entity_id IN (SELECT id FROM patrol_scans WHERE checkpoint_id IS NULL);

DELETE FROM patrol_scans WHERE checkpoint_id IS NULL;

-- A patrol counts its scans as they are made (scan_count); the ones just deleted must stop counting.
UPDATE patrol_routes r
SET scan_count = (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = r.id)
WHERE r.scan_count <> (SELECT count(*) FROM patrol_scans s WHERE s.patrol_id = r.id);

ALTER TABLE media FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_scans FORCE ROW LEVEL SECURITY;
ALTER TABLE patrol_routes FORCE ROW LEVEL SECURITY;

-- patrol_scans_answers_check goes with its column.
ALTER TABLE patrol_scans
    ALTER COLUMN checkpoint_id SET NOT NULL,
    DROP COLUMN answers;

-- +goose Down
-- The structure comes back; the scans that were deleted do not. A scan keeps no old-style answers.
ALTER TABLE patrol_scans
    ADD COLUMN answers jsonb NOT NULL DEFAULT '[]',
    ADD CONSTRAINT patrol_scans_answers_check CHECK (jsonb_typeof(answers) = 'array'),
    ALTER COLUMN checkpoint_id DROP NOT NULL;
