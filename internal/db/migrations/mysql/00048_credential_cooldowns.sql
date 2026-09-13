-- +goose Up
CREATE TABLE credential_cooldowns (
    cookie_id VARCHAR(255) NOT NULL,
    kind VARCHAR(32) NOT NULL,
    marked_at BIGINT NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (cookie_id, kind),
    CONSTRAINT fk_credential_cooldowns_cookie FOREIGN KEY (cookie_id) REFERENCES cookies(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS credential_cooldowns;
