-- +goose Up

-- Phase 07.6 Part A: the DANRU marking a team member absent (mangkir) directly, and the one new
-- column that capture needs. marked_absent_by_id already exists (07.1) and was unused until now.

ALTER TABLE attendances ADD COLUMN absent_reason TEXT;

-- +goose Down
ALTER TABLE attendances DROP COLUMN absent_reason;
