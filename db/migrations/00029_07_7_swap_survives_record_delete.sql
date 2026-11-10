-- +goose Up

-- Phase 07.7 (fix): a swap's replaced record can be deleted for real: revoking an approved leave
-- deletes its LEAVE rows, and the DANRU's DELETE /attendances/absent/{id} deletes an ABSENT row.
-- Without an ON DELETE rule both failed with a foreign-key violation (a 500) as soon as the record
-- had ever been swapped, a cancelled swap included. The swap keeps its own snapshot (team, date,
-- shift, times), so it survives the delete with the link nulled.

ALTER TABLE shift_swaps ALTER COLUMN replaced_attendance_id DROP NOT NULL;
ALTER TABLE shift_swaps DROP CONSTRAINT shift_swaps_replaced_attendance_id_fkey;
ALTER TABLE shift_swaps
    ADD CONSTRAINT shift_swaps_replaced_attendance_id_fkey
    FOREIGN KEY (replaced_attendance_id) REFERENCES attendances(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE shift_swaps DROP CONSTRAINT shift_swaps_replaced_attendance_id_fkey;
DELETE FROM shift_swaps WHERE replaced_attendance_id IS NULL;
ALTER TABLE shift_swaps
    ADD CONSTRAINT shift_swaps_replaced_attendance_id_fkey
    FOREIGN KEY (replaced_attendance_id) REFERENCES attendances(id);
ALTER TABLE shift_swaps ALTER COLUMN replaced_attendance_id SET NOT NULL;
