-- +goose Up

-- Phase 10.8: a zone's checkpoint positions are unique, but checked when the transaction commits.
--
-- Closing the gap a deleted checkpoint leaves means moving the positions above it down by one. With the
-- unique checked row by row, that cannot be one statement (row 3 -> 2 can meet the old 2 before it moves),
-- so it had to be two updates, or a delete and re-insert. Both lock rows the deleting request has no
-- business locking: Postgres re-checks a foreign key when a row that this same transaction already
-- wrote is updated again, and an insert checks it too, and that check takes a share lock on the
-- checkpoint the link points at. A request that holds one of the zone's other checkpoints and waits for
-- the one being deleted then deadlocks with it (found by TestPatrolAssignAndDeleteDoNotDeadlock).
--
-- Deferring the check lets the renumbering be a single UPDATE of rows this transaction has not written,
-- which touches no key column and so checks no foreign key and locks nothing but the link rows. The
-- positions are still unique: a duplicate fails at commit, never reaches another request.
--
-- Nothing may use this constraint as the target of ON CONFLICT or of a foreign key (a deferrable unique
-- cannot be); nothing does.
ALTER TABLE patrol_zone_checkpoints DROP CONSTRAINT patrol_zone_checkpoints_order_uq;
ALTER TABLE patrol_zone_checkpoints
    ADD CONSTRAINT patrol_zone_checkpoints_order_uq UNIQUE (organization_id, zone_id, sort_order)
    DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
ALTER TABLE patrol_zone_checkpoints DROP CONSTRAINT patrol_zone_checkpoints_order_uq;
ALTER TABLE patrol_zone_checkpoints
    ADD CONSTRAINT patrol_zone_checkpoints_order_uq UNIQUE (organization_id, zone_id, sort_order);
