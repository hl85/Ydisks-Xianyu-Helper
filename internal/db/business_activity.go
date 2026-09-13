package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// businessActivityTimeLayouts 是业务时间列可能出现的文本格式列表。
// SQLite/MySQL/Postgres 的 CURRENT_TIMESTAMP 与历史值存在有无小数、有无时区后缀的差异；
// 无时区文本按仓库既有 UTC 约定解释，避免看门狗把本地时区误判为静默。
var businessActivityTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

// LatestBusinessActivityAt 返回三张可靠业务表（ws_messages、orders、automation_runs）
// 中最近一次业务活动的时间，用于「进程健康但业务停摆」的静默看门狗判定。
//
// 三张表的时间列均由数据库侧 CURRENT_TIMESTAMP 写入，同一行内格式统一，
// MAX 的字典序比较与时间序一致；每张表先取 MAX 再 UNION，避免整表行外传。
// 查询失败或任何一张表不可用时返回错误，由调用方记告警但不中断业务循环。
// 数据库尚无任何业务记录时返回零值 time.Time，调用方不得据此判定静默。
func (q *AnalyticsQueries) LatestBusinessActivityAt(ctx context.Context) (time.Time, error) {
	if q == nil || q.DB == nil {
		return time.Time{}, errors.New("业务活动查询存储未初始化")
	}
	// castType 是把 MAX 结果统一转成文本的方言类型名：Postgres/SQLite 用 TEXT，MySQL 用 CHAR。
	castType := "TEXT"
	if q.Dialect == DialectMySQL {
		castType = "CHAR"
	}
	// query 对三张业务表分别取时间列 MAX 后 UNION，再取全局最大值，只返回一行一列。
	query := `SELECT MAX(t) FROM (
		SELECT CAST(MAX(created_at) AS ` + castType + `) AS t FROM ws_messages
		UNION ALL
		SELECT CAST(MAX(updated_at) AS ` + castType + `) AS t FROM orders
		UNION ALL
		SELECT CAST(MAX(updated_at) AS ` + castType + `) AS t FROM automation_runs
	)`
	// latest 保存全局最新业务时间文本；三表全空时为 NULL。
	var latest sql.NullString
	// err 表示执行跨表 MAX 查询或行扫描时的数据库错误。
	if err := q.DB.QueryRowContext(ctx, query).Scan(&latest); err != nil {
		return time.Time{}, err
	}
	if !latest.Valid || strings.TrimSpace(latest.String) == "" {
		return time.Time{}, nil
	}
	return parseBusinessActivityTime(latest.String), nil
}

// parseBusinessActivityTime 按仓库既有 UTC 约定解析业务时间列文本；
// 无法识别的格式返回零值 time.Time，由调用方按「无有效记录」处理。
func parseBusinessActivityTime(raw string) time.Time {
	// trimmed 去掉驱动可能携带的空白，保证格式匹配稳定。
	trimmed := strings.TrimSpace(raw)
	// layout 表示当前遍历过程中的候选时间格式，命中任一格式即解析成功。
	for _, layout := range businessActivityTimeLayouts {
		// parsed 保存按当前格式解析出的时间；解析成功即视为有效业务时间。
		if parsed, err := time.ParseInLocation(layout, trimmed, time.UTC); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
