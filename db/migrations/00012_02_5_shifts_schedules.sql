-- +goose Up

-- Phase 02.5: shifts and per-person schedules, the way PJ48 (Persist Personalia admin panel)
-- does them: a master list of shifts, weekly shift groups (Monday to Sunday, one shift each),
-- each person's assignment to a group (FIXED) or to no group (FLEXIBLE), and one schedule row
-- per person per date.
--
-- Two things differ from PJ48, because SiAPP knows less and more:
-- * There is no contract end date in SiAPP (Personalia owns it), so a FIXED assignment is
--   generated over a ROLLING window (today + 60 days) that a nightly job keeps extending.
-- * A shift's times are local to where the person works: the timezone of the project of their
--   regu on that date (DL-03), not a fixed Asia/Jakarta.

-- shifts: the master list (PJ48 m_shifts)
-- ========================================
-- Times are minutes after midnight, local to the project. end_minute <= start_minute means the
-- shift runs OVERNIGHT and ends on the next day. An OFF shift is a day without work: it has no
-- times (both 0), and is marked by is_off rather than by its name, as PJ48 did.
CREATE TABLE shifts (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                      REFERENCES organizations(id),
    name              TEXT NOT NULL,
    start_minute      SMALLINT NOT NULL,
    end_minute        SMALLINT NOT NULL,
    is_off            BOOLEAN NOT NULL DEFAULT false,
    tolerance_minutes SMALLINT NOT NULL DEFAULT 15,
    active            BOOLEAN NOT NULL DEFAULT true,
    created_by        BIGINT REFERENCES personnels(id),
    updated_by        BIGINT REFERENCES personnels(id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    CONSTRAINT shifts_minutes_check CHECK (start_minute BETWEEN 0 AND 1439 AND end_minute BETWEEN 0 AND 1439),
    -- A working shift has a length; an OFF day has no times at all.  → SHIFT_INVALID_TIME
    CONSTRAINT shifts_times_check CHECK (
        (is_off AND start_minute = 0 AND end_minute = 0) OR (NOT is_off AND start_minute <> end_minute)),
    CONSTRAINT shifts_tolerance_check CHECK (tolerance_minutes BETWEEN 0 AND 720)
);
-- A name is unique among the shifts that are not deleted (PJ48 checks the same).
CREATE UNIQUE INDEX shifts_org_name_key ON shifts (organization_id, lower(name)) WHERE deleted_at IS NULL;

-- shift_groups + shift_group_days: weekly patterns (PJ48 m_shift_group_master + t_shift_group_detail)
-- ===============================================================================================
-- A group always has exactly seven days, Monday (1) to Sunday (7, ISO), each pointing at one
-- shift (an OFF shift for a day off). Changing a group changes the pattern only: schedules
-- that were already generated stay as they are, as in PJ48; dates generated later follow the
-- new pattern.
CREATE TABLE shift_groups (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    name            TEXT NOT NULL,
    created_by      BIGINT REFERENCES personnels(id),
    updated_by      BIGINT REFERENCES personnels(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);
CREATE UNIQUE INDEX shift_groups_org_name_key ON shift_groups (organization_id, lower(name)) WHERE deleted_at IS NULL;

CREATE TABLE shift_group_days (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    shift_group_id  BIGINT NOT NULL REFERENCES shift_groups(id),
    weekday         SMALLINT NOT NULL,
    shift_id        BIGINT NOT NULL REFERENCES shifts(id),
    PRIMARY KEY (shift_group_id, weekday),
    CONSTRAINT shift_group_days_weekday_check CHECK (weekday BETWEEN 1 AND 7)
);
CREATE INDEX shift_group_days_shift_idx ON shift_group_days (organization_id, shift_id);

-- personnel_shift_assignments: how a person's schedule is made (PJ48 m_employee_work.shift_groub*)
-- ==============================================================================================
-- * FIXED: generated from shift_group_id's weekly pattern from effective_date on.
-- * FLEXIBLE: never generated; an admin enters each date (or imports them).
-- generated_until is the last date generated so far; the nightly job extends it to
-- today + 60 days. One row per person: the history of changes is the audit log.
CREATE TYPE schedule_type AS ENUM ('FIXED', 'FLEXIBLE');

CREATE TABLE personnel_shift_assignments (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    schedule_type   schedule_type NOT NULL,
    shift_group_id  BIGINT REFERENCES shift_groups(id),
    effective_date  DATE NOT NULL,
    generated_until DATE,
    updated_by      BIGINT REFERENCES personnels(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, personnel_id),
    CONSTRAINT personnel_shift_assignments_group_check CHECK (
        (schedule_type = 'FIXED' AND shift_group_id IS NOT NULL) OR
        (schedule_type = 'FLEXIBLE' AND shift_group_id IS NULL))
);
CREATE INDEX personnel_shift_assignments_group_idx ON personnel_shift_assignments (organization_id, shift_group_id);

-- schedules: one shift per person per date (PJ48 m_schedules)
-- ============================================================
-- work_date is the business date the shift STARTS on, in the project's timezone; an overnight
-- shift ends on work_date + 1. shift_group_id is the group the row was generated from, NULL
-- for a row an admin entered by hand. Rows are deleted for real (as PJ48's generator does):
-- who removed what is in the audit log.
CREATE TABLE schedules (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    work_date       DATE NOT NULL,
    shift_id        BIGINT NOT NULL REFERENCES shifts(id),
    shift_group_id  BIGINT REFERENCES shift_groups(id),
    created_by      BIGINT REFERENCES personnels(id),
    updated_by      BIGINT REFERENCES personnels(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- One schedule per person per date.  → SCHEDULE_EXISTS
    CONSTRAINT schedules_personnel_date_key UNIQUE (organization_id, personnel_id, work_date)
);
CREATE INDEX schedules_date_idx ON schedules (organization_id, work_date);
CREATE INDEX schedules_shift_idx ON schedules (organization_id, shift_id);

ALTER TABLE shifts ENABLE ROW LEVEL SECURITY;
ALTER TABLE shifts FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON shifts
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON shifts TO siapp_app;

ALTER TABLE shift_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE shift_groups FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON shift_groups
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON shift_groups TO siapp_app;

ALTER TABLE shift_group_days ENABLE ROW LEVEL SECURITY;
ALTER TABLE shift_group_days FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON shift_group_days
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON shift_group_days TO siapp_app;

ALTER TABLE personnel_shift_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE personnel_shift_assignments FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON personnel_shift_assignments
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON personnel_shift_assignments TO siapp_app;

ALTER TABLE schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE schedules FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON schedules
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON schedules TO siapp_app;

-- +goose Down
DROP TABLE schedules;
DROP TABLE personnel_shift_assignments;
DROP TYPE schedule_type;
DROP TABLE shift_group_days;
DROP TABLE shift_groups;
DROP TABLE shifts;
