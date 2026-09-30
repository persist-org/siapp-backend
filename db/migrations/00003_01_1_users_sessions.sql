-- Phase 01.1: users and login sessions.

-- +goose Up
CREATE TABLE users (
    id                 CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Stored lowercase by the application.
    email              VARCHAR(255) NOT NULL,
    username           VARCHAR(50)  NULL,
    full_name          VARCHAR(150) NOT NULL,
    phone              VARCHAR(30)  NULL,
    -- Nullable because the seeded admin has none. The admin endpoint that creates users
    -- requires it (01.3). It is also the default password of a new user, so it is never
    -- exposed by the API.
    birth_date         DATE NULL,
    system_role        ENUM('ADMIN', 'MEMBER') NOT NULL,
    status             ENUM('ACTIVE', 'DISABLED') NOT NULL DEFAULT 'ACTIVE',
    -- argon2id in PHC string format.
    password_hash      VARCHAR(255) NOT NULL,
    failed_login_count INT NOT NULL DEFAULT 0,
    locked_until       DATETIME(3) NULL,
    last_login_at      DATETIME(3) NULL,
    created_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at         DATETIME(3) NULL,

    -- ⚠ "Unique among non-deleted rows": MySQL has no partial indexes, so each unique value
    -- goes through a stored generated column that is NULL for deleted rows. MySQL allows any
    -- number of NULLs in a UNIQUE index, so a deleted user's email or username can be reused.
    active_email       VARCHAR(255) AS (IF(deleted_at IS NULL, email, NULL)) STORED,
    active_username    VARCHAR(50)  AS (IF(deleted_at IS NULL, username, NULL)) STORED,

    PRIMARY KEY (id),
    UNIQUE KEY uq_users_active_email (active_email),
    UNIQUE KEY uq_users_active_username (active_username),
    KEY idx_users_system_role (system_role)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE sessions (
    id                 CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id            CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- All sessions descended from one login share a family, so a replayed refresh token
    -- can revoke the whole chain (01.2).
    family_id          CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Hex SHA-256 of the refresh token. The token itself is never stored.
    refresh_token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    remember_me        BOOLEAN NOT NULL,
    expires_at         DATETIME(3) NOT NULL,
    revoked_at         DATETIME(3) NULL,
    replaced_by_id     CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    user_agent         VARCHAR(255) NULL,
    ip                 VARCHAR(45)  NULL,
    last_used_at       DATETIME(3) NULL,
    created_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at         DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),

    PRIMARY KEY (id),
    UNIQUE KEY uq_sessions_refresh_token_hash (refresh_token_hash),
    KEY idx_sessions_user_id (user_id),
    KEY idx_sessions_family_id (family_id),
    CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
