-- Phase 01.3: single-use password reset links.

-- +goose Up
CREATE TABLE password_reset_tokens (
    id         CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id    CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Hex SHA-256 of the token in the emailed link. The token itself is never stored, so a
    -- leaked database cannot be used to reset anyone's password.
    token_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at DATETIME(3) NOT NULL,
    -- Set when the link is used, or when a newer link replaces it.
    used_at    DATETIME(3) NULL,
    created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),

    PRIMARY KEY (id),
    UNIQUE KEY uq_password_reset_tokens_token_hash (token_hash),
    KEY idx_password_reset_tokens_user_id (user_id),
    CONSTRAINT fk_password_reset_tokens_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE password_reset_tokens;
