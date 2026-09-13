-- +goose Up
CREATE TABLE credential_cooldowns (
    cookie_id TEXT NOT NULL REFERENCES cookies(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    marked_at BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (cookie_id, kind)
);

-- +goose Down
DROP TABLE IF EXISTS credential_cooldowns;
