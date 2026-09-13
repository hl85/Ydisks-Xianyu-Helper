// send_counter.go 出站发送日计数的持久化仓储。
// 表结构见 00050_send_daily_counters：以 (cookie_id, day) 为主键，按本地自然日分桶记录发送条数。
// cookie_id 允许为空字符串，表示多账号合计的全局桶；因此该表不建外键约束。
// 供进程重启后恢复账号与全局的当日用量，避免额度因重启被清零放大。

package db

import (
	"context"
	"database/sql"
	"errors"
)

// sendCounterMaxInsertRetries 是并发首插冲突时的有界重试次数；
// 单进程部署下几乎不会触发，多进程偶发竞争时重试即可收敛。
const sendCounterMaxInsertRetries = 3

// errSendCounterInsertConflict 表示插入仍与并发写入冲突，需要外层重试。
var errSendCounterInsertConflict = errors.New("发送计数并发插入冲突")

// SendCounterStore 提供发送日计数的读取与累加。
type SendCounterStore struct {
	// DB 是共享的数据库连接池，由 Store 统一持有与关停。
	DB *sql.DB
	// Dialect 保留方言标识；当前实现只用三条方言一致的 SQL，暂不分支。
	Dialect Dialect
}

// GetSendCount 读取指定账号（或空字符串全局桶）在指定自然日已登记的发送条数。
// ctx 是调用方取消边界；无记录时返回 0 和 nil，调用方无需区分「无记录」与「零条」。
func (s *SendCounterStore) GetSendCount(ctx context.Context, cookieID, day string) (int, error) {
	// total 保存查询到的当日累计条数；无记录时 Scan 报 ErrNoRows，按 0 处理。
	var total int64
	// err 是当日计数查询错误；除无记录外的查询失败都上抛给调用方。
	err := s.DB.QueryRowContext(ctx,
		`SELECT sent_count FROM send_daily_counters WHERE cookie_id = ? AND day = ?`,
		cookieID, day).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(total), nil
}

// AddSendCount 在事务内以「先更新后插入」累加指定桶的当日计数，并返回累加后的总条数。
// 写法与三条方言兼容：只使用 ? 占位符与标准错误判断；并发首插冲突时有界重试。
func (s *SendCounterStore) AddSendCount(ctx context.Context, cookieID, day string, delta int) (int64, error) {
	// lastErr 保留最近一次冲突错误，重试耗尽后上抛给调用方（调用方按 fail-open 降级）。
	var lastErr error
	// attempt 是当前「先更新后插入」事务尝试的序号，仅用于限制冲突重试次数。
	for attempt := 0; attempt < sendCounterMaxInsertRetries; attempt++ {
		// total、err 是单次事务尝试得到的总条数与错误。
		total, err := s.addSendCountOnce(ctx, cookieID, day, delta)
		if err == nil {
			return total, nil
		}
		if !errors.Is(err, errSendCounterInsertConflict) {
			return 0, err
		}
		lastErr = err
	}
	return 0, lastErr
}

// addSendCountOnce 执行一次「先更新后插入」事务尝试；并发首插冲突时返回哨兵错误触发重试。
func (s *SendCounterStore) addSendCountOnce(ctx context.Context, cookieID, day string, delta int) (int64, error) {
	// tx 是单次累加事务；任一步失败即整体回滚，不留下半程计数。
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// result 是更新语句的结果，用于判断是否已有当日记录。
	result, err := tx.ExecContext(ctx,
		`UPDATE send_daily_counters SET sent_count = sent_count + ?, updated_at = CURRENT_TIMESTAMP WHERE cookie_id = ? AND day = ?`,
		delta, cookieID, day)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	// affected 是更新命中的行数；大于 0 说明记录已存在，读回总条数即可提交。
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if affected > 0 {
		// total 保存累加后的当日总条数。
		var total int64
		// scanErr 是事务内读回总条数的错误；读不回即回滚，避免返回未落库的值。
		if scanErr := tx.QueryRowContext(ctx,
			`SELECT sent_count FROM send_daily_counters WHERE cookie_id = ? AND day = ?`,
			cookieID, day).Scan(&total); scanErr != nil {
			_ = tx.Rollback()
			return 0, scanErr
		}
		// commitErr 是更新分支的提交错误；提交失败时本次累加不生效。
		if commitErr := tx.Commit(); commitErr != nil {
			return 0, commitErr
		}
		return total, nil
	}
	// 无记录可更新：插入新桶；主键冲突说明并发方刚插入，回滚后由外层重试。
	if _, insErr := tx.ExecContext(ctx,
		`INSERT INTO send_daily_counters (cookie_id, day, sent_count) VALUES (?, ?, ?)`,
		cookieID, day, delta); insErr != nil {
		_ = tx.Rollback()
		return 0, errSendCounterInsertConflict
	}
	// commitErr 是插入分支的提交错误；提交失败时新桶不生效。
	if commitErr := tx.Commit(); commitErr != nil {
		return 0, commitErr
	}
	return int64(delta), nil
}
