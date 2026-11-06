-- +goose Up

-- Phase 07.1: the attendance record SiAPP captures (selfie now, QR in 07.2) and the outbox that
-- carries every change of it to Persist Personel (delivery is 07.3).

CREATE TYPE attendance_status AS ENUM ('PRESENT', 'LEAVE', 'ABSENT');
CREATE TYPE attendance_method AS ENUM ('QR', 'SELFIE', 'GROUP', 'FORCE');
CREATE TYPE personel_sync_status AS ENUM ('PENDING', 'SYNCED', 'FAILED');
CREATE TYPE outbox_kind AS ENUM ('ATTENDANCE');
CREATE TYPE outbox_status AS ENUM ('PENDING', 'SENT', 'DEAD');

-- ---------- attendances ----------

-- ⚠ This row is SiAPP's CAPTURE AND EVIDENCE of an attendance (the selfie, the GPS fix, the QR's
-- DANRU, the distance to the site): it is NOT the official record. Persist Personel holds that
-- (LOCK F5). Every change to a row here writes a personel_outbox row in the same transaction, and
-- Personel receives it asynchronously (07.3), so a Personel outage never blocks a guard.
--
-- One row per SHIFT INSTANCE: person x regu x work date x shift. Check-in, leave and absence all
-- write the same row, and every SiAPP view reads it. The regu is the one the person serves in on
-- work_date (a person can serve in two regu on one day, and then has two instances). Regu and
-- project are named by code (D19), the shift by id.
CREATE TABLE attendances (
    id                       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id          BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                             REFERENCES organizations(id),
    personnel_id             BIGINT NOT NULL REFERENCES personnels(id),
    team_code                TEXT NOT NULL,
    project_code             TEXT NOT NULL,
    -- The business date the shift STARTS on, in the project's timezone (DL-03).
    work_date                DATE NOT NULL,
    shift_id                 BIGINT NOT NULL REFERENCES shifts(id),
    -- ⚠ The shift's times and the timezone are COPIED here, not read from the shift and the project
    -- later: an admin may edit a shift template or move a project to another zone tomorrow, and
    -- the history of what was expected of a guard on a past day must not change with it.
    shift_starts_at          timestamptz NOT NULL,
    shift_ends_at            timestamptz NOT NULL,
    timezone                 TEXT NOT NULL,
    status                   attendance_status NOT NULL,

    check_in_at              timestamptz,
    check_in_method          attendance_method,
    check_in_media_id        BIGINT REFERENCES media(id) ON DELETE SET NULL,
    check_in_latitude        NUMERIC(9, 6),
    check_in_longitude       NUMERIC(9, 6),
    check_in_accuracy_meters INTEGER,
    check_in_distance_meters INTEGER,
    -- NULL when nothing was measured (a QR check-in without a fix, 07.2).
    check_in_within_area     BOOLEAN,
    -- The DANRU whose QR the check-in scanned (07.2).
    check_in_qr_danru_id     BIGINT REFERENCES personnels(id),
    -- ⚠ Whole minutes the check-in came after shift_starts_at; 0 when on time. A check-in inside the
    -- shift's tolerance_minutes counts as on time; past it, the lateness is counted from the start
    -- of the shift, not from the end of the tolerance. Stored, not derived on read, for the same
    -- reason the shift times are copied: the tolerance of the shift can be edited later, and what
    -- was counted for a past day must not change with it. A late guard is never refused.
    late_minutes             INTEGER NOT NULL DEFAULT 0,

    check_out_at             timestamptz,
    check_out_method          attendance_method,
    check_out_media_id        BIGINT REFERENCES media(id) ON DELETE SET NULL,
    check_out_latitude        NUMERIC(9, 6),
    check_out_longitude       NUMERIC(9, 6),
    check_out_accuracy_meters INTEGER,
    check_out_distance_meters INTEGER,
    check_out_within_area     BOOLEAN,
    check_out_qr_danru_id     BIGINT REFERENCES personnels(id),
    -- How many times "update pulang" overwrote the check-out.
    check_out_update_count    INTEGER NOT NULL DEFAULT 0,

    -- The leave request (07.5) and the swap (07.7) have no table yet, so these two carry no foreign
    -- key until their phase adds it.
    leave_request_id         BIGINT,
    marked_absent_by_id      BIGINT REFERENCES personnels(id),
    shift_swap_id            BIGINT,

    personel_sync_status     personel_sync_status NOT NULL DEFAULT 'PENDING',
    personel_synced_at       timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT attendances_instance_uq UNIQUE (organization_id, personnel_id, team_code, work_date, shift_id),
    CONSTRAINT attendances_ends_after_start_check CHECK (shift_ends_at > shift_starts_at),
    -- A present record has its check-in; a leave or absence has none.
    CONSTRAINT attendances_present_check CHECK (
        (status = 'PRESENT') = (check_in_at IS NOT NULL AND check_in_method IS NOT NULL)),
    CONSTRAINT attendances_check_out_check CHECK (
        check_out_at IS NULL OR (check_in_at IS NOT NULL AND check_out_method IS NOT NULL)),
    CONSTRAINT attendances_update_count_check CHECK (check_out_update_count >= 0),
    CONSTRAINT attendances_late_minutes_check CHECK (late_minutes >= 0)
);
CREATE INDEX attendances_team_date_idx ON attendances (organization_id, team_code, work_date);
CREATE INDEX attendances_personnel_date_idx ON attendances (organization_id, personnel_id, work_date);

ALTER TABLE attendances ENABLE ROW LEVEL SECURITY;
ALTER TABLE attendances FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON attendances
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON attendances TO siapp_app;

-- ---------- personel_outbox ----------

-- The transactional outbox towards Persist Personel: a row is written in the same transaction as
-- the change it reports, so a change is never lost and never reported twice. The worker that
-- delivers them is Phase 07.3; until then rows stay PENDING. event_id is what Personel receives as
-- its idempotency key, so a delivery that is repeated after a lost answer is a duplicate there.
CREATE TABLE personel_outbox (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    event_id        uuid NOT NULL DEFAULT gen_random_uuid(),
    kind            outbox_kind NOT NULL,
    -- The attendance id (kind ATTENDANCE). Events of one aggregate are delivered in order.
    aggregate_id    BIGINT NOT NULL,
    payload         jsonb NOT NULL,
    status          outbox_status NOT NULL DEFAULT 'PENDING',
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      TEXT,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT personel_outbox_event_uq UNIQUE (event_id),
    CONSTRAINT personel_outbox_attempts_check CHECK (attempts >= 0)
);
CREATE INDEX personel_outbox_due_idx ON personel_outbox (organization_id, status, next_attempt_at);
CREATE INDEX personel_outbox_aggregate_idx ON personel_outbox (organization_id, kind, aggregate_id, id);

ALTER TABLE personel_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE personel_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON personel_outbox
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON personel_outbox TO siapp_app;

-- +goose Down
DROP TABLE personel_outbox;
DROP TABLE attendances;
DROP TYPE outbox_status;
DROP TYPE outbox_kind;
DROP TYPE personel_sync_status;
DROP TYPE attendance_method;
DROP TYPE attendance_status;
