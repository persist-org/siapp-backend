-- +goose Up

-- Phase 08.2: GPS points of the people on shift (location_logs), and the newest point of each
-- person (personnel_last_locations) that the maps read.

-- location_logs: every accepted point, partitioned by month on logged_at
-- =======================================================================
-- A tenant table (RLS). Like audit_logs it is partitioned so that retention is a DROP of whole
-- months, not a DELETE (see drop_location_partitions). siapp_app gets SELECT and INSERT only:
-- a point is never edited, and the only way one disappears is the retention function below.
--
-- * The unique key and the primary key contain logged_at, the partition key (Postgres demands
--   it). UNIQUE (organization_id, personnel_id, logged_at) is what makes a re-sent offline
--   batch harmless: the same point inserted twice is one row.
-- * personnel_id has a foreign key: unlike audit rows, a point has no reason to outlive the
--   person, and the retention window is short.
CREATE TABLE location_logs (
    id              BIGINT GENERATED ALWAYS AS IDENTITY,
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    logged_at       timestamptz NOT NULL,
    latitude        numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude       numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    accuracy_meters numeric(8,2) NOT NULL CHECK (accuracy_meters >= 0),
    battery_pct     smallint CHECK (battery_pct BETWEEN 0 AND 100),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, logged_at),
    UNIQUE (organization_id, personnel_id, logged_at)
) PARTITION BY RANGE (logged_at);

ALTER TABLE location_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE location_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON location_logs
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
REVOKE UPDATE, DELETE ON location_logs FROM siapp_app;
GRANT SELECT, INSERT ON location_logs TO siapp_app;

-- Partitions are made by this function (here for the first months, afterwards by the monthly
-- worker task location:partitions) and removed by drop_location_partitions.
--
-- SECURITY DEFINER for the same reason as ensure_audit_partition (00002): the worker connects
-- as siapp_app, which cannot run DDL. It touches no tenant data, refuses months far from today
-- so a compromised app role cannot create tables without bound, and pins search_path. A
-- partition is queried directly by name too, so each one carries its own RLS and its own revoke.
-- +goose StatementBegin
CREATE FUNCTION ensure_location_partition(month date) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    first_day  date := date_trunc('month', month)::date;
    lo         timestamptz := first_day::timestamp AT TIME ZONE 'UTC';
    hi         timestamptz := (first_day + interval '1 month')::timestamp AT TIME ZONE 'UTC';
    part       text := format('location_logs_%s', to_char(first_day, 'YYYY_MM'));
    this_month date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
    IF first_day < this_month - interval '12 months' OR first_day > this_month + interval '24 months' THEN
        RAISE EXCEPTION 'location partition month % is out of range', first_day;
    END IF;
    IF to_regclass(format('public.%I', part)) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE public.%I PARTITION OF public.location_logs FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', part);
    EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', part);
    EXECUTE format($p$CREATE POLICY org_isolation ON public.%I
        USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
        WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)$p$, part);
    EXECUTE format('REVOKE UPDATE, DELETE ON public.%I FROM siapp_app', part);
    EXECUTE format('GRANT SELECT, INSERT ON public.%I TO siapp_app', part);
END
$$;
-- +goose StatementEnd

-- Drops every monthly partition whose range ends at or before cutoff, and returns how many.
-- Only partitions of location_logs named location_logs_YYYY_MM are considered, so nothing else
-- can be dropped through it. SECURITY DEFINER for the same reason as above.
-- +goose StatementBegin
CREATE FUNCTION drop_location_partitions(cutoff timestamptz) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    part    record;
    dropped integer := 0;
    month   date;
BEGIN
    FOR part IN
        SELECT c.relname AS name
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        JOIN pg_class p ON p.oid = i.inhparent
        JOIN pg_namespace n ON n.oid = p.relnamespace
        WHERE p.relname = 'location_logs' AND n.nspname = 'public' AND c.relname ~ '^location_logs_[0-9]{4}_[0-9]{2}$'
    LOOP
        month := to_date(substr(part.name, length('location_logs_') + 1), 'YYYY_MM');
        IF ((month + interval '1 month')::timestamp AT TIME ZONE 'UTC') <= cutoff THEN
            EXECUTE format('DROP TABLE public.%I', part.name);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    RETURN dropped;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION ensure_location_partition(date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_location_partition(date) TO siapp_app;
REVOKE ALL ON FUNCTION drop_location_partitions(timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION drop_location_partitions(timestamptz) TO siapp_app;

-- The current month and the next three; the monthly task keeps the window moving.
SELECT ensure_location_partition((date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => m))::date)
FROM generate_series(0, 3) AS m;

-- personnel_last_locations: where each person was last seen
-- ===========================================================
-- One row per person, which is what the maps read: scanning location_logs for "the newest point
-- of each member" would not scale. Written by the ingest only, and only forward in time.
CREATE TABLE personnel_last_locations (
    organization_id BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                    REFERENCES organizations(id),
    personnel_id    BIGINT NOT NULL REFERENCES personnels(id),
    logged_at       timestamptz NOT NULL,
    latitude        numeric(9,6) NOT NULL CHECK (latitude BETWEEN -90 AND 90),
    longitude       numeric(9,6) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    accuracy_meters numeric(8,2) NOT NULL CHECK (accuracy_meters >= 0),
    PRIMARY KEY (organization_id, personnel_id)
);

ALTER TABLE personnel_last_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE personnel_last_locations FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON personnel_last_locations
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON personnel_last_locations TO siapp_app;

-- +goose Down
DROP TABLE personnel_last_locations;
DROP FUNCTION drop_location_partitions(timestamptz);
DROP FUNCTION ensure_location_partition(date);
DROP TABLE location_logs;
