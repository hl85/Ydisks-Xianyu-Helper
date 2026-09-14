// send_budget_test.go 验证进程级全局日发送预算的边界行为。
// 覆盖：额度边界、跨日滚动（假时钟）、DB 恢复取较大值、env 解析，以及全局拒绝时账号槽回滚。

package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

// budgetTestPersist 是全局预算测试专用的内存计数仓储；线程安全且支持按桶预置。
type budgetTestPersist struct {
	// mu 保护 counters 的并发读写。
	mu sync.Mutex
	// counters 以 (cookieID, day) 为键记录条数。
	counters map[string]int
}

// GetSendCount 返回指定桶的当前条数；无记录返回 0。
func (f *budgetTestPersist) GetSendCount(ctx context.Context, cookieID, day string) (int, error) {
	// _ 显式忽略未使用的取消边界，保持接口签名一致。
	_ = ctx
	// mu 保护并发读取。
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters[cookieID+"|"+day], nil
}

// AddSendCount 累加指定桶并返回累加后的总条数。
func (f *budgetTestPersist) AddSendCount(ctx context.Context, cookieID, day string, delta int) (int64, error) {
	// _ 显式忽略未使用的取消边界，保持接口签名一致。
	_ = ctx
	// mu 保护读改写。
	f.mu.Lock()
	defer f.mu.Unlock()
	// key 是当前桶的内部键。
	key := cookieID + "|" + day
	f.counters[key] += delta
	return int64(f.counters[key]), nil
}

// TestSendBudget_LimitBoundary 验证全局额度用尽即拒绝、次日自动重置。
func TestSendBudget_LimitBoundary(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// budget 是上限为 2 的全局预算，注入假时钟。
	budget := NewSendBudget(2, nil, slog.Default())
	budget.now = clock.now
	// ctx 是占用的取消边界。
	ctx := context.Background()
	// day、err 是第 1 次占用的结果，不应被拒绝。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("第 1 次占用不应被拒绝: %v", err)
	}
	// err 是第 2 次占用的结果，不应被拒绝。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("第 2 次占用不应被拒绝: %v", err)
	}
	// err 是第 3 次占用的结果，应触发全局额度拒绝。
	if _, err := budget.acquire(ctx); !errors.Is(err, ErrSendGateGlobalDailyLimit) {
		t.Fatalf("额度 2 用尽后应拒绝，实际 %v", err)
	}
	// 前进一天后额度应重置。
	clock.advance(24 * time.Hour)
	// err 是跨日后的占用结果，不应被拒绝。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("跨日后占用应重置并成功: %v", err)
	}
}

// TestSendBudget_ZeroLimitUnlimited 验证额度为 0 时不限制也不写穿。
func TestSendBudget_ZeroLimitUnlimited(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// persist 用于断言未发生写穿。
	persist := &budgetTestPersist{counters: map[string]int{}}
	// budget 是不限制的全局预算。
	budget := NewSendBudget(0, persist, slog.Default())
	budget.now = clock.now
	// ctx 是占用的取消边界。
	ctx := context.Background()
	// i 是当前占用序号。
	for i := 0; i < 3; i++ {
		// err 是循环内单次占用的结果，不应被拒绝。
		if _, err := budget.acquire(ctx); err != nil {
			t.Fatalf("不限额时第 %d 次占用不应被拒绝: %v", i+1, err)
		}
	}
	persist.mu.Lock()
	// writes 是写穿过的桶数量，不限额时应为 0。
	writes := len(persist.counters)
	persist.mu.Unlock()
	if writes != 0 {
		t.Fatalf("不限额时不应写穿任何桶，实际 %d", writes)
	}
}

