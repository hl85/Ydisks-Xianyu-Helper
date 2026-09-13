// send_gate_persist_test.go 验证发送闸门的计数持久化接线。
// 覆盖：构造时从 DB 恢复较大值、acquire 后写穿、写穿失败 fail-open、静默时段不写穿。

package engine

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeSendCounterPersist 是测试用的内存计数仓储；可注入写入失败以验证 fail-open。
type fakeSendCounterPersist struct {
	// mu 保护 counters 与 failWrite 两个可变字段。
	mu sync.Mutex
	// counters 以 (cookieID, day) 为键记录已登记条数。
	counters map[string]int
	// failWrite 非空时模拟持久化写入失败。
	failWrite error
	// writes 记录写穿调用次数，用于断言「静默时段拒绝不写穿」。
	writes int
}

// key 把账号与自然日拼成内部计数键。
func (f *fakeSendCounterPersist) key(cookieID, day string) string {
	return cookieID + "|" + day
}

// GetSendCount 返回指定账号指定日的已登记条数；无记录返回 0。
func (f *fakeSendCounterPersist) GetSendCount(ctx context.Context, cookieID, day string) (int, error) {
	// _ 显式忽略未使用的取消边界，保持接口签名一致。
	_ = ctx
	// mu 保护并发读取下的 map 一致性。
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counters[f.key(cookieID, day)], nil
}

// AddSendCount 累加指定账号指定日的计数并返回累加后的总条数；failWrite 非空时返回注入的错误。
func (f *fakeSendCounterPersist) AddSendCount(ctx context.Context, cookieID, day string, delta int) (int64, error) {
	// _ 显式忽略未使用的取消边界，保持接口签名一致。
	_ = ctx
	// mu 保护 map 的读改写。
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.failWrite != nil {
		return 0, f.failWrite
	}
	// k 是当前桶的内部键。
	k := f.key(cookieID, day)
	f.counters[k] += delta
	return int64(f.counters[k]), nil
}

// newGateForPersistTest 构造带假时钟与日志的闸门，供持久化用例复用。
func newGateForPersistTest(cfg sendGateConfig, clock *gateTestClock) *sendGate {
	// gate 是注入假时钟后的闸门。
	gate := newSendGate(cfg)
	gate.now = clock.now
	gate.sleep = clock.sleep
	return gate
}

// TestSendGate_RestoreFromPersistence 验证构造接线时从 DB 恢复较大值，防止重启重置额度。
func TestSendGate_RestoreFromPersistence(t *testing.T) {
	// clock 是本用例的假时钟，固定在 2026-09-13 12:00。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// persist 预置 DB 中今日已登记 3 条。
	persist := &fakeSendCounterPersist{counters: map[string]int{}}
	persist.counters["acc-1|2026-09-13"] = 3
	// gate 是接入持久化的账号闸门，日额度 5。
	gate := newGateForPersistTest(sendGateConfig{DailyLimit: 5}, clock)
	// ctx 是恢复读取的取消边界。
	ctx := context.Background()
	gate.attachPersistence(ctx, persist, "acc-1", slog.Default())
	gate.mu.Lock()
	// usedBefore 是恢复后的内存计数，应等于 DB 的 3。
	usedBefore := gate.sentToday
	gate.mu.Unlock()
	if usedBefore != 3 {
		t.Fatalf("恢复后内存计数应为 3，实际 %d", usedBefore)
	}
	// err 是第 1 次发送的闸门结果，不应被拒绝。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("恢复后第 1 次发送不应被拒绝: %v", err)
	}
	// err 是第 2 次发送的闸门结果，不应被拒绝。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("恢复后第 2 次发送不应被拒绝: %v", err)
	}
	// err 是第 3 次发送的闸门结果，应触发日额度拒绝。
	if err := gate.acquire(ctx); !errors.Is(err, ErrSendGateDailyLimit) {
		t.Fatalf("恢复 3 条 + 本次 2 条后应触发日额度拒绝，实际 %v", err)
	}
	// dbCount 是 DB 侧最终条数，应与内存一致为 5。
	dbCount, _ := persist.GetSendCount(ctx, "acc-1", "2026-09-13")
	if dbCount != 5 {
		t.Fatalf("写穿后 DB 计数应为 5，实际 %d", dbCount)
	}
}

