-- +goose Up

-- Phase 02.3: who serves in which regu, in what field role, and for how long.
--
-- ⚠ History is kept. A membership is never overwritten or deleted: leaving, moving and a role
-- change END a row (end_date) and, for the last two, start another. Past attendance and
-- reports then still find the regu a person served in on that day.
--
-- start_date and end_date are business dates (the project's timezone, DL-03) and end_date is
-- INCLUSIVE: a row with end_date = D still counts on D. NULL means "until further notice".

CREATE TYPE team_role AS ENUM ('DANRU', 'MEMBER');

CREATE TABLE team_members (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    team_code       TEXT NOT NULL,
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    role            team_role NOT NULL,
    start_date      DATE NOT NULL,
    end_date        DATE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    CONSTRAINT team_members_dates_check CHECK (end_date IS NULL OR end_date >= start_date),

    -- A person may hold memberships in several teams and projects at once (§2.9.1), just not
    -- two overlapping ones in the SAME team: that would count them twice. A move or a role
    -- change ends the old row the day before the new one starts, so the two touch but never
    -- overlap.  → MEMBERSHIP_OVERLAP
    CONSTRAINT team_members_no_overlap EXCLUDE USING gist (
        organization_id WITH =, team_code WITH =, personnel_id WITH =,
        daterange(start_date, coalesce(end_date, 'infinity'::date), '[]') WITH &&),

    -- One DANRU per team per date (BR-RULE-02): two rows that are both DANRU may not overlap.
    -- → TEAM_HAS_DANRU
    CONSTRAINT team_members_one_danru EXCLUDE USING gist (
        organization_id WITH =, team_code WITH =,
        daterange(start_date, coalesce(end_date, 'infinity'::date), '[]') WITH &&)
        WHERE (role = 'DANRU')
);

-- "Where does this person serve" (the person's teams on a date) and "who is in this team".
CREATE INDEX team_members_personnel_idx ON team_members (organization_id, personnel_id);

ALTER TABLE team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE team_members FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON team_members
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON team_members TO siapp_app;

-- +goose Down
DROP TABLE team_members;
DROP TYPE team_role;
