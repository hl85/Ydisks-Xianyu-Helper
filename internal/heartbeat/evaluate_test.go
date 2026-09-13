package heartbeat

import (
	"testing"
	"time"
)

// TestEvaluateHeartbeat_Cases 覆盖正常、过期与未配置三类判定。
func TestEvaluateHeartbeat_Cases(t *testing.T) {
	// base 是判定比较的基准「现在」。
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	// cases 是表驱动用例：最近心跳、现在、超时窗口与期望的过期/配置状态。
	cases := []struct {
		// name 是用例名称。
		name string
		// lastBeat 是最近一次心跳时刻；零值表示无记录。
		lastBeat time.Time
		// now 是判定时刻。
		now time.Time
		// timeout 是容忍窗口。
		timeout time.Duration
		// wantStale 是期望的过期判定。
		wantStale bool
		// wantConfigured 是期望的配置状态。
		wantConfigured bool
	}{
		{"正常：窗口内", base.Add(-10 * time.Second), base, 30 * time.Second, false, true},
		{"正常：恰好等于窗口边界不判过期", base.Add(-30 * time.Second), base, 30 * time.Second, false, true},
		{"过期：超出窗口", base.Add(-31 * time.Second), base, 30 * time.Second, true, true},
		{"未配置：无心跳记录", time.Time{}, base, 30 * time.Second, true, false},
		{"未配置阈值：窗口为 0", base.Add(-1 * time.Second), base, 0, true, true},
		{"未配置阈值且无记录", time.Time{}, base, 0, true, false},
	}
	for // tc 表示当前遍历过程中的用例。
	_, tc := range cases {
		// stale、configured 是本次判定的实际结果。
		stale, configured := EvaluateHeartbeat(tc.lastBeat, tc.now, tc.timeout)
		if stale != tc.wantStale || configured != tc.wantConfigured {
			t.Fatalf("%s: EvaluateHeartbeat = (stale=%v, configured=%v), want (stale=%v, configured=%v)",
				tc.name, stale, configured, tc.wantStale, tc.wantConfigured)
		}
	}
}
