package automation

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// silentTestLogger 返回丢弃全部输出的测试日志器，保持断言输出干净。
func silentTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

// discardWriter 是丢弃全部写入的 io.Writer。
type discardWriter struct{}

// Write 丢弃内容并报告全部写入成功。
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// fakeSilenceClock 是可手动推进的假时钟，保证静默判定测试的确定性。
type fakeSilenceClock struct {
	// current 是当前假时间；测试通过 set 前进。
	current time.Time
}

// set 把假时钟推进到指定时刻。
func (f *fakeSilenceClock) set(t time.Time) { f.current = t }

// now 返回当前假时间，匹配 silenceWatchdog.now 的签名。
func (f *fakeSilenceClock) now() time.Time { return f.current }

// fakeSilenceReader 是可编程的最近业务活动读取替身。
type fakeSilenceReader struct {
	// last 是每次读取返回的最近业务活动时间。
	last time.Time
	// err 非 nil 时模拟数据库查询失败。
	err error
	// calls 记录读取被调用次数，用于验证阈值关闭时不发起查询。
	calls int
}

// read 返回预设的业务活动时间或错误，并累计调用次数。
func (f *fakeSilenceReader) read(context.Context) (time.Time, error) {
	f.calls++
	return f.last, f.err
}

// fakeSilenceAlerter 记录业务静默告警的发送内容。
type fakeSilenceAlerter struct {
	// titles 是每次告警的标题列表，长度即告警次数。
	titles []string
}

// NotifyBusinessSilence 记录一次告警发送。
func (f *fakeSilenceAlerter) NotifyBusinessSilence(_ context.Context, title, _ string) {
	f.titles = append(f.titles, title)
}

// newTestSilenceWatchdog 基于假时钟、假读取器与假发送器构造被测看门狗。
func newTestSilenceWatchdog(threshold time.Duration, reader *fakeSilenceReader, alerter BusinessSilenceAlerter, clock *fakeSilenceClock) *silenceWatchdog {
	return &silenceWatchdog{
		threshold:    threshold,
		readActivity: reader.read,
		alerter:      alerter,
		now:          clock.now,
		logger:       silentTestLogger(),
	}
}

