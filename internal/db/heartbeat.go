// heartbeat.go 提供进程心跳时间戳的持久化读写，供应用内写者与宿主侧检查共用。
//
// 本仓储只读写我们自己的 process_heartbeats 表，不依赖任何平台（闲鱼）实现，
// 因此心跳绝不发起任何外部平台请求：不发消息、不调 mtop、不刷新 cookie、
// 不重新登录、不重连 WebSocket。写者只允许更新内存时间戳 + 写自有数据库（+ 可选日志）。

package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// HeartbeatStore 提供进程心跳的写入与读取能力；由应用内心跳写者与宿主检查入口共用。
// 本类型只触碰 process_heartbeats 表，不持有也不调用任何平台客户端。
type HeartbeatStore struct {
	// DB 是共享的数据库连接池，由 Store 统一持有与关停。
	DB *sql.DB
	// Dialect 保留方言标识；当前 SQL 三方言一致，暂不分支。
	Dialect Dialect
	// InstanceKey 区分同一数据库上的多个进程实例；单实例部署使用默认键。
	InstanceKey string
}

// defaultHeartbeatInstanceKey 是未指定实例键时使用的默认进程键。
const defaultHeartbeatInstanceKey = "default"

// errHeartbeatNotInitialized 表示心跳仓储未注入数据库连接，调用前应确保已装配。
var errHeartbeatNotInitialized = errors.New("心跳仓储未初始化")

// NewHeartbeatStore 构造进程心跳仓储；database 为 nil 时返回 nil，
// 调用方据此跳过心跳写者启动。instanceKey 为空字符串时回落默认键。
func NewHeartbeatStore(database *sql.DB, dialect Dialect, instanceKey string) *HeartbeatStore {
	if database == nil {
		return nil
	}
	// key 是归一化后的实例键；空白键统一回落默认键，避免产生空主键行。
	key := strings.TrimSpace(instanceKey)
	if key == "" {
		key = defaultHeartbeatInstanceKey
	}
	return &HeartbeatStore{DB: database, Dialect: dialect, InstanceKey: key}
}

// Beat 把给定时刻 at 写入本实例的心跳行；先更新已有行，无行时插入单行。
// at 由调用方注入的时钟提供（Unix 整秒），写库前不依赖数据库侧 CURRENT_TIMESTAMP，
// 保证测试可确定性地驱动心跳时间与判定。写失败直接上抛，由写者按 fail-open 降级处理。
func (s *HeartbeatStore) Beat(ctx context.Context, at time.Time) error {
	if s == nil || s.DB == nil {
		return errHeartbeatNotInitialized
	}
	// unix 是把注入时钟换算成的整秒时间戳；统一按 UTC 整秒存储，跨方言无需文本格式解析。
	unix := at.Unix()
	// result 保存 UPDATE 的命中结果；命中 0 行说明本实例尚无记录，需要插入。
	result, err := s.DB.ExecContext(ctx,
		`UPDATE process_heartbeats SET last_beat_at = ?, updated_at = ? WHERE instance_key = ?`,
		unix, unix, s.InstanceKey)
	if err != nil {
		return err
	}
	// affected 表示本实例是否已有记录；大于 0 时更新已落库，直接返回。
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}
	// 首次写入：插入本实例单行；单实例部署下主键唯一保证不会冲突。
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO process_heartbeats (instance_key, last_beat_at, updated_at) VALUES (?, ?, ?)`,
		s.InstanceKey, unix, unix); err != nil {
		return err
	}
	return nil
}

// Latest 读取本实例最近一次心跳的整秒时间戳；数据库尚无记录时返回零值 time.Time。
// 宿主侧检查入口据此判断进程是否存活、是否已超过容忍窗口。
func (s *HeartbeatStore) Latest(ctx context.Context) (time.Time, error) {
	if s == nil || s.DB == nil {
		return time.Time{}, errHeartbeatNotInitialized
	}
	// ts 保存本实例最近一次心跳的整秒时间戳；无记录时 Scan 报 ErrNoRows，按零值处理。
	var ts sql.NullInt64
	// err 是读取本实例心跳的错误；除无记录外的失败都上抛给调用方。
	err := s.DB.QueryRowContext(ctx,
		`SELECT last_beat_at FROM process_heartbeats WHERE instance_key = ?`,
		s.InstanceKey).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !ts.Valid {
		return time.Time{}, nil
	}
	// beat 把整秒时间戳还原为 UTC 时间，供调用方做窗口比较。
	beat := time.Unix(ts.Int64, 0).UTC()
	return beat, nil
}
