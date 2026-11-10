-- +goose Up

-- Phase 07.5 Part B: the requests (submit, list, detail, cancel). The decision is Part C, but its
-- columns are here so the table is made once.

CREATE TYPE leave_request_status AS ENUM ('PENDING', 'APPROVED', 'REJECTED', 'CANCELLED');
-- ⚠ Who decided. This is kpi-personalia-api's approval_status 1 (supervisor) vs 2 (HR): it is kept
-- because only an ADMIN may overrule a DANRU's approval.
CREATE TYPE leave_decided_level AS ENUM ('DANRU', 'ADMIN');

-- The attachment of a sick request (a doctor's letter).
ALTER TYPE media_purpose ADD VALUE IF NOT EXISTS 'LEAVE_ATTACHMENT';

-- ---------- leave_requests ----------

CREATE TABLE leave_requests (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id       BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                          REFERENCES organizations(id),
    -- LR / PR / SR + YYYYMM + a 5-digit counter per type (leave_request_counters).
    number                TEXT NOT NULL,
    personnel_id          BIGINT NOT NULL REFERENCES personnels(id),
    -- ⚠ The regu and the project are COPIED: the person may move to another regu later, and the
    -- request keeps the one it was made in (who decides it, whose quota it used).
    team_code             TEXT NOT NULL,
    project_code          TEXT NOT NULL,
    type                  leave_category_type NOT NULL,
    category_id           BIGINT NOT NULL REFERENCES leave_categories(id),
    start_date            DATE NOT NULL,
    end_date              DATE NOT NULL,
    -- Local to the project, for the "part of day" categories only.
    start_time            TIME,
    end_time              TIME,
    -- The days counted against the quota; 0 for a category without one.
    working_days          INTEGER NOT NULL DEFAULT 0,
    reason                TEXT NOT NULL,
    responsibility        TEXT,
    contact_info          TEXT,
    change_day_from       DATE,
    change_day_to         DATE,
    -- Sick leave only.
    hospitalization       BOOLEAN,
    letter_status         BOOLEAN,
    letter_date           DATE,
    disease               TEXT,
    attachment_media_id   BIGINT REFERENCES media(id) ON DELETE SET NULL,

    status                leave_request_status NOT NULL DEFAULT 'PENDING',
    -- The DANRU of the regu at submit. It only drives the notification; the decision is always
    -- checked live against who may decide for the regu today (Part C).
    approver_personnel_id BIGINT REFERENCES personnels(id),
    decided_level         leave_decided_level,
    decided_by_id         BIGINT REFERENCES personnels(id),
    decided_at            timestamptz,
    decision_note         TEXT,
    cancelled_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT leave_requests_number_uq UNIQUE (organization_id, number),
    CONSTRAINT leave_requests_dates_check CHECK (end_date >= start_date),
    CONSTRAINT leave_requests_times_check CHECK ((start_time IS NULL) = (end_time IS NULL)),
    CONSTRAINT leave_requests_change_day_check CHECK (
        (change_day_from IS NULL) = (change_day_to IS NULL) AND (change_day_to IS NULL OR change_day_to >= change_day_from)),
    CONSTRAINT leave_requests_working_days_check CHECK (working_days >= 0),
    CONSTRAINT leave_requests_reason_check CHECK (reason <> ''),
    CONSTRAINT leave_requests_decision_check CHECK (
        (status IN ('APPROVED', 'REJECTED')) = (decided_level IS NOT NULL AND decided_at IS NOT NULL)),
    CONSTRAINT leave_requests_cancel_check CHECK ((status = 'CANCELLED') = (cancelled_at IS NOT NULL))
);
-- The own list, newest first, and the overlap check (a person's live requests by date).
CREATE INDEX leave_requests_personnel_idx ON leave_requests (organization_id, personnel_id, id DESC);
CREATE INDEX leave_requests_personnel_dates_idx ON leave_requests (organization_id, personnel_id, start_date, end_date)
    WHERE status IN ('PENDING', 'APPROVED');
-- The approver's list (Part C) by regu.
CREATE INDEX leave_requests_team_idx ON leave_requests (organization_id, team_code, status, id DESC);

ALTER TABLE leave_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_requests
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON leave_requests TO siapp_app;

-- ---------- counters ----------

-- ⚠ One counter per organization and type, incremented in the submit transaction by one statement
-- (INSERT .. ON CONFLICT DO UPDATE .. RETURNING), which locks the row: two submits at once get
-- different numbers. Not reset every month, as in KPI.
CREATE TABLE leave_request_counters (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    type            leave_category_type NOT NULL,
    counter         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (organization_id, type),
    CONSTRAINT leave_request_counters_check CHECK (counter >= 0)
);
ALTER TABLE leave_request_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_request_counters FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_request_counters
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON leave_request_counters TO siapp_app;

-- ---------- the keys 07.1 and Part A left out ----------

ALTER TABLE attendances
    ADD CONSTRAINT attendances_leave_request_fk FOREIGN KEY (leave_request_id) REFERENCES leave_requests(id);
CREATE INDEX attendances_leave_request_idx ON attendances (leave_request_id) WHERE leave_request_id IS NOT NULL;

ALTER TABLE leave_ledger
    ADD CONSTRAINT leave_ledger_leave_request_fk FOREIGN KEY (leave_request_id) REFERENCES leave_requests(id);

-- +goose Down
ALTER TABLE leave_ledger DROP CONSTRAINT leave_ledger_leave_request_fk;
DROP INDEX attendances_leave_request_idx;
ALTER TABLE attendances DROP CONSTRAINT attendances_leave_request_fk;
DROP TABLE leave_request_counters;
DROP TABLE leave_requests;
DROP TYPE leave_decided_level;
DROP TYPE leave_request_status;
-- An enum value cannot be dropped: LEAVE_ATTACHMENT stays in media_purpose.
