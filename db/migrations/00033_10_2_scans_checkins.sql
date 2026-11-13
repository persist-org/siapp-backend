-- +goose Up

-- Phase 10.2: scans of a zone (patrol_scans) and check-in/out of a zone (zone_checkins).
--
-- As in 10.1, the plan's numeric team_id and project_id are codes here (D19).

CREATE TYPE patrol_scan_status AS ENUM ('SUBMITTED', 'APPROVED', 'DECLINED');

-- patrol_scans: one visit to a zone, with the answers the guard gave
-- ===================================================================
-- * local_id is the id the app made for the scan while offline; UNIQUE per organization, so a
--   scan sent twice is one row (the Idempotency-Key middleware answers most replays before they
--   get here; this is the backstop).
-- * zone_type is copied from the zone: a scan keeps meaning what it meant when it was made.
-- * team_code and work_date come from the shift rule (see patrol.Service.Scan), not from the app.
-- * answers is the snapshot of each question with its answer, so a later edit of the checklist
--   never rewrites history. The photos themselves are rows of media attached to the scan.
-- * distance_meters is NULL for a zone without a geofence.
-- * patrol_id points at the patrol route the guard had running (Phase 10.4 creates that table
--   and adds the foreign key). It is set by the server, never sent by the app.
-- * The decision columns are filled by Phase 10.3.
CREATE TABLE patrol_scans (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                      REFERENCES organizations(id),
    local_id          uuid NOT NULL,
    zone_id           BIGINT NOT NULL,
    zone_type         patrol_zone_type NOT NULL,
    personnel_id      BIGINT NOT NULL REFERENCES personnels(id),
    team_code         TEXT NOT NULL,
    project_code      TEXT NOT NULL,
    occurred_at       timestamptz NOT NULL,
    received_at       timestamptz NOT NULL,
    work_date         DATE NOT NULL,
    latitude          numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude         numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    accuracy_meters   numeric(8,2) NOT NULL CHECK (accuracy_meters >= 0),
    distance_meters   INTEGER CHECK (distance_meters >= 0),
    subject           TEXT NOT NULL,
    notes             TEXT,
    answers           jsonb NOT NULL DEFAULT '[]',
    status            patrol_scan_status NOT NULL DEFAULT 'SUBMITTED',
    decided_by_id     BIGINT REFERENCES personnels(id),
    decided_at        timestamptz,
    decision_note     TEXT,
    patrol_id         BIGINT,
    submitted_offline BOOLEAN NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, zone_id) REFERENCES patrol_zones (organization_id, id),
    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT patrol_scans_local_id_uq UNIQUE (organization_id, local_id),
    CONSTRAINT patrol_scans_subject_check CHECK (subject <> ''),
    CONSTRAINT patrol_scans_answers_check CHECK (jsonb_typeof(answers) = 'array'),
    -- Decided exactly when it is not SUBMITTED any more.
    CONSTRAINT patrol_scans_decision_check CHECK (
        (status = 'SUBMITTED') = (decided_at IS NULL AND decided_by_id IS NULL))
);
CREATE INDEX patrol_scans_zone_idx ON patrol_scans (organization_id, zone_id, team_code, work_date);
CREATE INDEX patrol_scans_person_idx ON patrol_scans (organization_id, personnel_id, work_date);
-- The interval rule asks for one person's scans of one zone around a moment.
CREATE INDEX patrol_scans_interval_idx ON patrol_scans (organization_id, personnel_id, zone_id, occurred_at);

-- zone_checkins: a guard present in a zone, from check-in to check-out
-- =====================================================================
-- Feeds the "Check-in Zona" report (DL-44). shift_id is the shift the person was working at the
-- time, when there was one.
CREATE TABLE zone_checkins (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    zone_id         BIGINT NOT NULL,
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    team_code       TEXT NOT NULL,
    shift_id        BIGINT REFERENCES shifts(id),
    check_in_at     timestamptz NOT NULL,
    check_in_lat    numeric(9,6) NOT NULL CHECK (check_in_lat BETWEEN -90 AND 90),
    check_in_lng    numeric(9,6) NOT NULL CHECK (check_in_lng BETWEEN -180 AND 180),
    check_out_at    timestamptz,
    check_out_lat   numeric(9,6) CHECK (check_out_lat BETWEEN -90 AND 90),
    check_out_lng   numeric(9,6) CHECK (check_out_lng BETWEEN -180 AND 180),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, zone_id) REFERENCES patrol_zones (organization_id, id),
    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    CONSTRAINT zone_checkins_out_check CHECK (
        (check_out_at IS NULL) = (check_out_lat IS NULL AND check_out_lng IS NULL)
        AND (check_out_at IS NULL OR check_out_at >= check_in_at))
);
-- One open check-in per person, in any zone: a guard is at one post at a time.
CREATE UNIQUE INDEX zone_checkins_open_uq ON zone_checkins (organization_id, personnel_id) WHERE check_out_at IS NULL;
CREATE INDEX zone_checkins_zone_idx ON zone_checkins (organization_id, zone_id, check_in_at);

ALTER TABLE patrol_scans ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_scans FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_scans
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- Evidence: a scan is decided (10.3) but never deleted.
GRANT SELECT, INSERT, UPDATE ON patrol_scans TO siapp_app;

ALTER TABLE zone_checkins ENABLE ROW LEVEL SECURITY;
ALTER TABLE zone_checkins FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON zone_checkins
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE ON zone_checkins TO siapp_app;

-- +goose Down
DROP TABLE zone_checkins;
DROP TABLE patrol_scans;
DROP TYPE patrol_scan_status;
