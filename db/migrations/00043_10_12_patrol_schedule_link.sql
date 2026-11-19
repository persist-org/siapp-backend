-- +goose Up

-- Phase 10.12 (docs/plan/10.1-patrol-update.md): a guard starts a patrol from a schedule, and the phone's
-- condition is recorded when the patrol starts and ends.
--
-- A patrol (patrol_routes) is the realization of a schedule: it already holds the person, the project, the start
-- and end, the status, the offline local_id, the walked path and the link from scans, so it gets two columns
-- rather than a parallel table. A patrol still belongs to a project; it has a zone only when it was started from a
-- schedule (a scan in another zone during the patrol still joins it, as before).
ALTER TABLE patrol_routes
    ADD COLUMN zone_id     BIGINT,
    ADD COLUMN schedule_id uuid;

-- The zone is of the patrol's own project (the key of the zone that 10.6 made unique), and the schedule is the
-- organization's. MATCH SIMPLE: a patrol with no zone checks neither.
ALTER TABLE patrol_routes
    ADD CONSTRAINT patrol_routes_zone_fk FOREIGN KEY (organization_id, project_code, zone_id)
        REFERENCES patrol_zones (organization_id, project_code, id),
    ADD CONSTRAINT patrol_routes_schedule_fk FOREIGN KEY (organization_id, schedule_id)
        REFERENCES patrol_schedules (organization_id, id),
    ADD CONSTRAINT patrol_routes_schedule_zone_check CHECK (schedule_id IS NULL OR zone_id IS NOT NULL);

-- One patrol per schedule. The usecase checks it (a clear answer), the database enforces it (two starts at once).
CREATE UNIQUE INDEX patrol_routes_schedule_uq ON patrol_routes (organization_id, schedule_id) WHERE schedule_id IS NOT NULL;
CREATE INDEX patrol_routes_zone_idx ON patrol_routes (organization_id, zone_id, work_date) WHERE zone_id IS NOT NULL;

-- The phone's condition when a patrol started and ended (the app's `device_condition`, an object as in 10.9).
--
-- ⚠ A table, not columns on patrol_routes: it keeps a large, changing JSON out of the row every list reads, and
-- leaves room for more kinds later. It is evidence: nothing updates or deletes a row, and it lives as long as the
-- patrol does (the raw points of 10.4 go after 7 days; this stays). A patrol ended by patrol:auto-end has no END
-- row (the phone is gone).
CREATE TYPE patrol_metadata_kind AS ENUM ('START', 'END');

CREATE TABLE patrol_route_metadata (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                     REFERENCES organizations(id),
    patrol_id        BIGINT NOT NULL,
    kind             patrol_metadata_kind NOT NULL,
    device_condition jsonb NOT NULL,
    -- The phone's clock when it was recorded: the patrol's started_at or ended_at.
    recorded_at      timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, patrol_id) REFERENCES patrol_routes (organization_id, id),
    CONSTRAINT patrol_route_metadata_kind_uq UNIQUE (organization_id, patrol_id, kind),
    CONSTRAINT patrol_route_metadata_object_check CHECK (jsonb_typeof(device_condition) = 'object')
);

ALTER TABLE patrol_route_metadata ENABLE ROW LEVEL SECURITY;
ALTER TABLE patrol_route_metadata FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON patrol_route_metadata
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
-- ⚠ A REVOKE: default privileges already give siapp_app every right on a new table, so a GRANT of less would not
-- take anything away.
REVOKE UPDATE, DELETE ON patrol_route_metadata FROM siapp_app;

-- +goose Down
DROP TABLE patrol_route_metadata;
DROP TYPE patrol_metadata_kind;
DROP INDEX patrol_routes_zone_idx;
DROP INDEX patrol_routes_schedule_uq;
ALTER TABLE patrol_routes
    DROP CONSTRAINT patrol_routes_schedule_zone_check,
    DROP CONSTRAINT patrol_routes_schedule_fk,
    DROP CONSTRAINT patrol_routes_zone_fk,
    DROP COLUMN schedule_id,
    DROP COLUMN zone_id;
