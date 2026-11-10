-- +goose Up

-- Phase 07.6 (fix): group check-in records presence without each guard's own evidence, which
-- weakens the anti-fraud purpose of QR and selfie (manual §2.6.1). Off by default; a project opts
-- in explicitly.

ALTER TABLE projects ADD COLUMN allow_group_attendance BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE projects DROP COLUMN allow_group_attendance;
