-- +goose Up
-- 进程心跳表：应用内心跳写者周期性写入当前时间戳（整秒），供宿主侧
-- （Docker healthcheck / systemd / 运维脚本）判断「这个进程到底还活着吗」。
-- instance_key 区分同一数据库上的多个进程实例；单实例部署统一用默认键。
-- 本表只记录本进程存活信号，与任何平台（闲鱼）业务数据无关。
CREATE TABLE process_heartbeats (
    instance_key VARCHAR(255) NOT NULL,
    last_beat_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (instance_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS process_heartbeats;
