-- +goose Up

-- audit_logs: append-only record of sensitive writes
-- ===================================================
-- A tenant table (RLS), partitioned by month on created_at. It has no updated_at: rows are
-- never changed.
--
-- * No foreign key on actor_personnel_id (or on entity_id): an audit row must outlive the
--   rows it describes. A personnel who is deleted or anonymised later must not take their
--   history, or block their own deletion, through a constraint.
-- * siapp_app gets INSERT and SELECT only. Without an UPDATE or DELETE grant the table is
--   append-only at the database level, whatever the application code does. The default
--   privileges from migration 00001 grant DML on every new table, so they are revoked
--   explicitly here and on every partition.
-- * The primary key includes created_at because a partitioned table's unique keys must
--   contain the partition key.
CREATE TABLE audit_logs (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY,
    organization_id    BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                       REFERENCES organizations(id),
    actor_personnel_id BIGINT,
    action             TEXT NOT NULL,
    entity_type        TEXT NOT NULL,
    entity_id          BIGINT NOT NULL,
    metadata           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX audit_logs_entity_idx ON audit_logs (organization_id, entity_type, entity_id, created_at);
CREATE INDEX audit_logs_actor_idx  ON audit_logs (organization_id, actor_personnel_id, created_at);

ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON audit_logs
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
REVOKE UPDATE, DELETE ON audit_logs FROM siapp_app;
GRANT SELECT, INSERT ON audit_logs TO siapp_app;

-- Partitions are created by this function, here for the first three months and afterwards
-- by the monthly worker task audit:partitions.
--
-- SECURITY DEFINER because the worker connects as siapp_app, which cannot run DDL. It is
-- the one exception to "no SECURITY DEFINER functions": it touches no tenant data, only
-- creates (or finds) a partition of audit_logs with the same protections as the parent,
-- and refuses months far from today, so a compromised app role cannot create tables
-- without bound. search_path is pinned so a caller cannot shadow the objects it uses.
--
-- A partition is queried directly by name too, so each one needs its own RLS and its own
-- revoke: privileges and policies of the parent do not apply to a direct query.
-- +goose StatementBegin
CREATE FUNCTION ensure_audit_partition(month date) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    first_day date := date_trunc('month', month)::date;
    lo        timestamptz := first_day::timestamp AT TIME ZONE 'UTC';
    hi        timestamptz := (first_day + interval '1 month')::timestamp AT TIME ZONE 'UTC';
    part      text := format('audit_logs_%s', to_char(first_day, 'YYYY_MM'));
    this_month date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
    IF first_day < this_month - interval '12 months' OR first_day > this_month + interval '24 months' THEN
        RAISE EXCEPTION 'audit partition month % is out of range', first_day;
    END IF;
    IF to_regclass(format('public.%I', part)) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE public.%I PARTITION OF public.audit_logs FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
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

REVOKE ALL ON FUNCTION ensure_audit_partition(date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION ensure_audit_partition(date) TO siapp_app;

SELECT ensure_audit_partition((date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => m))::date)
FROM generate_series(0, 2) AS m;

-- idempotency_keys
-- ================
-- One row per (actor, Idempotency-Key). The row is inserted when the first request starts,
-- with status_code NULL (in flight), and completed with the response when the handler
-- returns. Two concurrent first requests therefore race on the primary key: one inserts,
-- the other sees the row and waits for it. A row that never completes (the process died)
-- is taken over after a timeout; a 5xx, 409 or 429 response deletes the row so a retry
-- really runs again. See pkg/platform/idem.
CREATE TABLE idempotency_keys (
    organization_id    BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                       REFERENCES organizations(id),
    actor_personnel_id BIGINT NOT NULL,
    key                uuid NOT NULL,
    method             TEXT NOT NULL,
    path               TEXT NOT NULL,
    request_hash       BYTEA NOT NULL,
    status_code        INTEGER,
    response_body      JSONB,
    created_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, actor_personnel_id, key)
);

-- Cleanup runs per organization and deletes by age.
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (organization_id, created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON idempotency_keys
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO siapp_app;

-- +goose Down
DROP TABLE idempotency_keys;
DROP FUNCTION ensure_audit_partition(date);
DROP TABLE audit_logs;
