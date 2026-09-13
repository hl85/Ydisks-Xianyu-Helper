-- +goose Up
-- 发送日计数表：cookie_id 为空字符串时表示多账号合计的全局桶，因此不加外键约束。
CREATE TABLE send_daily_counters (
    cookie_id VARCHAR(255) NOT NULL,
    day VARCHAR(10) NOT NULL,
    sent_count BIGINT NOT NULL DEFAULT 0,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (cookie_id, day)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS send_daily_counters;