// TestSendBudget_RestoreKeepsLargerUsage 验证恢复取内存与 DB 的较大值、只抬升不缩小。
func TestSendBudget_RestoreKeepsLargerUsage(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// persist 预置全局桶今日已用 4 条。
	persist := &budgetTestPersist{counters: map[string]int{}}
	persist.counters["|2026-09-13"] = 4
	// budget 是上限 5 的全局预算。
	budget := NewSendBudget(5, persist, slog.Default())
	budget.now = clock.now
	// ctx 是占用与恢复的取消边界。
	ctx := context.Background()
	// 恢复后内存用量应等于 DB 的 4。
	budget.Restore(ctx)
	budget.mu.Lock()
	// usedAfterRestore 是恢复后的内存用量，应等于 DB 的 4。
	usedAfterRestore := budget.used
	budget.mu.Unlock()
	if usedAfterRestore != 4 {
		t.Fatalf("恢复后用量应为 4，实际 %d", usedAfterRestore)
	}
	// 把 DB 降为 1（模拟过期读数）后再次恢复，内存不应被缩小。
	persist.mu.Lock()
	persist.counters["|2026-09-13"] = 1
	persist.mu.Unlock()
	budget.Restore(ctx)
	budget.mu.Lock()
	// usedAfterStale 是过期读数恢复后的内存用量，应保持 4。
	usedAfterStale := budget.used
	budget.mu.Unlock()
	if usedAfterStale != 4 {
		t.Fatalf("过期 DB 读数不应缩小内存用量，应为 4，实际 %d", usedAfterStale)
	}
	// 已用 4，再占 1 次即用尽。
	// err 是恢复后第 1 次占用的结果，不应被拒绝。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("恢复后第 1 次占用不应被拒绝: %v", err)
	}
	// err 是第 2 次占用的结果，应触发全局额度拒绝。
	if _, err := budget.acquire(ctx); !errors.Is(err, ErrSendGateGlobalDailyLimit) {
		t.Fatalf("恢复 4 条 + 本次 1 条后应拒绝，实际 %v", err)
	}
}

// TestSendGate_GlobalRejectRollsBackAccountSlot 验证全局拒绝时账号槽被回滚。
func TestSendGate_GlobalRejectRollsBackAccountSlot(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// budget 是上限 1 的全局预算，已被外部占用 1 条。
	budget := NewSendBudget(1, nil, slog.Default())
	budget.now = clock.now
	// ctx 是占用的取消边界。
	ctx := context.Background()
	// err 是外部预占用的结果，不应失败。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("预占用不应失败: %v", err)
	}
	// gate 是账号级不限额但接入全局预算的闸门；零配置也能走全局占用路径。
	gate := newSendGate(sendGateConfig{})
	gate.now = clock.now
	gate.sleep = clock.sleep
	gate.global = budget
	// err 是本次发送的闸门结果，应因全局额度用尽被拒绝。
	if err := gate.acquire(ctx); !errors.Is(err, ErrSendGateGlobalDailyLimit) {
		t.Fatalf("全局额度用尽应拒绝发送，实际 %v", err)
	}
	// 验证账号槽已回滚：内存计数回到 0，释放全局额度后可立即再次发送。
	budget.mu.Lock()
	// used 是全局预算的当前占用，应保持 1。
	used := budget.used
	budget.mu.Unlock()
	if used != 1 {
		t.Fatalf("全局占用应保持 1，实际 %d", used)
	}
	// 手动归还全局额度模拟次日重置，验证账号槽可再次占用。
	gate.mu.Lock()
	// memUsed 是账号闸门的内存计数，全局拒绝回滚后应为 0。
	memUsed := gate.sentToday
	gate.mu.Unlock()
	if memUsed != 0 {
		t.Fatalf("全局拒绝后账号槽应回滚为 0，实际 %d", memUsed)
	}
	// 模拟归还全局额度（如次日重置），验证账号槽可再次占用。
	budget.mu.Lock()
	budget.used = 0
	budget.mu.Unlock()
	// err 是归还后的预算占用结果，不应被拒绝。
	if _, err := budget.acquire(ctx); err != nil {
		t.Fatalf("归还全局额度后不应再拒绝: %v", err)
	}
	// 再次归还，确保账号闸门的第二次发送能通过全局占用。
	budget.mu.Lock()
	budget.used = 0
	budget.mu.Unlock()
	// err 是账号槽回滚后再次发送的闸门结果，不应被拒绝。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("账号槽回滚后应可再次发送: %v", err)
	}
}

