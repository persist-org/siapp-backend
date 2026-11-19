-- +goose Up

-- Phase 10.11 (docs/plan/10.1-patrol-update.md): an admin schedules a person to patrol a zone in a time
-- window. Phase 10.12 lets the guard start a patrol from the schedule, so a schedule is the plan and the
-- patrol the realization of it.
--
-- * The status of a schedule (SCHEDULED, IN_PROGRESS, COMPLETED, MISSED, CANCELLED) is not stored: it is a
--   function of the clock, of cancelled_at and, from 10.12, of the patrol (scheduleStatus), so a stored one
--   would be stale the moment a window ends.
-- * A schedule is cancelled, never deleted: a patrol will point at it (10.12). The app role cannot DELETE.
-- * The zone and the schedule are of one project (the composite FK), and so are the person's memberships,
--   which the usecase checks on the work date.
CREATE TABLE patrol_schedules (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    project_code    TEXT NOT NULL,
    zone_id         BIGINT NOT NULL,
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    starts_at       timestamptz NOT NULL,
    ends_at         timestamptz NOT NULL,
    -- The date of starts_at in the project's timezone, set by the usecase. A window that crosses midnight
    -- keeps the date it started on, as the shift rule of 10.2 does.
    work_date       DATE NOT NULL,
    notes           TEXT,
    created_by_id   BIGINT NOT NULL REFERENCES personnels(id),
    cancelled_at    timestamptz,
    cancelled_by_id BIGINT REFERENCES personnels(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, project_code, zone_id) REFERENCES patrol_zones (organization_id, project_code, id),
    CONSTRAINT patrol_schedules_org_id_uq UNIQUE (organization_id, id),
    CONSTRAINT patrol_schedules_window_check CHECK (ends_at > starts_at AND ends_at - starts_at <= interval '24 hours'),
    CONSTRAINT patrol_schedules_cancel_check CHECK ((cancelled_at IS NULL) = (cancelled_by_id IS NULL))
);

-- The overlap check and "my schedule" read a person's live schedules by time.
CREATE INDEX patrol_schedules_person_idx ON patrol_schedules (organization_id, personnel_id, starts_at)
    WHERE cancelled_at IS NULL;
CREATE INDEX patrol_schedules_zone_idx ON patrol_schedules (organization_id, project_code, zone_id, work_date);

ALTER TABLE patrol_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_schedules
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- ⚠ A REVOKE: default privileges already give siapp_app every right on a new table, so a GRANT of less
-- would not take anything away.
REVOKE DELETE ON patrol_schedules FROM siapp_app;

-- Who is told that their schedule was made, changed or cancelled.
ALTER TYPE notification_type ADD VALUE IF NOT EXISTS 'PATROL_SCHEDULE_CHANGED';

-- +goose Down
-- A value cannot be removed from an enum: PATROL_SCHEDULE_CHANGED stays (nothing uses it without the table).
DROP TABLE patrol_schedules;
