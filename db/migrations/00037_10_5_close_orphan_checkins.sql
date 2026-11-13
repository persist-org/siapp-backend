-- +goose Up

-- A zone that was deleted while someone was checked in to it left that check-in open for ever: the
-- guard could not check out (the zone no longer loads) and one open check-in blocks a check-in in
-- any zone. Close them, at the moment the zone was deleted. From now on deleting a zone closes its
-- check-ins itself.
UPDATE zone_checkins c
SET check_out_at = GREATEST(z.deleted_at, c.check_in_at), updated_at = now(),
    -- The table wants a position with every check-out time: the check-in's own.
    check_out_lat = c.check_in_lat, check_out_lng = c.check_in_lng
FROM patrol_zones z
WHERE z.organization_id = c.organization_id AND z.id = c.zone_id
  AND z.deleted_at IS NOT NULL AND c.check_out_at IS NULL;

-- +goose Down
-- Nothing to undo: the closed check-ins are history now.
SELECT 1;
