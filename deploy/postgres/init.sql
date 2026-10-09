-- Runs once, when the postgres volume is first created (docker-entrypoint-initdb.d).
--
-- siapp_owner (POSTGRES_USER) owns the database and runs migrations.
-- siapp_app is the only role the API, the worker and the tests connect as. It is not the
-- owner, not a superuser and has NOBYPASSRLS, so row-level security always applies to it
-- (LOCK F3, DL-56). Table privileges are granted by the migrations (Phase 00.2), because
-- default privileges are per database and the tests create their own databases.
CREATE ROLE siapp_app LOGIN PASSWORD 'siapp_app'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
GRANT CONNECT ON DATABASE siapp TO siapp_app;
