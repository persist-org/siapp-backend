-- +goose Up

-- Phase 07.5 Part A: leave, permit and sick categories, their quotas, and the annual-leave ledger.
-- SiAPP owns all of it (D2): nothing here is read from or sent to Personalia. The rules copy
-- kpi-personalia-api's (internal/modules/requests); docs/plan/07-attendance.md lists where they
-- differ on purpose. The requests themselves come in Part B.

CREATE TYPE leave_category_type AS ENUM ('LEAVE', 'PERMIT', 'SICK');
CREATE TYPE leave_ledger_kind AS ENUM ('GRANT', 'DEDUCT', 'REFUND', 'ADJUST');

-- ---------- leave_categories ----------

-- ⚠ What is special about a category is a FLAG on its row, never its id or its name. kpi-personalia-api
-- hardcodes ids (1 annual, 3 maternity, 10/11 part of day, 12 full-day duty, 13 change day) and
-- matches the name "Sakit"; ids differ between databases and a rename must not change behavior.
-- A category is deactivated, never deleted (DL-09): a request keeps pointing at it. updated_at is
-- the delta-sync watermark, like every master (DL-34, DL-57): a deactivation moves it.
CREATE TABLE leave_categories (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id     BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                        REFERENCES organizations(id),
    -- The category's identity, as a region's code is (D19): it never changes and is never reused.
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    type                leave_category_type NOT NULL,
    -- Has the ledger: days are deducted when the request is made and refunded when it is rejected
    -- or cancelled (KPI category 1).
    is_annual           BOOLEAN NOT NULL DEFAULT false,
    -- The days of a request are not compared with what is left (KPI category 3, maternity). At
    -- least one unit must still be left.
    skips_day_check     BOOLEAN NOT NULL DEFAULT false,
    -- Needs start_time and end_time, and is the "part of day" overlap class (KPI 10 and 11).
    requires_time_range BOOLEAN NOT NULL DEFAULT false,
    -- Needs change_day_from and change_day_to, and no other category may send them (KPI 13).
    requires_change_day BOOLEAN NOT NULL DEFAULT false,
    -- An approved request writes LEAVE attendance rows for its days. False for the "part of day"
    -- and "on duty" categories: someone on duty elsewhere must still be able to check in.
    makes_leave_days    BOOLEAN NOT NULL DEFAULT true,
    -- A covering approved request zeroes the day's lateness (KPI 12, on duty full day).
    excuses_lateness    BOOLEAN NOT NULL DEFAULT false,
    -- Counts against a quota. False for sick leave.
    has_quota           BOOLEAN NOT NULL DEFAULT true,
    sort_order          INTEGER NOT NULL DEFAULT 0,
    active              BOOLEAN NOT NULL DEFAULT true,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT leave_categories_code_uq UNIQUE (organization_id, code),
    CONSTRAINT leave_categories_code_check CHECK (code ~ '^[A-Z][A-Z0-9_]{1,39}$'),
    CONSTRAINT leave_categories_name_check CHECK (name <> ''),
    -- The ledger holds days of annual leave: sick leave and permits have no ledger.
    CONSTRAINT leave_categories_annual_check CHECK (NOT is_annual OR (type = 'LEAVE' AND has_quota AND makes_leave_days)),
    CONSTRAINT leave_categories_sick_check CHECK (type <> 'SICK' OR NOT has_quota)
);
CREATE INDEX leave_categories_delta_idx ON leave_categories (organization_id, updated_at, id);

-- ---------- quotas ----------

