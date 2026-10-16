-- +goose Up

CREATE TYPE personnel_status AS ENUM ('ACTIVE', 'INACTIVE');
CREATE TYPE session_client    AS ENUM ('MOBILE', 'WEB');

-- personnels: the people SiAPP works with
-- ========================================
-- A cache of Persist Personel (Personalia) people, filled at login and refreshed by later
-- phases. Personalia owns identity (LOCK F2: "Tak ada user store sendiri"), so this table
-- has NO password, no password hash and no birth date: nothing a credential check or a
-- password reset could need. A leaked copy of it cannot be used to sign in anywhere.
--
-- * personel_user_id is Personalia's m_users.id. A person who moves to another company in
--   Personalia becomes a new row under the new organization (D11): the key is the pair,
--   and each organization only ever sees its own row.
-- * nik is a VARCHAR, not a number: it keeps leading zeros (DL-18), and it is the key
--   Personalia recognises a person by (S4 lookups, attendance push). It is nullable
--   because Personel accounts without an employee number exist (D4).
-- * client_id / client_name are the Personalia client the person is placed at. A client is
--   not a tenant: it is only kept for display and for module 02's project mapping.
CREATE TABLE personnels (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id  BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                     REFERENCES organizations(id),
    personel_user_id BIGINT NOT NULL,
    nik              VARCHAR(32),
    client_id        BIGINT,
    client_name      TEXT,
    full_name        TEXT NOT NULL,
    email            TEXT,
    phone            TEXT,
    photo_url        TEXT,
    status           personnel_status NOT NULL DEFAULT 'ACTIVE',
    synced_at        timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT personnels_org_user_key UNIQUE (organization_id, personel_user_id)
);

CREATE UNIQUE INDEX personnels_org_nik_key ON personnels (organization_id, nik) WHERE nik IS NOT NULL;

ALTER TABLE personnels ENABLE ROW LEVEL SECURITY;
ALTER TABLE personnels FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON personnels
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON personnels TO siapp_app;

-- device_bindings: the one phone a person may use SiAPP on
-- ==========================================================
-- ⚠ This is SiAPP's own record, not Personalia's. The integration login never reads or
-- writes Personalia's t_employee_device, so the Personalia app and SiAPP bind a phone
-- independently and never block each other. HR's "reset device" in the Personalia admin
-- panel therefore frees the Personalia app only; SiAPP's own reset is Phase 01.3.
--
-- At most one row per person (the UNIQUE below). Only the SHA-256 of the device token is
-- stored, never the token. A reset deletes the row; its audit trail is an audit_logs
-- entry, so the row needs no reset_* columns of its own.
CREATE TABLE device_bindings (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id   BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                      REFERENCES organizations(id),
    personnel_id      BIGINT NOT NULL REFERENCES personnels(id) ON DELETE CASCADE,
    device_token_hash CHAR(64) NOT NULL,
    bound_at          timestamptz NOT NULL DEFAULT now(),
    last_seen_at      timestamptz NOT NULL DEFAULT now(),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT device_bindings_org_personnel_key UNIQUE (organization_id, personnel_id)
);

ALTER TABLE device_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE device_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON device_bindings
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON device_bindings TO siapp_app;

-- sessions: one row per refresh token
-- ====================================
-- Every login starts a family (family_id); each refresh retires the row and creates the
-- next one in the same family (replaced_by_id), so a replayed token can end the whole
-- chain (Phase 01.2).
--
-- * refresh_token_hash is the SHA-256 hex of the whole token string. The token is 256 bits
--   of randomness, so a fast hash is right and the plain token is never stored. It is
--   UNIQUE across tenants on purpose: that is a property of random tokens, not data.
-- * device_token_hash is the hash of the device token the session logged in with (NULL for
--   WEB). Phase 01.2 compares it with the person's current binding to notice that the
--   binding was reset or moved.
CREATE TABLE sessions (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id    BIGINT NOT NULL DEFAULT current_setting('app.current_org')::bigint
                       REFERENCES organizations(id),
    personnel_id       BIGINT NOT NULL REFERENCES personnels(id) ON DELETE CASCADE,
    family_id          UUID NOT NULL,
    refresh_token_hash CHAR(64) NOT NULL,
    device_token_hash  CHAR(64),
    remember_me        BOOLEAN NOT NULL DEFAULT false,
    client             session_client NOT NULL,
    expires_at         timestamptz NOT NULL,
    revoked_at         timestamptz,
    replaced_by_id     BIGINT REFERENCES sessions(id),
    user_agent         TEXT,
    ip                 TEXT,
    last_used_at       timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT sessions_refresh_token_hash_key UNIQUE (refresh_token_hash)
);

CREATE INDEX sessions_personnel_idx ON sessions (organization_id, personnel_id);
CREATE INDEX sessions_family_idx    ON sessions (organization_id, family_id);
CREATE INDEX sessions_expires_idx   ON sessions (organization_id, expires_at);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY org_isolation ON sessions
    USING (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_org', true), '')::bigint);
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO siapp_app;

-- +goose Down
DROP TABLE sessions;
DROP TABLE device_bindings;
DROP TABLE personnels;
DROP TYPE session_client;
DROP TYPE personnel_status;
