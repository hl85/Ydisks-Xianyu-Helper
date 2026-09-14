package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeBeatStore 是 BeatWriter 的内存假实现，记录每次写入的时刻与失败注入。
type fakeBeatStore struct {
	// mu 保护下方字段，使并发写循环与测试断言安全。
	mu sync.Mutex
	// beats 保存每次调用 Beat 的时刻，供断言写入次数与顺序。
	beats []time.Time
	// failFrom 是首次开始返回错误的调用序号；负数表示永不失败。
	failFrom int
	// calls 是 Beat 被调用的累计次数。
	calls int
}

// Beat 记录本次写入时刻；当调用序号达到 failFrom 时返回注入错误。
func (f *fakeBeatStore) Beat(_ context.Context, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// idx 是当前调用的序号，用于判断是否进入失败注入区间。
	idx := f.calls
	f.calls++
	f.beats = append(f.beats, at)
	if f.failFrom >= 0 && idx >= f.failFrom {
		return errors.New("注入的心跳写入失败")
	}
	return nil
}

// count 返回当前已记录的写入次数。
func (f *fakeBeatStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.beats)
}

// newTestWriter 用假存储与可控时钟构造测试用 Writer；interval 由调用方显式指定，
// 绕过 NewWriter 的环境变量解析，保证用例确定性。
func newTestWriter(store BeatWriter, interval time.Duration, now func() time.Time) *Writer {
	return &Writer{
		store:    store,
		interval: interval,
		done:     make(chan struct{}),
		now:      now,
		logger:   slog.Default(),
	}
}

