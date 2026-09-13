-- +goose Up
CREATE TABLE credential_cooldowns (
    cookie_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    marked_at INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (cookie_id, kind),
    FOREIGN KEY (cookie_id) REFERENCES cookies(id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE IF EXISTS credential_cooldowns;
