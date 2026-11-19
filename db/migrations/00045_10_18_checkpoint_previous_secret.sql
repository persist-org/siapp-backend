-- +goose Up
-- Phase 10.18 (docs/plan/10.3-patrol-mobile-feedback.md, BE-CP8): rotating a checkpoint's QR no longer
-- throws away offline scans made before the rotation.
--
-- A phone scans a sticker with no signal; the scan syncs hours later. If the QR was rotated in between, the
-- scan carries the old secret and was refused CHECKPOINT_QR_INVALID, although it was made while the old
-- sticker was the right one. The checkpoint now keeps the secret it had before the last rotation, and a scan
-- whose occurred_at is before qr_rotated_at may use it. Only one generation back: a second rotation
-- overwrites it.
--
-- ⚠ It is a secret like qr_secret: no read of the API returns it (the no-secret tests cover both).
ALTER TABLE patrol_checkpoints
    ADD COLUMN previous_qr_secret TEXT,
    ADD CONSTRAINT patrol_checkpoints_previous_secret_check CHECK (previous_qr_secret IS NULL OR length(previous_qr_secret) = 22);

-- +goose Down
ALTER TABLE patrol_checkpoints
    DROP CONSTRAINT patrol_checkpoints_previous_secret_check,
    DROP COLUMN previous_qr_secret;
