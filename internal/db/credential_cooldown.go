// credential_cooldown.go 登录恢复冷却状态的持久化仓储。
// 表结构见 00048_credential_cooldowns：以 (cookie_id, kind) 为主键，记录最近一次冷却标记时间，
// 供进程重启后继续尊重此前的登录失败冷却，避免重启风暴反复向平台申请登录凭证。

package db

import (
	"context"
	"time"
)

// CredentialCooldown 是一条冷却记录的最小快照。
type CredentialCooldown struct {
	// CookieID 是账号标识。
	CookieID string
	// Kind 是冷却类别（session_expired / password_login / password_error）。
	Kind string
	// MarkedAt 是最近一次标记时间。
	MarkedAt time.Time
}

// ListCooldowns 返回全部冷却记录，供进程启动时恢复。
// ctx 是调用方取消边界；记录数量以账号数计，量级很小，允许全量读取。
func (s *RenewalStore) ListCooldowns(ctx context.Context) ([]CredentialCooldown, error) {
	// rows 是冷却记录的查询游标。
	rows, err := s.DB.QueryContext(ctx, `SELECT cookie_id, kind, marked_at FROM credential_cooldowns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// records 是返回结果；容量按常见账号量级预估，避免频繁扩容。
	records := make([]CredentialCooldown, 0, 8)
	for rows.Next() {
		// record 是当前遍历到的记录。
		var record CredentialCooldown
		// markedAtUnix 是数据库中的 Unix 秒时间。
		var markedAtUnix int64
		// err 是单行扫描错误。
		if err := rows.Scan(&record.CookieID, &record.Kind, &markedAtUnix); err != nil {
			return nil, err
		}
		record.MarkedAt = time.Unix(markedAtUnix, 0)
		records = append(records, record)
	}
	// err 是游标遍历收尾错误；遗漏它会把部分读取当成完整结果。
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// MarkCooldown 以 (cookie_id, kind) 为主键记录最近一次冷却标记时间。
// 写入采用「先删后插」的事务写法，三条方言行为一致；失败时调用方降级为纯内存冷却。
func (s *RenewalStore) MarkCooldown(ctx context.Context, cookieID, kind string, markedAt time.Time) error {
	// tx 是单条冷却记录的写入事务；任一步失败即整体回滚。
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// delErr 是删除旧记录的错误。
	if _, delErr := tx.ExecContext(ctx, `DELETE FROM credential_cooldowns WHERE cookie_id = ? AND kind = ?`, cookieID, kind); delErr != nil {
		_ = tx.Rollback()
		return delErr
	}
	// insErr 是插入新记录的错误。
	if _, insErr := tx.ExecContext(ctx,
		`INSERT INTO credential_cooldowns (cookie_id, kind, marked_at) VALUES (?, ?, ?)`,
		cookieID, kind, markedAt.Unix()); insErr != nil {
		_ = tx.Rollback()
		return insErr
	}
	return tx.Commit()
}

// ClearCooldowns 删除指定账号的全部冷却记录，对应账号登录成功后的冷却重置。
func (s *RenewalStore) ClearCooldowns(ctx context.Context, cookieID string) error {
	// result 是删除结果；错误必须上抛，避免「看似重置、实际仍冷却」的假成功。
	_, err := s.DB.ExecContext(ctx, `DELETE FROM credential_cooldowns WHERE cookie_id = ?`, cookieID)
	return err
}
