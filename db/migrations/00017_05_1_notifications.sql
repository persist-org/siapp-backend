-- +goose Up

-- Phase 05.1: the in-app inbox. One row per notification per recipient: a message to a whole regu
-- is as many rows as it has people, because each person reads, and so changes, only their own.
--
-- The 12 types are Tabel 7 of the manual (List Notifikasi); the last four are implied by the
-- manual's flows (a leave decision, instruction feedback, a swap, the end of a panic). A type
-- that a later phase needs is a new migration: ALTER TYPE ... ADD VALUE.
CREATE TYPE notification_type AS ENUM (
    'LEAVE_REQUESTED', 'ABSENCE_MARKED', 'REPORT_SUBMITTED', 'INSTRUCTION_RECEIVED',
    'TEAM_CHANGED', 'POSITION_CHANGED', 'PATROL_SUBMITTED', 'PATROL_APPROVED',
    'GUARD_SUBMITTED', 'GUARD_APPROVED', 'PANIC_TRIGGERED', 'OFFLINE_SUBMISSION_SENT',
    'LEAVE_DECIDED', 'INSTRUCTION_FEEDBACK', 'SWAP_ASSIGNED', 'PANIC_RESOLVED');

-- HIGH is for panic only. Phase 05.2 sends it through the critical queue and the siren channel.
CREATE TYPE notification_priority AS ENUM ('NORMAL', 'HIGH');

CREATE TABLE notifications (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id        BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                           REFERENCES organizations(id),
    recipient_personnel_id BIGINT NOT NULL REFERENCES personnels(id),
    type                   notification_type NOT NULL,
    -- Rendered when the row is written (pkg/notifications Render), in Indonesian, with the labels
    -- of the project's business line. Stored, not recomputed: an inbox shows what was sent.
    title                  TEXT NOT NULL,
    body                   TEXT NOT NULL,
    -- The record the notification is about, so the app can open it. Both or neither.
    entity_type            TEXT,
    entity_id              BIGINT,
    -- Extra deep-link payload. Ids and codes, never a NIK or a full name (DL-17).
    data                   jsonb NOT NULL DEFAULT '{}',
    priority               notification_priority NOT NULL DEFAULT 'NORMAL',
    read_at                timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- ⚠ updated_at is the delta-sync watermark column (DL-34, DL-57): a row is sent to the app
    -- when it changed after the app's `since`. Whatever changes a row, reading it or deleting it,
    -- must set updated_at in the same statement, or the app never hears about the change.
    updated_at             timestamptz NOT NULL DEFAULT now(),
    -- A tombstone: the row stays so a delta pull can tell the app to drop it. Nothing in this phase
    -- deletes; retention, when it comes, sets deleted_at AND updated_at, and purges old tombstones.
    deleted_at             timestamptz,
    CONSTRAINT notifications_text_check CHECK (title <> '' AND body <> ''),
    CONSTRAINT notifications_entity_check CHECK ((entity_type IS NULL) = (entity_id IS NULL)),
    CONSTRAINT notifications_data_check CHECK (jsonb_typeof(data) = 'object')
);

-- The inbox and its counter: one person's rows, unread first or newest first.
CREATE INDEX notifications_inbox_idx ON notifications
    (organization_id, recipient_personnel_id, read_at, created_at);
-- Delta sync walks one person's rows in (updated_at, id) order; the id makes the keyset page
-- boundary exact when several rows share a timestamp (one Notify writes them with the same one).
CREATE INDEX notifications_delta_idx ON notifications
    (organization_id, recipient_personnel_id, updated_at, id);

ALTER TABLE notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON notifications
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON notifications TO siapp_app;

-- +goose Down
DROP TABLE notifications;
DROP TYPE notification_priority;
DROP TYPE notification_type;