// TestSendBudgetFromEnv 验证环境变量解析：合法值生效，非法值回落为不限。
func TestSendBudgetFromEnv(t *testing.T) {
	// persist 是占位的空仓储。
	persist := &budgetTestPersist{counters: map[string]int{}}
	// logger 使用测试默认日志。
	logger := slog.Default()
	t.Setenv(sendGateGlobalDailyLimitEnv, "42")
	// budget 是从环境解析出的全局预算。
	budget := NewSendBudgetFromEnv(persist, logger)
	if budget.limit != 42 {
		t.Fatalf("合法环境值应生效，实际 %d", budget.limit)
	}
	t.Setenv(sendGateGlobalDailyLimitEnv, "-3")
	// invalidBudget 是非法环境值下的预算，应回落为不限。
	invalidBudget := NewSendBudgetFromEnv(persist, logger)
	if invalidBudget.limit != 0 {
		t.Fatalf("非法环境值应回落 0，实际 %d", invalidBudget.limit)
	}
}

// 内存设置读取器：返回预置的键值或错误，用于验证数据库设置优先逻辑。
type fakeSettingGetter struct {
	// values 保存预置的系统设置键值。
	values map[string]string
	// err 是非空时统一返回该读取错误。
	err error
}

// 让 fakeSettingGetter 满足 ResolveGlobalSendDailyLimit 的 getSetting 签名。
func (f *fakeSettingGetter) get(ctx context.Context, key string) (string, error) {
	// _ 显式忽略未使用的取消边界，保持接口签名一致。
	_ = ctx
	if f.err != nil {
		return "", f.err
	}
	// v 是预置设置值；缺失返回空串表示未配置。
	v, ok := f.values[key]
	if !ok {
		return "", nil
	}
	return v, nil
}

// TestResolveGlobalSendDailyLimit 验证读取优先级：设置命中 > 设置为空回落 env > 都为空回落默认 > 非法值回落默认。
func TestResolveGlobalSendDailyLimit(t *testing.T) {
	// cases 覆盖四种优先级分支，确保数据库配置真正压过环境变量。
	cases := []struct {
		// name 是当前用例名称。
		name string
		// setting 是数据库系统设置值；空串表示未配置。
		setting string
		// configured 表示是否预置数据库设置值。
		configured bool
		// env 是环境变量 XIANYU_GLOBAL_SEND_DAILY_LIMIT 的值；空串表示未设置。
		env string
		// expect 是期望解析出的额度。
		expect int
	}{
		{name: "设置命中优先于环境变量", setting: "50", configured: true, env: "999", expect: 50},
		{name: "设置为空回落到环境变量", setting: "", configured: true, env: "30", expect: 30},
		{name: "设置与环境变量都为空回落默认不限", setting: "", configured: true, env: "", expect: 0},
		{name: "设置非法值回落到环境变量", setting: "abc", configured: true, env: "25", expect: 25},
		{name: "设置负值回落到环境变量", setting: "-7", configured: true, env: "25", expect: 25},
		{name: "数据库读取失败回落到环境变量", setting: "50", configured: false, env: "40", expect: 40},
	}
	for // tc 表示当前遍历过程中的用例
	_, tc := range cases {
		// 快照避免闭包捕获循环变量，供子测试使用。
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// getSetting 是注入的假设置读取函数；未配置时返回空串模拟缺席。
			var getSetting func(context.Context, string) (string, error)
			if tc.configured {
				getSetting = func(_ context.Context, key string) (string, error) {
					_ = key
					return tc.setting, nil
				}
			} else {
				// s 是模拟读取失败的假仓储。
				s := &fakeSettingGetter{err: errors.New("db unavailable")}
				getSetting = s.get
			}
			if tc.env != "" {
				t.Setenv(sendGateGlobalDailyLimitEnv, tc.env)
			} else {
				// 清除环境变量，确保不污染旧值的回落结果。
				t.Setenv(sendGateGlobalDailyLimitEnv, "")
			}
			// got 是解析出的额度。
			got := ResolveGlobalSendDailyLimit(context.Background(), getSetting, os.Getenv)
			if got != tc.expect {
				t.Fatalf("用例 %q 额度应为 %d，实际 %d", tc.name, tc.expect, got)
			}
		})
	}
}
