package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"xianyu-go/internal/automation"
)

// gateTestClock 是发送闸门测试使用的可控时钟；它同时记录被要求的等待时长。
type gateTestClock struct {
	// current 是当前被假定的时间。
	current time.Time
	// waits 按调用顺序记录每次被要求的等待时长。
	waits []time.Duration
}

// now 返回当前假定时间。
func (c *gateTestClock) now() time.Time {
	return c.current
}

// sleep 记录等待时长并推进假定时间，不产生真实等待；ctx 被忽略。
func (c *gateTestClock) sleep(ctx context.Context, d time.Duration) error {
	_ = ctx
	c.waits = append(c.waits, d)
	c.current = c.current.Add(d)
	return nil
}

// advance 把假定时间向前推进指定时长，用于模拟自然时间流逝。
func (c *gateTestClock) advance(d time.Duration) {
	c.current = c.current.Add(d)
}

// newGateWithClock 构造一个使用假时钟的闸门；抖动固定为 0 使等待时长可被精确断言。
func newGateWithClock(cfg sendGateConfig, clock *gateTestClock) *sendGate {
	// gate 是注入假时钟后的闸门；其余注入点保持生产实现。
	gate := newSendGate(cfg)
	gate.now = clock.now
	gate.sleep = clock.sleep
	return gate
}

// TestSendGate_ZeroConfigDisabled 验证全零配置下闸门不产生任何等待。
func TestSendGate_ZeroConfigDisabled(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 是关闭状态的闸门。
	gate := newGateWithClock(sendGateConfig{}, clock)
	// err 是本次发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("关闭的闸门不应拒绝发送: %v", err)
	}
	if len(clock.waits) != 0 {
		t.Fatalf("关闭的闸门不应产生等待，实际 %v", clock.waits)
	}
}

// TestSendGate_FirstSendDoesNotWait 验证首条发送无需等待，间隔只约束后续发送。
func TestSendGate_FirstSendDoesNotWait(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 是仅约束发送间隔的闸门。
	gate := newGateWithClock(sendGateConfig{MinInterval: time.Second}, clock)
	// err 是首条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("首条发送不应被拒绝: %v", err)
	}
	if clock.waits[0] != 0 {
		t.Fatalf("首条发送等待应为 0，实际 %v", clock.waits[0])
	}
}

// TestSendGate_EnforcesMinimumInterval 验证连续两次发送之间会被补齐最小间隔。
func TestSendGate_EnforcesMinimumInterval(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 是仅约束发送间隔的闸门。
	gate := newGateWithClock(sendGateConfig{MinInterval: time.Second}, clock)
	// err 是首条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("首条发送不应被拒绝: %v", err)
	}
	clock.advance(200 * time.Millisecond)
	// err2 是第二条发送的准入结论。
	if err2 := gate.acquire(context.Background()); err2 != nil {
		t.Fatalf("第二条发送不应被拒绝: %v", err2)
	}
	// secondWait 是第二条发送被要求的等待时长，应为补足 1 秒所需的 800 毫秒。
	secondWait := clock.waits[1]
	if secondWait != 800*time.Millisecond {
		t.Fatalf("第二条发送等待应为 800ms，实际 %v", secondWait)
	}
}

// TestSendGate_JitterStaysWithinBound 验证开启抖动后等待时长落在基础间隔与上限之间。
func TestSendGate_JitterStaysWithinBound(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// cfg 同时启用基础间隔与抖动上限。
	cfg := sendGateConfig{MinInterval: time.Second, Jitter: time.Second}
	// gate 是本用例的闸门。
	gate := newGateWithClock(cfg, clock)
	// err 是首条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("首条发送不应被拒绝: %v", err)
	}
	// err2 是第二条发送的准入结论。
	if err2 := gate.acquire(context.Background()); err2 != nil {
		t.Fatalf("第二条发送不应被拒绝: %v", err2)
	}
	// wait 是第二条发送被要求的等待时长，应落在 [基础间隔, 基础间隔+抖动] 区间内。
	wait := clock.waits[1]
	if wait < cfg.MinInterval || wait > cfg.MinInterval+cfg.Jitter {
		t.Fatalf("带抖动的等待应落在 [%v, %v]，实际 %v", cfg.MinInterval, cfg.MinInterval+cfg.Jitter, wait)
	}
}