-- The days a client's people may take of a category in a calendar year. client_code NULL is the
-- organization's default; a client's own row wins. ⚠ NO ROW MEANS 0, as in KPI: the category cannot
-- be requested until HR sets a quota. (ANNUAL has no row: its limit is the ledger.)
CREATE TABLE leave_category_quotas (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    category_id     BIGINT NOT NULL REFERENCES leave_categories(id),
    client_code     TEXT,
    quota_days      INTEGER NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, client_code) REFERENCES clients(organization_id, code),
    CONSTRAINT leave_category_quotas_days_check CHECK (quota_days >= 0),
    -- NULLS NOT DISTINCT: there is one default row per category, not any number of them.
    CONSTRAINT leave_category_quotas_uq UNIQUE NULLS NOT DISTINCT (organization_id, category_id, client_code)
);

-- Whether a client's people may request at all (KPI's can_request_leave_permit). No row: enabled.
CREATE TABLE leave_client_settings (
    organization_id  BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                     REFERENCES organizations(id),
    client_code      TEXT NOT NULL,
    requests_enabled BOOLEAN NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, client_code),
    FOREIGN KEY (organization_id, client_code) REFERENCES clients(organization_id, code)
);

-- ---------- leave_ledger ----------

-- The balance of annual leave, in days. APPEND-ONLY (siapp_app has UPDATE and DELETE revoked): a mistake is
-- corrected by an ADJUST row, so the history always adds up. balance_after is the running balance;
-- the balance of a person is the last row's.
--
-- ⚠ The balance is written under the person's row lock (the LockPersonnel query), not by locking
-- the last ledger row: a person's first row has none to lock. kpi-personalia-api writes refunds
-- with the same process_id its attendance module reads as a yearly grant; here the kinds are apart.
--
-- leave_request_id has no foreign key yet: leave_requests comes in Part B, which adds it.
CREATE TABLE leave_ledger (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id  BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                     REFERENCES organizations(id),
    personnel_id     BIGINT NOT NULL REFERENCES personnels(id),
    kind             leave_ledger_kind NOT NULL,
    days             INTEGER NOT NULL,
    balance_after    INTEGER NOT NULL,
    leave_request_id BIGINT,
    note             TEXT NOT NULL DEFAULT '',
    created_by_id    BIGINT REFERENCES personnels(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT leave_ledger_balance_check CHECK (balance_after >= 0),
    CONSTRAINT leave_ledger_days_check CHECK (
        (kind = 'GRANT' AND days > 0) OR (kind = 'REFUND' AND days > 0) OR
        (kind = 'DEDUCT' AND days < 0) OR (kind = 'ADJUST' AND days <> 0)),
    -- A request is deducted once and refunded at most once.
    CONSTRAINT leave_ledger_request_check CHECK ((kind IN ('DEDUCT', 'REFUND')) = (leave_request_id IS NOT NULL))
);
CREATE INDEX leave_ledger_person_idx ON leave_ledger (organization_id, personnel_id, id);
CREATE UNIQUE INDEX leave_ledger_deduct_uq ON leave_ledger (organization_id, leave_request_id) WHERE kind = 'DEDUCT';
CREATE UNIQUE INDEX leave_ledger_refund_uq ON leave_ledger (organization_id, leave_request_id) WHERE kind = 'REFUND';

-- ---------- row-level security ----------

ALTER TABLE leave_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_categories FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_categories
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE ON leave_categories TO siapp_app;

ALTER TABLE leave_category_quotas ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_category_quotas FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_category_quotas
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON leave_category_quotas TO siapp_app;

ALTER TABLE leave_client_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_client_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_client_settings
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE ON leave_client_settings TO siapp_app;

ALTER TABLE leave_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_ledger FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON leave_ledger
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- ⚠ Tables get every privilege for siapp_app by default (00001), so the ledger's append-only rule
-- is a REVOKE, as audit_logs' is: a GRANT of less would not take anything away.
REVOKE UPDATE, DELETE ON leave_ledger FROM siapp_app;

-- +goose Down
DROP TABLE leave_ledger;
DROP TABLE leave_client_settings;
DROP TABLE leave_category_quotas;
DROP TABLE leave_categories;
DROP TYPE leave_ledger_kind;
DROP TYPE leave_category_type;
