-- +goose Up

-- Phase 07.7 (fix): approving a leave turns an ABSENT (mangkir) record of those days into LEAVE
-- (KPI's alpa is false under a permit) and used to forget who marked it and why. Revoking the leave
-- then deleted the row and the mangkir was gone. The row now remembers what it was, so a revoke
-- puts the mangkir back.
ALTER TABLE attendances
    ADD COLUMN leave_replaced_absent       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN leave_prev_absent_by_id     BIGINT REFERENCES personnels(id),
    ADD COLUMN leave_prev_absent_reason    TEXT;

-- +goose Down
ALTER TABLE attendances
    DROP COLUMN leave_prev_absent_reason,
    DROP COLUMN leave_prev_absent_by_id,
    DROP COLUMN leave_replaced_absent;