// TestSendGate_RestoreKeepsLargerMemoryCount 验证恢复只抬升、不缩小内存计数。
func TestSendGate_RestoreKeepsLargerMemoryCount(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// gate 先在内存中登记 2 条；最小间隔为正以保证 acquire 真正走登记路径。
	gate := newGateForPersistTest(sendGateConfig{MinInterval: time.Second}, clock)
	// ctx 是恢复读取的取消边界。
	ctx := context.Background()
	// err 是第 1 次内存登记的闸门结果，不应失败。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("内存登记不应失败: %v", err)
	}
	// err 是第 2 次内存登记的闸门结果，不应失败。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("内存登记不应失败: %v", err)
	}
	// persist 的 DB 值落后于内存（模拟 DB 尚未写穿的窗口）。
	persist := &fakeSendCounterPersist{counters: map[string]int{}}
	persist.counters["acc-2|2026-09-13"] = 1
	gate.attachPersistence(ctx, persist, "acc-2", slog.Default())
	gate.mu.Lock()
	// usedAfter 是恢复后的内存计数，应保持较大值 2 而不是被 DB 的 1 覆盖。
	usedAfter := gate.sentToday
	gate.mu.Unlock()
	if usedAfter != 2 {
		t.Fatalf("恢复不应缩小内存计数，应为 2，实际 %d", usedAfter)
	}
}

// TestSendGate_WriteThroughFailOpen 验证写穿失败只降级不阻断发送。
func TestSendGate_WriteThroughFailOpen(t *testing.T) {
	// clock 是本用例的假时钟。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 12, 0, 0, 0, time.Local)}
	// persist 注入写入失败。
	persist := &fakeSendCounterPersist{counters: map[string]int{}, failWrite: errors.New("db down")}
	// gate 是接入持久化的闸门；最小间隔为正以保证 acquire 真正走登记路径。
	gate := newGateForPersistTest(sendGateConfig{MinInterval: time.Second}, clock)
	// ctx 是发送与恢复的取消边界。
	ctx := context.Background()
	gate.attachPersistence(ctx, persist, "acc-3", slog.Default())
	// err 验证写穿失败时发送仍成功（fail-open）。
	if err := gate.acquire(ctx); err != nil {
		t.Fatalf("写穿失败不应阻断发送: %v", err)
	}
	gate.mu.Lock()
	// memUsed 是内存计数，写穿失败时应照常推进为 1。
	memUsed := gate.sentToday
	gate.mu.Unlock()
	if memUsed != 1 {
		t.Fatalf("写穿失败时内存计数应仍为 1，实际 %d", memUsed)
	}
}

// TestSendGate_NoWriteThroughWhenQuietRejected 验证静默时段拒绝路径不产生写穿。
func TestSendGate_NoWriteThroughWhenQuietRejected(t *testing.T) {
	// clock 是本用例的假时钟，落在静默时段 23-7 内。
	clock := &gateTestClock{current: time.Date(2026, 9, 13, 2, 0, 0, 0, time.Local)}
	// persist 用于统计写穿次数。
	persist := &fakeSendCounterPersist{counters: map[string]int{}}
	// gate 启用 23-7 静默时段。
	gate := newGateForPersistTest(sendGateConfig{QuietStartHour: 23, QuietEndHour: 7}, clock)
	// ctx 是恢复读取的取消边界。
	ctx := context.Background()
	gate.attachPersistence(ctx, persist, "acc-4", slog.Default())
	// err 应为静默时段拒绝。
	if err := gate.acquire(ctx); !errors.Is(err, ErrSendGateQuietHours) {
		t.Fatalf("静默时段应拒绝发送，实际 %v", err)
	}
	persist.mu.Lock()
	// writes 是本次用例累计的写穿调用次数，拒绝路径下应为 0。
	writes := persist.writes
	persist.mu.Unlock()
	if writes != 0 {
		t.Fatalf("静默时段拒绝不应写穿，实际写穿 %d 次", writes)
	}
}