// TestSilenceShouldAlert 覆盖静默判定的全部边界：阈值关闭、零值活动、恰等阈值与未超阈。
func TestSilenceShouldAlert(t *testing.T) {
	// base 是统一的判定基准时刻。
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// cases 是表驱动用例。
	cases := []struct {
		// name 是用例名称。
		name string
		// last 是最近业务活动时间。
		last time.Time
		// now 是当前时间。
		now time.Time
		// threshold 是告警阈值。
		threshold time.Duration
		// want 是期望判定结果。
		want bool
	}{
		{"阈值关闭不告警", base.Add(-time.Hour), base, 0, false},
		{"负阈值视为关闭", base.Add(-time.Hour), base, -time.Minute, false},
		{"零值活动不告警", time.Time{}, base, time.Hour, false},
		{"未超阈不告警", base.Add(-59 * time.Minute), base, time.Hour, false},
		{"恰等阈值告警", base.Add(-time.Hour), base, time.Hour, true},
		{"超阈告警", base.Add(-2 * time.Hour), base, time.Hour, true},
	}
	for // tc 表示当前遍历过程中的用例
	_, tc := range cases {
		// got 保存实际判定结果。
		got := silenceShouldAlert(tc.last, tc.now, tc.threshold)
		if got != tc.want {
			t.Fatalf("%s: silenceShouldAlert = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSilenceWatchdogCheck 覆盖看门狗状态机的完整生命周期：告警一次、静默期内去重、恢复后重置。
func TestSilenceWatchdogCheck(t *testing.T) {
	// base 是看门狗启动时的基准时刻，也是夹具里的最近业务活动时间。
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// threshold 是测试统一使用的告警阈值。
	threshold := 30 * time.Minute
	// reader、alerter、clock 是共享的可编程替身。
	reader := &fakeSilenceReader{last: base}
	// alerter 记录整个生命周期内的告警发送次数。
	alerter := &fakeSilenceAlerter{}
	// clock 是可手动推进的假时钟，驱动静默判定。
	clock := &fakeSilenceClock{current: base}
	// watchdog 是被测对象。
	watchdog := newTestSilenceWatchdog(threshold, reader, alerter, clock)
	// ctx 是检查用的生命周期上下文。
	ctx := context.Background()

	// 步骤一：业务活跃，不应告警。
	watchdog.check(ctx)
	if len(alerter.titles) != 0 {
		t.Fatalf("业务活跃时不应告警, got %d 次", len(alerter.titles))
	}

	// 步骤二：时间推进到刚好超阈且业务仍停留在 base，应告警一次。
	clock.set(base.Add(threshold + time.Minute))
	watchdog.check(ctx)
	if len(alerter.titles) != 1 {
		t.Fatalf("静默超阈应告警一次, got %d 次", len(alerter.titles))
	}

	// 步骤三：继续推进时间仍静默，不得重复告警。
	clock.set(base.Add(2*threshold + time.Minute))
	watchdog.check(ctx)
	if len(alerter.titles) != 1 {
		t.Fatalf("静默期内不应重复告警, got %d 次", len(alerter.titles))
	}

	// 步骤四：业务恢复（活动时间跳到当前），状态应重置且不发恢复通知。
	reader.last = base.Add(2*threshold + time.Minute)
	watchdog.check(ctx)
	if len(alerter.titles) != 1 {
		t.Fatalf("业务恢复不应发送新告警, got %d 次", len(alerter.titles))
	}
	watchdog.mu.Lock()
	// resetAfterRecovery 表示恢复后告警状态应回到未告警。
	resetAfterRecovery := !watchdog.alerted
	watchdog.mu.Unlock()
	if !resetAfterRecovery {
		t.Fatal("业务恢复后看门狗状态未重置")
	}

	// 步骤五：恢复后再次静默超阈，应能再次告警。
	reader.last = base
	clock.set(base.Add(3*threshold + 2*time.Minute))
	watchdog.check(ctx)
	if len(alerter.titles) != 2 {
		t.Fatalf("恢复后再次静默应再次告警, got %d 次", len(alerter.titles))
	}
}

// TestSilenceWatchdogDisabledAndFailures 覆盖阈值关闭与查询失败路径：不查询、不告警、不改变状态。
func TestSilenceWatchdogDisabledAndFailures(t *testing.T) {
	// base 是统一的判定基准时刻。
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	// clock 是共享假时钟。
	clock := &fakeSilenceClock{current: base}

	// 场景一：阈值为 0 时连活动查询都不发起。
	// closedReader 是阈值关闭场景的读取替身。
	closedReader := &fakeSilenceReader{last: base.Add(-time.Hour)}
	// closedWatchdog 是阈值关闭的被测对象。
	closedWatchdog := newTestSilenceWatchdog(0, closedReader, &fakeSilenceAlerter{}, clock)
	closedWatchdog.check(context.Background())
	if closedReader.calls != 0 {
		t.Fatalf("阈值关闭时不应发起活动查询, got %d 次", closedReader.calls)
	}

	// 场景二：查询失败时记日志、不告警、不置告警状态。
	// failingReader 模拟数据库故障。
	failingReader := &fakeSilenceReader{err: errors.New("数据库不可用")}
	// failingAlerter 记录失败场景下是否误发告警。
	failingAlerter := &fakeSilenceAlerter{}
	// failingWatchdog 是查询失败场景的被测对象。
	failingWatchdog := newTestSilenceWatchdog(time.Hour, failingReader, failingAlerter, clock)
	failingWatchdog.check(context.Background())
	if len(failingAlerter.titles) != 0 {
		t.Fatal("查询失败时不应告警")
	}
	failingWatchdog.mu.Lock()
	// stayedIdle 表示失败后状态应保持未告警。
	stayedIdle := !failingWatchdog.alerted
	failingWatchdog.mu.Unlock()
	if !stayedIdle {
		t.Fatal("查询失败后不应置为已告警状态")
	}

	// 场景三：发送器为 nil 时仍应完成状态流转（只记日志），避免恢复逻辑被跳过。
	// nilAlerterWatchdog 是无发送器的被测对象。
	nilAlerterWatchdog := newTestSilenceWatchdog(time.Hour, &fakeSilenceReader{last: base.Add(-2 * time.Hour)}, nil, clock)
	nilAlerterWatchdog.check(context.Background())
	nilAlerterWatchdog.mu.Lock()
	// alertedWithoutSender 表示无发送器时也应置已告警，保证静默期内去重。
	alertedWithoutSender := nilAlerterWatchdog.alerted
	nilAlerterWatchdog.mu.Unlock()
	if !alertedWithoutSender {
		t.Fatal("无发送器时静默超阈仍应置已告警状态")
	}
}

// TestSilenceThresholdFromEnv 覆盖阈值解析的默认、关闭、非法与自定义分支。
func TestSilenceThresholdFromEnv(t *testing.T) {
	// defaultMinutes 是期望的默认阈值。
	defaultMinutes := defaultSilenceAlertMinutes * time.Minute
	// cases 是表驱动用例。
	cases := []struct {
		// name 是用例名称。
		name string
		// raw 是环境变量文本。
		raw string
		// want 是期望阈值。
		want time.Duration
	}{
		{"未配置回落默认", "", defaultMinutes},
		{"显式关闭", "0", 0},
		{"自定义分钟", "45", 45 * time.Minute},
		{"非法文本回落默认", "abc", defaultMinutes},
		{"负数回落默认", "-5", defaultMinutes},
		{"空白回落默认", "  ", defaultMinutes},
	}
	for // tc 表示当前遍历过程中的用例
	_, tc := range cases {
		// got 保存实际解析阈值。
		got := silenceThresholdFromEnv(func(string) string { return tc.raw })
		if got != tc.want {
			t.Fatalf("%s: threshold = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolveSilenceThreshold 验证读取优先级：设置命中 > 设置为空回落 env > 都为空回落默认 > 非法值回落默认。
func TestResolveSilenceThreshold(t *testing.T) {
	// defaultMinutes 是期望的默认阈值。
	defaultMinutes := defaultSilenceAlertMinutes * time.Minute
	// cases 覆盖四种优先级分支，确保数据库配置真正压过环境变量。
	cases := []struct {
		// name 是当前用例名称。
		name string
		// setting 是数据库系统设置值；ok 为 false 时模拟未配置或读取失败。
		setting string
		// ok 表示是否预置数据库设置值。
		ok bool
		// env 是环境变量 XIANYU_SILENCE_ALERT_MINUTES 的值；空串表示未设置。
		env string
		// want 是期望解析出的阈值。
		want time.Duration
	}{
		{name: "设置命中优先于环境变量", setting: "90", ok: true, env: "999", want: 90 * time.Minute},
		{name: "设置为空回落到环境变量", setting: "", ok: true, env: "60", want: 60 * time.Minute},
		{name: "设置与环境变量都为空回落默认", setting: "", ok: true, env: "", want: defaultMinutes},
		{name: "设置非法值回落到环境变量", setting: "bad", ok: true, env: "45", want: 45 * time.Minute},
		{name: "设置负值回落到环境变量", setting: "-9", ok: true, env: "45", want: 45 * time.Minute},
		{name: "数据库读取失败回落到环境变量", setting: "90", ok: false, env: "30", want: 30 * time.Minute},
	}
	for // tc 表示当前遍历过程中的用例
	_, tc := range cases {
		// getSetting 是注入的假设置读取函数；ok 为 false 时返回错误模拟读取失败。
		var getSetting func(context.Context, string) (string, error)
		if tc.ok {
			getSetting = func(_ context.Context, _ string) (string, error) { return tc.setting, nil }
		} else {
			getSetting = func(_ context.Context, _ string) (string, error) { return "", errors.New("db unavailable") }
		}
		// getenv 是注入的假环境变量读取函数。
		getenv := func(string) string { return tc.env }
		// got 保存实际解析阈值。
		got := resolveSilenceThreshold(context.Background(), getSetting, getenv)
		if got != tc.want {
			t.Fatalf("%s: threshold = %v, want %v", tc.name, got, tc.want)
		}
	}
}
