-- +goose Up

-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS btree_gist; -- exclusion constraints on (id, daterange)
CREATE EXTENSION IF NOT EXISTS pg_trgm;    -- ILIKE '%q%' search indexes
-- +goose StatementEnd

-- Conventions for every migration that follows
-- ============================================
--
-- * Ids are `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`. The one exception is
--   organizations.id, which is Personalia's company id (m_companies.id) and is never
--   generated here (D11).
-- * Timestamps are `timestamptz`, always stored in UTC. Every table has created_at and
--   updated_at.
-- * Every tenant table carries
--       organization_id BIGINT NOT NULL
--           DEFAULT current_setting('app.current_org')::bigint
--           REFERENCES organizations(id)
--   The default reads the setting that WithTenant() sets for the transaction, so an insert
--   cannot forget the tenant. The RLS WITH CHECK below rejects an explicit organization_id
--   that differs from the setting. current_setting() has no missing_ok flag here on purpose:
--   an insert outside WithTenant fails loudly instead of writing a NULL or wrong tenant.
--   Every index on such a table leads with organization_id.
-- * Every tenant table gets, in its own migration:
--       ALTER TABLE x ENABLE ROW LEVEL SECURITY;
--       ALTER TABLE x FORCE ROW LEVEL SECURITY;
--       CREATE POLICY org_isolation ON x
--         USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
--         WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
--       GRANT SELECT, INSERT, UPDATE, DELETE ON x TO siapp_app;
--   The policy reads the setting with missing_ok = true and wraps it in NULLIF(..., ''), so
--   a query outside WithTenant compares against NULL and sees zero rows. NULLIF is needed
--   because once set_config(..., true) has run on a connection, Postgres reports the
--   setting as '' (not NULL) after the transaction ends, and ''::bigint would raise an
--   error instead of hiding the rows.
-- * Why siapp_app must not own the tables: the table owner and any BYPASSRLS role skip row
--   level security. Migrations run as siapp_owner, the API and the worker connect as
--   siapp_app (NOSUPERUSER, NOBYPASSRLS, not the owner). FORCE ROW LEVEL SECURITY closes the
--   remaining gap, because it applies the policies to the owner as well.
--   A guard test fails if a table with an organization_id column lacks RLS, FORCE or the
--   org_isolation policy.

-- The tenant registry. It has no RLS: it is the one table read outside a tenant, because
-- it is what names the tenants.
CREATE TYPE organization_status AS ENUM ('ACTIVE', 'SUSPENDED');

CREATE TABLE organizations (
    id            BIGINT PRIMARY KEY,
    name          TEXT NOT NULL,
    status        organization_status NOT NULL DEFAULT 'ACTIVE',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Tables and sequences the owner creates later are usable by the app role without a
-- separate grant. Row access is still decided by RLS. This must run as the role that runs
-- the later migrations (siapp_owner).
ALTER DEFAULT PRIVILEGES GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO siapp_app;
ALTER DEFAULT PRIVILEGES GRANT USAGE, SELECT ON SEQUENCES TO siapp_app;

-- The registry is narrower than the default: the app can record and update tenants, never
-- delete them.
GRANT SELECT, INSERT, UPDATE ON organizations TO siapp_app;

-- +goose Down
DROP TABLE organizations;
DROP TYPE organization_status;
ALTER DEFAULT PRIVILEGES REVOKE USAGE, SELECT ON SEQUENCES FROM siapp_app;
ALTER DEFAULT PRIVILEGES REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM siapp_app;
