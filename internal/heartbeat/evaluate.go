package heartbeat

import "time"

// EvaluateHeartbeat 判定最近一次心跳是否仍在容忍窗口内，供宿主侧检查入口与测试共用。
//
// 返回值语义：
//   - configured 表示数据库已有心跳记录（false 即「未配置 / 尚无进程写过」）；
//   - stale 表示已过期（需告警 / 判定不健康）。
//
// 判定规则：lastBeat 为零值（无记录）视为未配置且过期；timeout 小于等于 0 视为未配置阈值，
// 一律判过期以促使运维显式配置超时；其余情况按 now 与 lastBeat 的间隔是否超过 timeout 判定。
func EvaluateHeartbeat(lastBeat, now time.Time, timeout time.Duration) (stale, configured bool) {
	// configured 标记数据库是否已有心跳记录；零值时间表示无记录。
	configured = !lastBeat.IsZero()
	// 未配置阈值：无法判定健康，一律视为过期，迫使运维显式配置超时窗口。
	if timeout <= 0 {
		return true, configured
	}
	// 无记录即「未配置 / 进程从未打点」，按过期处理。
	if !configured {
		return true, false
	}
	// stale 是本进程最近一次心跳距今是否已超过容忍窗口。
	stale = now.Sub(lastBeat) > timeout
	return stale, configured
}