// TestSendGate_DailyLimitRejects 验证达到当日额度后发送被拒绝。
func TestSendGate_DailyLimitRejects(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 是当日额度为 2 的闸门。
	gate := newGateWithClock(sendGateConfig{DailyLimit: 2}, clock)
	// err 是第 1 条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("第 1 条发送不应被拒绝: %v", err)
	}
	// err2 是第 2 条发送的准入结论。
	if err2 := gate.acquire(context.Background()); err2 != nil {
		t.Fatalf("第 2 条发送不应被拒绝: %v", err2)
	}
	// thirdErr 是第 3 条发送的准入结论，应命中当日额度。
	thirdErr := gate.acquire(context.Background())
	if !errors.Is(thirdErr, ErrSendGateDailyLimit) {
		t.Fatalf("第 3 条发送应命中当日额度限制，实际 %v", thirdErr)
	}
}

// TestSendGate_DailyLimitResetsOnNextDay 验证跨自然日后当日用量被重置。
func TestSendGate_DailyLimitResetsOnNextDay(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 是当日额度为 1 的闸门。
	gate := newGateWithClock(sendGateConfig{DailyLimit: 1}, clock)
	// err 是第 1 条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("第 1 条发送不应被拒绝: %v", err)
	}
	if !errors.Is(gate.acquire(context.Background()), ErrSendGateDailyLimit) {
		t.Fatal("同日第 2 条发送应被拒绝")
	}
	clock.advance(24 * time.Hour)
	// err2 是跨日后的发送准入结论。
	if err2 := gate.acquire(context.Background()); err2 != nil {
		t.Fatalf("跨日后发送应恢复，实际 %v", err2)
	}
}

// TestSendGate_QuietHoursRejects 验证静默时段内发送被拒绝。
func TestSendGate_QuietHoursRejects(t *testing.T) {
	// clock 是落在静默时段内的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 23, 30, 0, 0, time.Local)}
	// gate 配置了跨越午夜的静默时段 23-7。
	gate := newGateWithClock(sendGateConfig{QuietStartHour: 23, QuietEndHour: 7}, clock)
	// err 是静默时段内的发送准入结论，应命中静默时段拒绝。
	if err := gate.acquire(context.Background()); !errors.Is(err, ErrSendGateQuietHours) {
		t.Fatalf("静默时段内应被拒绝，实际 %v", err)
	}
}

// TestSendGate_QuietHoursAllowsOutsideWindow 验证静默时段之外发送正常放行。
func TestSendGate_QuietHoursAllowsOutsideWindow(t *testing.T) {
	// clock 落在静默时段之外的正午。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 配置了跨越午夜的静默时段 23-7。
	gate := newGateWithClock(sendGateConfig{QuietStartHour: 23, QuietEndHour: 7}, clock)
	// err 是静默时段之外的发送准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("静默时段之外不应被拒绝: %v", err)
	}
}

// TestSendGate_WaitIsCapped 验证过于保守的间隔配置会被等待上限收敛。
func TestSendGate_WaitIsCapped(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 的最小间隔远大于等待上限，用于验证收敛行为。
	gate := newGateWithClock(sendGateConfig{MinInterval: time.Hour}, clock)
	// err 是首条发送的准入结论。
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatalf("首条发送不应被拒绝: %v", err)
	}
	// err2 是第二条发送的准入结论。
	if err2 := gate.acquire(context.Background()); err2 != nil {
		t.Fatalf("第二条发送不应被拒绝: %v", err2)
	}
	// got 是第二条发送被要求的等待时长。
	got := clock.waits[1]
	if got != sendGateMaxWait {
		t.Fatalf("等待应被收敛到 %v，实际 %v", sendGateMaxWait, got)
	}
}

// TestSleepWithContext_RespectsCancellation 验证等待能被调用方 ctx 提前取消。
func TestSleepWithContext_RespectsCancellation(t *testing.T) {
	// ctx 是已被取消的调用边界。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err 是等待的返回结果，应为取消错误。
	if err := sleepWithContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应立即返回取消错误，实际 %v", err)
	}
}

// TestSendGateConfigFromEnv 验证环境变量能被正确解析，非法值回落默认。
func TestSendGateConfigFromEnv(t *testing.T) {
	t.Setenv(sendGateMinIntervalEnv, "2500")
	t.Setenv(sendGateJitterEnv, "300")
	t.Setenv(sendGateDailyLimitEnv, "500")
	t.Setenv(sendGateQuietHoursEnv, "23-7")
	// cfg 是解析后的配置。
	cfg := sendGateConfigFromEnv()
	if cfg.MinInterval != 2500*time.Millisecond {
		t.Fatalf("最小间隔解析错误: %v", cfg.MinInterval)
	}
	if cfg.Jitter != 300*time.Millisecond {
		t.Fatalf("抖动解析错误: %v", cfg.Jitter)
	}
	if cfg.DailyLimit != 500 {
		t.Fatalf("日额度解析错误: %d", cfg.DailyLimit)
	}
	if cfg.QuietStartHour != 23 || cfg.QuietEndHour != 7 {
		t.Fatalf("静默时段解析错误: %d-%d", cfg.QuietStartHour, cfg.QuietEndHour)
	}
}

