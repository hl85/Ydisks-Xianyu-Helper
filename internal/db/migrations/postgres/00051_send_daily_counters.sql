-- +goose Up
-- 发送日计数表：cookie_id 为空字符串时表示多账号合计的全局桶，因此不加外键约束。
CREATE TABLE send_daily_counters (
    cookie_id TEXT NOT NULL,
    day TEXT NOT NULL,
    sent_count BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (cookie_id, day)
);

-- +goose Down
DROP TABLE IF EXISTS send_daily_counters;