// TestWriter_WritesPeriodicallyAndStopsOnCancel 验证写者在周期内持续打点，
// ctx 取消后停止并通过 done 解除等待，不留 goroutine。
func TestWriter_WritesPeriodicallyAndStopsOnCancel(t *testing.T) {
	// store 是记录每次心跳的内存假存储。
	store := &fakeBeatStore{}
	// clock 是可注入的假时钟，初始固定时刻。
	clock := &fakeClock{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	// w 是周期 10 毫秒的测试写者。
	w := newTestWriter(store, 10*time.Millisecond, clock.nowFunc)
	// ctx、cancel 控制写者生命周期；取消后写循环必须停止。
	ctx, cancel := context.WithCancel(context.Background())
	// wg 等待写者 goroutine 退出，避免测试提前结束。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()
	// 让写者运行足够多个周期再取消，确保多次打点发生。
	time.Sleep(60 * time.Millisecond)
	cancel()
	// waitErr 是带预算等待写循环退出的结果，超时说明 goroutine 未正常收束。
	waitErr := w.WaitContext(context.Background())
	if waitErr != nil {
		t.Fatalf("WaitContext 不应返回错误: %v", waitErr)
	}
	wg.Wait()
	// wrote 是取消前实际写入次数，应至少 2 次（启动一次 + 至少一次周期触发）。
	wrote := store.count()
	if wrote < 2 {
		t.Fatalf("周期内写入次数 = %d, want >= 2", wrote)
	}
}

// TestWriter_FirstBeatOnStart 验证写者启动时立即打点一次，缩短健康检查发现空窗。
func TestWriter_FirstBeatOnStart(t *testing.T) {
	// store 是记录每次心跳的内存假存储。
	store := &fakeBeatStore{}
	// clock 是可注入的假时钟。
	clock := &fakeClock{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	// w 是周期较长的测试写者，避免启动后立刻触发第二次。
	w := newTestWriter(store, time.Hour, clock.nowFunc)
	// ctx、cancel 控制写者生命周期；取消后写循环必须退出。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 同步启动后应立即完成首次打点，无需等待周期。
	go w.Run(ctx)
	// 短等待确认首次写入已发生，再取消等待收束避免 goroutine 泄漏。
	time.Sleep(20 * time.Millisecond)
	// first 是启动后记录的首次心跳次数。
	first := store.count()
	if first < 1 {
		t.Fatalf("启动后应至少打点一次, got %d", first)
	}
	// 周期长达一小时，必须先取消让写循环退出，再等收束；否则 WaitContext 会永久阻塞。
	cancel()
	// waitErr 是等待写循环退出的结果；ctx 已取消应立即成功。
	waitErr := w.WaitContext(context.Background())
	if waitErr != nil {
		t.Fatalf("首次打点用例 WaitContext 不应返回错误: %v", waitErr)
	}
}

// TestWriter_WriteFailureIsFailOpen 验证写入失败时写者不 panic、不阻塞，
// 仍能继续周期写入并在取消后正常退出。
func TestWriter_WriteFailureIsFailOpen(t *testing.T) {
	// store 从第二次调用起持续失败，模拟数据库瞬时不可用。
	store := &fakeBeatStore{failFrom: 1}
	// clock 是可注入的假时钟。
	clock := &fakeClock{now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	// w 是周期 10 毫秒的测试写者。
	w := newTestWriter(store, 10*time.Millisecond, clock.nowFunc)
	// ctx、cancel 控制写者生命周期。
	ctx, cancel := context.WithCancel(context.Background())
	// wg 等待写者 goroutine 退出。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()
	// 让写者在失败模式下运行若干周期，确认没有死锁或 panic。
	time.Sleep(50 * time.Millisecond)
	cancel()
	// waitErr 是带预算等待写循环退出的结果。
	waitErr := w.WaitContext(context.Background())
	if waitErr != nil {
		t.Fatalf("失败模式下 WaitContext 不应返回错误: %v", waitErr)
	}
	wg.Wait()
	// attempted 是失败模式下仍被尝试的写入次数，应大于 1（首次成功 + 后续失败仍继续）。
	attempted := store.count()
	if attempted < 2 {
		t.Fatalf("失败模式仍应持续尝试写入, got %d", attempted)
	}
}

// TestWriter_NilStoreNoGoroutine 验证未装配存储时 Run 直接返回且不创建 goroutine、不阻塞。
func TestWriter_NilStoreNoGoroutine(t *testing.T) {
	// w 是 store 为 nil 的写者。
	w := &Writer{done: make(chan struct{})}
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// Run 应立即返回，不阻塞；done 随之关闭。
	w.Run(ctx)
	// waitErr 是等待结果，未启动时应立即成功。
	waitErr := w.WaitContext(context.Background())
	if waitErr != nil {
		t.Fatalf("未装配存储时 WaitContext 不应返回错误: %v", waitErr)
	}
}

// TestNewWriter_DisabledWhenZero 验证环境变量为 0 时 NewWriter 返回 nil，调用方据此跳过启动。
func TestNewWriter_DisabledWhenZero(t *testing.T) {
	// env 是返回 "0" 的假环境变量函数，表示显式关闭。
	env := func(string) string { return "0" }
	// interval 是绕过 NewWriter 直接验证环境解析的结果；此处仅确认解析为 0。
	interval := heartbeatIntervalFromEnv(env)
	if interval != 0 {
		t.Fatalf("环境变量 0 应解析为关闭（0）, got %v", interval)
	}
	// store 是假存储；NewWriter 在解析为 0 时应返回 nil。
	store := &fakeBeatStore{}
	// writer 是 NewWriter 的构造结果，应为 nil（关闭）。
	writer := newWriterWithInterval(store, interval)
	if writer != nil {
		t.Fatalf("关闭状态 NewWriter 应返回 nil, got %+v", writer)
	}
}

// TestHeartbeatIntervalFromEnv_Fallback 覆盖未配置、非法与显式关闭三类回落。
func TestHeartbeatIntervalFromEnv_Fallback(t *testing.T) {
	// cases 是表驱动用例：环境变量文本与期望周期。
	cases := []struct {
		// name 是用例名称。
		name string
		// raw 是环境变量文本。
		raw string
		// want 是期望周期；0 表示关闭。
		want time.Duration
	}{
		{"未配置回落默认", "", defaultHeartbeatInterval},
		{"非法非数字回落默认", "abc", defaultHeartbeatInterval},
		{"负数回落默认", "-5", defaultHeartbeatInterval},
		{"显式关闭", "0", 0},
		{"合法正数", "45", 45 * time.Second},
	}
	for // tc 表示当前遍历过程中的用例。
	_, tc := range cases {
		// env 是当前用例的环境变量函数。
		env := func(string) string { return tc.raw }
		// got 是本次解析得到的周期。
		got := heartbeatIntervalFromEnv(env)
		if tc.want == 0 {
			if got != 0 {
				t.Fatalf("%s: 应解析为关闭(0), got %v", tc.name, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("%s: 解析(%q) = %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
}

// newWriterWithInterval 是测试辅助：直接用指定周期构造 Writer，绕过环境变量关闭判定。
func newWriterWithInterval(store BeatWriter, interval time.Duration) *Writer {
	if interval <= 0 {
		return nil
	}
	return &Writer{store: store, interval: interval, done: make(chan struct{}), now: time.Now, logger: slog.Default()}
}

// fakeClock 提供可注入的当前时间，供测试确定性驱动。
type fakeClock struct {
	// now 是假时钟返回的当前时刻。
	now time.Time
}

// nowFunc 把固定时刻包装成 func() time.Time，满足写者可注入时钟的签名。
func (c *fakeClock) nowFunc() time.Time {
	return c.now
}

// TestNewWriter_EnabledAndDisabled 用真实构造入口覆盖 NewWriter 的启用、显式关闭与未装配三条分支。
func TestNewWriter_EnabledAndDisabled(t *testing.T) {
	// store 是假存储，供构造期注入。
	store := &fakeBeatStore{}
	t.Setenv(heartbeatIntervalEnv, "45")
	// enabled 是有效配置下的写者，周期应为配置值。
	enabled := NewWriter(store, slog.Default())
	if enabled == nil {
		t.Fatal("有效配置应构造出写者")
	}
	if enabled.interval != 45*time.Second {
		t.Fatalf("周期应为 45s, got %v", enabled.interval)
	}
	// 显式关闭：环境变量为 0 时不应构造写者，调用方据此跳过启动。
	t.Setenv(heartbeatIntervalEnv, "0")
	if closed := NewWriter(store, slog.Default()); closed != nil {
		t.Fatalf("显式关闭应返回 nil, got %+v", closed)
	}
	// 未装配存储：即便配置有效也不应构造空转写者。
	t.Setenv(heartbeatIntervalEnv, "30")
	if nilStore := NewWriter(nil, slog.Default()); nilStore != nil {
		t.Fatalf("未装配存储应返回 nil, got %+v", nilStore)
	}
}