// TestSendGateConfigFromEnvRejectsInvalid 验证非法配置回落到安全默认，而不是阻断发送。
func TestSendGateConfigFromEnvRejectsInvalid(t *testing.T) {
	t.Setenv(sendGateMinIntervalEnv, "abc")
	t.Setenv(sendGateJitterEnv, "-5")
	t.Setenv(sendGateDailyLimitEnv, "-1")
	t.Setenv(sendGateQuietHoursEnv, "25-99")
	// cfg 是解析后的配置。
	cfg := sendGateConfigFromEnv()
	if cfg.MinInterval != sendGateDefaultMinInterval {
		t.Fatalf("非法间隔应回落默认 %v，实际 %v", sendGateDefaultMinInterval, cfg.MinInterval)
	}
	if cfg.Jitter != sendGateDefaultJitter {
		t.Fatalf("负数抖动应回落默认 %v，实际 %v", sendGateDefaultJitter, cfg.Jitter)
	}
	if cfg.DailyLimit != 0 {
		t.Fatalf("负数日额度应回落为不限制，实际 %d", cfg.DailyLimit)
	}
	if cfg.quietEnabled() {
		t.Fatal("越界的静默时段应视为不启用")
	}
}

// TestEnvQuietHoursFormat 验证静默时段的各种输入形态。
func TestEnvQuietHoursFormat(t *testing.T) {
	// cases 覆盖合法区间、缺省、单值缺失与越界四类输入。
	cases := []struct {
		// raw 是环境变量原文。
		raw string
		// wantStart、wantEnd 是期望解析出的起止小时。
		wantStart int
		wantEnd   int
	}{
		{raw: "22-6", wantStart: 22, wantEnd: 6},
		{raw: "", wantStart: 0, wantEnd: 0},
		{raw: "22", wantStart: 0, wantEnd: 0},
		{raw: "24-1", wantStart: 0, wantEnd: 0},
	}
	// testCase 是当前遍历到的输入样例。
	for _, testCase := range cases {
		t.Setenv(sendGateQuietHoursEnv, testCase.raw)
		// start、end 是本轮解析出的起止小时。
		start, end := envQuietHours(sendGateQuietHoursEnv)
		if start != testCase.wantStart || end != testCase.wantEnd {
			t.Fatalf("输入 %q 解析为 %d-%d，期望 %d-%d", testCase.raw, start, end, testCase.wantStart, testCase.wantEnd)
		}
	}
}

// TestAcquireSendSlot_WrapsAsMessageNotSent 验证闸门拒绝被归类为「确定未发送」，避免自动化误判为结果不确定。
func TestAcquireSendSlot_WrapsAsMessageNotSent(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// coordinator 只装配闸门，用于验证错误归类。
	coordinator := &outgoingMessageCoordinator{gate: newGateWithClock(sendGateConfig{DailyLimit: 1}, clock)}
	// err 是额度内的发送准入结论。
	if err := coordinator.acquireSendSlot(context.Background()); err != nil {
		t.Fatalf("额度内的发送不应被拒绝: %v", err)
	}
	// err2 是额度用尽后的准入结论。
	err2 := coordinator.acquireSendSlot(context.Background())
	if !errors.Is(err2, automation.ErrMessageNotSent) {
		t.Fatalf("闸门拒绝应包装为确定未发送，实际 %v", err2)
	}
	if !errors.Is(err2, ErrSendGateDailyLimit) {
		t.Fatalf("包装后仍应能识别原始原因，实际 %v", err2)
	}
}

// TestAcquireSendSlot_NilGateAllows 验证未装配闸门的协调器不受影响。
func TestAcquireSendSlot_NilGateAllows(t *testing.T) {
	// coordinator 是没有闸门的协调器，对应测试中直接构造的形态。
	coordinator := &outgoingMessageCoordinator{}
	// err 是发送准入结论。
	if err := coordinator.acquireSendSlot(context.Background()); err != nil {
		t.Fatalf("未装配闸门时不应拒绝发送: %v", err)
	}
}
