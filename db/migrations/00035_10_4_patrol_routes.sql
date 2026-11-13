-- +goose Up

-- Phase 10.4: the patrol route, the line a guard walks between "start patrol" and "end".
--
-- As in 10.1 to 10.3, the plan's numeric project_id and team_id are codes here (D19).

CREATE TYPE patrol_route_status AS ENUM ('ACTIVE', 'ENDED', 'AUTO_ENDED');

-- patrol_routes: one walk
-- ========================
-- * Its own record, not a view of location_logs (08): location_logs lives 30 days and only while
--   the project tracks, a patrol route is evidence of a patrol and lives as long as its scans.
-- * local_id is the id the app made for the patrol while offline; UNIQUE per organization, so a
--   start sent twice is one row.
-- * team_code and work_date come from the shift rule of 10.2 (patrol.Service.place), not the app.
-- * path is filled when the patrol ends (NULL while ACTIVE): the ordered positions after the noisy
--   ones were dropped and the line simplified, `[{lat, lng, at, acc}]`. The raw points sit in
--   patrol_route_points until retention removes them.
-- * point_count is how many points the phone sent (the raw ones), scan_count how many scans joined
--   the patrol; distance_meters is the length of the stored path.
-- * last_point_at is when the newest point was logged, so patrol:auto-end finds a stale patrol
--   without reading every point of every active patrol. The start counts as the first point.
CREATE TABLE patrol_routes (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                      REFERENCES organizations(id),
    local_id          uuid NOT NULL,
    personnel_id      BIGINT NOT NULL REFERENCES personnels(id),
    team_code         TEXT NOT NULL,
    project_code      TEXT NOT NULL,
    status            patrol_route_status NOT NULL DEFAULT 'ACTIVE',
    started_at        timestamptz NOT NULL,
    ended_at          timestamptz,
    work_date         DATE NOT NULL,
    path              jsonb,
    distance_meters   INTEGER NOT NULL DEFAULT 0 CHECK (distance_meters >= 0),
    point_count       INTEGER NOT NULL DEFAULT 0 CHECK (point_count >= 0),
    scan_count        INTEGER NOT NULL DEFAULT 0 CHECK (scan_count >= 0),
    submitted_offline BOOLEAN NOT NULL DEFAULT false,
    last_point_at     timestamptz NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, team_code) REFERENCES teams (organization_id, code),
    FOREIGN KEY (organization_id, project_code) REFERENCES projects (organization_id, code),
    CONSTRAINT patrol_routes_local_id_uq UNIQUE (organization_id, local_id),
    -- The target of the foreign keys of the points and of patrol_scans.
    CONSTRAINT patrol_routes_org_id_uq UNIQUE (organization_id, id),
    -- Running exactly while there is no end and no path.
    CONSTRAINT patrol_routes_end_check CHECK (
        (status = 'ACTIVE') = (ended_at IS NULL AND path IS NULL)),
    CONSTRAINT patrol_routes_time_check CHECK (ended_at IS NULL OR ended_at >= started_at),
    CONSTRAINT patrol_routes_path_check CHECK (path IS NULL OR jsonb_typeof(path) = 'array')
);
-- One active patrol per person, in any project: a guard walks one round at a time.
CREATE UNIQUE INDEX patrol_routes_active_uq ON patrol_routes (organization_id, personnel_id) WHERE status = 'ACTIVE';
CREATE INDEX patrol_routes_project_idx ON patrol_routes (organization_id, project_code, team_code, work_date);
CREATE INDEX patrol_routes_person_idx ON patrol_routes (organization_id, personnel_id, started_at);
-- patrol:auto-end looks for the active patrols that went quiet.
CREATE INDEX patrol_routes_stale_idx ON patrol_routes (organization_id, last_point_at) WHERE status = 'ACTIVE';
-- patrol:retention looks for the ended patrols that still have raw points.
CREATE INDEX patrol_routes_ended_idx ON patrol_routes (organization_id, ended_at) WHERE status <> 'ACTIVE';

-- patrol_route_points: the raw points received while the patrol runs
-- ====================================================================
-- Points arrive in many small batches, and appending to one jsonb row would rewrite it every time,
-- hence a table. The app numbers its points (seq), so a retried or offline batch is
-- INSERT ... ON CONFLICT DO NOTHING and cannot duplicate or reorder anything. seq 0 is the start.
CREATE TABLE patrol_route_points (
    patrol_id       BIGINT NOT NULL,
    seq             INTEGER NOT NULL CHECK (seq >= 0),
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    logged_at       timestamptz NOT NULL,
    latitude        numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude       numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    accuracy_meters numeric(8,2) NOT NULL CHECK (accuracy_meters >= 0),
    PRIMARY KEY (patrol_id, seq),
    FOREIGN KEY (organization_id, patrol_id) REFERENCES patrol_routes (organization_id, id)
);

-- A scan joins the patrol the guard had running: patrol_scans.patrol_id (10.2) now has its table.
-- MATCH SIMPLE: a scan with no patrol (NULL) is not checked.
ALTER TABLE patrol_scans
    ADD CONSTRAINT patrol_scans_patrol_fk FOREIGN KEY (organization_id, patrol_id) REFERENCES patrol_routes (organization_id, id);
CREATE INDEX patrol_scans_patrol_idx ON patrol_scans (organization_id, patrol_id) WHERE patrol_id IS NOT NULL;

ALTER TABLE patrol_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_routes
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- A patrol is evidence: ended, never deleted.
GRANT SELECT, INSERT, UPDATE ON patrol_routes TO siapp_app;

ALTER TABLE patrol_route_points ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_route_points FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_route_points
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- The raw points are the one thing here that goes: patrol:retention deletes those of a patrol that
-- ended a week ago, once its stored path is the record. They are never updated.
GRANT SELECT, INSERT, DELETE ON patrol_route_points TO siapp_app;

-- +goose Down
ALTER TABLE patrol_scans DROP CONSTRAINT patrol_scans_patrol_fk;
DROP INDEX patrol_scans_patrol_idx;
DROP TABLE patrol_route_points;
DROP TABLE patrol_routes;
DROP TYPE patrol_route_status;
