-- +goose Up

-- Phase 07.7: shift swap (tukar jaga). The DANRU assigns a replacement for an absent guard; the
-- replacement then holds an extra shift, which takes effect at once (no approval step).

CREATE TABLE shift_swaps (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id        BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                           REFERENCES organizations(id),
    replaced_personnel_id  BIGINT NOT NULL REFERENCES personnels(id),
    replaced_attendance_id BIGINT NOT NULL REFERENCES attendances(id),
    replacing_personnel_id BIGINT NOT NULL REFERENCES personnels(id),
    -- ⚠ Snapshotted like attendances' own shift fields (07.1): the team, shift and times of the
    -- replaced record at the moment of the swap, so a schedule edited later does not change history.
    team_code              TEXT NOT NULL,
    work_date              DATE NOT NULL,
    shift_id               BIGINT NOT NULL REFERENCES shifts(id),
    starts_at              timestamptz NOT NULL,
    ends_at                timestamptz NOT NULL,
    note                   TEXT,
    created_by_id          BIGINT NOT NULL REFERENCES personnels(id),
    cancelled_at           timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    CONSTRAINT shift_swaps_ends_after_start_check CHECK (ends_at > starts_at),
    CONSTRAINT shift_swaps_not_self_check CHECK (replaced_personnel_id <> replacing_personnel_id)
);
-- One ACTIVE swap per absent record (Postgres has no partial UNIQUE constraint, only a partial
-- unique index): a cancelled one keeps its row so history stays, but no longer blocks a fresh swap
-- of the same record.  → SWAP_ALREADY_EXISTS
CREATE UNIQUE INDEX shift_swaps_active_uq ON shift_swaps (organization_id, replaced_attendance_id)
    WHERE cancelled_at IS NULL;

-- The replacer's two tabs ("Digantikan", "Menggantikan") and the candidate/replaceable lookups.
CREATE INDEX shift_swaps_replaced_idx ON shift_swaps (organization_id, replaced_personnel_id, work_date);
CREATE INDEX shift_swaps_replacing_idx ON shift_swaps (organization_id, replacing_personnel_id, work_date);
CREATE INDEX shift_swaps_team_date_idx ON shift_swaps (organization_id, team_code, work_date) WHERE cancelled_at IS NULL;

ALTER TABLE shift_swaps ENABLE ROW LEVEL SECURITY;
ALTER TABLE shift_swaps FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON shift_swaps
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON shift_swaps TO siapp_app;

-- The key 07.1 left out.
ALTER TABLE attendances
    ADD CONSTRAINT attendances_shift_swap_fk FOREIGN KEY (shift_swap_id) REFERENCES shift_swaps(id);
CREATE INDEX attendances_shift_swap_idx ON attendances (shift_swap_id) WHERE shift_swap_id IS NOT NULL;

-- +goose Down
ALTER TABLE attendances DROP CONSTRAINT attendances_shift_swap_fk;
DROP INDEX attendances_shift_swap_idx;
DROP TABLE shift_swaps;
