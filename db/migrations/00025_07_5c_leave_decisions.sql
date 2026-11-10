-- +goose Up

-- Phase 07.5 Part C: deciding a request and what an approval does to attendance.

-- The outbox row that tells Persist Personel of an approved or revoked leave (D21). aggregate_id is
-- the request id, so the events of one request are delivered in order.
ALTER TYPE outbox_kind ADD VALUE IF NOT EXISTS 'LEAVE';

-- Someone who asked for leave withdraws the request: the DANRU (or the admins) who were told of it
-- are told it is gone.
ALTER TYPE notification_type ADD VALUE IF NOT EXISTS 'LEAVE_CANCELLED';

-- ⚠ late_minutes is what the person is held to; late_minutes_raw is what the clock said at check-in.
-- An approved permit can excuse the lateness (zero it) or take its own window off it, and a
-- permit approved after the check-in, or revoked later, changes it: the raw figure is what the
-- effective one is computed from again, so a revoked permit gives the lateness back.
ALTER TABLE attendances ADD COLUMN late_minutes_raw INTEGER NOT NULL DEFAULT 0;
UPDATE attendances SET late_minutes_raw = late_minutes;
ALTER TABLE attendances ADD CONSTRAINT attendances_late_minutes_raw_check CHECK (late_minutes_raw >= 0);

-- The approver's list reads requests by regu and status.
CREATE INDEX leave_requests_status_idx ON leave_requests (organization_id, status, id DESC);

-- +goose Down
DROP INDEX leave_requests_status_idx;
ALTER TABLE attendances DROP CONSTRAINT attendances_late_minutes_raw_check;
ALTER TABLE attendances DROP COLUMN late_minutes_raw;
-- An enum value cannot be dropped: LEAVE stays in outbox_kind and LEAVE_CANCELLED in notification_type.
