// send_gate.go 账号级出站发送闸门。
// 它在回复、发货与商品卡片真正写入平台之前，统一施加发送间隔、日额度与静默时段约束，
// 避免短时间内的密集发送构成平台风控面。
// 设计来源：同类项目把「日额度 + 随机发送延迟 + 静默时段」作为风控底座的沉淀，此处按本仓库约定用 Go 重写。
//
// 归属：闸门由 outgoingMessageCoordinator 持有，每个账号一份，不跨账号共享。
// 锁：mu 只保护当天的计数与上次登记时刻，临界区内仅做算术，不睡眠、不做 I/O。
// 关停：闸门不启动任何后台协程，等待完全受调用方 ctx 控制，因此没有需要显式关停的资源。

package engine

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 发送闸门的配置键与默认值集中定义，便于审阅与调参。
const (
	// sendGateMinIntervalEnv 配置两次出站发送之间的基础最小间隔，单位毫秒。
	sendGateMinIntervalEnv = "XIANYU_SEND_MIN_INTERVAL_MS"
	// sendGateJitterEnv 配置叠加在基础间隔之上的随机抖动上限，单位毫秒，用于打散固定节奏。
	sendGateJitterEnv = "XIANYU_SEND_JITTER_MS"
	// sendGateDailyLimitEnv 配置单个账号每个自然日的发送条数上限；留空或 0 表示不限制。
	sendGateDailyLimitEnv = "XIANYU_SEND_DAILY_LIMIT"
	// sendGateQuietHoursEnv 配置静默时段，格式为「起始小时-结束小时」（本地时间 0-23）；留空表示不启用。
	sendGateQuietHoursEnv = "XIANYU_SEND_QUIET_HOURS"

	// sendGateDefaultMinInterval 是默认基础间隔：远低于正常客服与发货节奏，只为消除瞬时爆发。
	sendGateDefaultMinInterval = 1 * time.Second
	// sendGateDefaultJitter 是默认抖动上限，与默认基础间隔共同形成 1~2 秒的非固定节奏。
	sendGateDefaultJitter = 1 * time.Second
	// sendGateMaxWait 是单次发送允许等待的上限；超过它说明配置过于保守，按上限等待以免拖死调用链。
	sendGateMaxWait = 30 * time.Second
)

// ErrSendGateDailyLimit 表示账号当日发送额度已用尽，需要等到次日或由人工提高额度。
var ErrSendGateDailyLimit = errors.New("账号当日发送额度已用尽")

// ErrSendGateQuietHours 表示当前处于配置的静默时段；本次发送被拒绝，而不是原地等待数小时。
var ErrSendGateQuietHours = errors.New("当前处于发送静默时段")

// sendGateConfig 描述一个账号的发送闸门参数；全零配置表示闸门完全关闭。
type sendGateConfig struct {
	// MinInterval 是两次出站发送之间的基础最小间隔。
	MinInterval time.Duration
	// Jitter 是叠加在基础间隔之上的随机抖动上限，实际取值区间为 [0, Jitter)。
	Jitter time.Duration
	// DailyLimit 是每个自然日的发送条数上限；小于等于 0 表示不限制。
	DailyLimit int
	// QuietStartHour 是静默时段的起始小时（本地时间 0-23）。
	QuietStartHour int
	// QuietEndHour 是静默时段的结束小时（本地时间 0-23）；等于起始小时时表示不启用静默时段。
	QuietEndHour int
}

// enabled 返回该配置是否施加了任何限制；全零配置视为闸门关闭，发送路径不会因此产生任何等待。
func (c sendGateConfig) enabled() bool {
	return c.MinInterval > 0 || c.Jitter > 0 || c.DailyLimit > 0 || c.quietEnabled()
}

// quietEnabled 返回该配置是否定义了有效的静默时段。
func (c sendGateConfig) quietEnabled() bool {
	return c.QuietStartHour != c.QuietEndHour
}

// sendGateJitterFunc 为一次发送计算叠加的随机抖动；抽成函数类型是为了让测试摆脱随机性。
type sendGateJitterFunc func(max time.Duration) time.Duration

// randomSendGateJitter 返回 [0, max) 区间内的随机抖动。
// 这里只需要打散固定节奏，不承担安全用途，因此使用进程级伪随机源即可。
func randomSendGateJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

// sendGate 是单账号的出站发送闸门。
type sendGate struct {
	// cfg 是构造时固定的限制参数，运行期不再变化。
	cfg sendGateConfig
	// now 返回当前时间；注入假时钟后可完全摆脱真实等待。
	now func() time.Time
	// sleep 按调用方 ctx 等待指定时长；注入后可断言被要求的等待时长。
	sleep func(context.Context, time.Duration) error
	// jitter 计算本次发送叠加的随机抖动；注入后可得到确定结果。
	jitter sendGateJitterFunc
	// mu 保护 day、sentToday 与 lastSentAt 三个字段。
	mu sync.Mutex
	// day 是 sentToday 所属的本地自然日，格式为 2006-01-02。
	day string
	// sentToday 是本自然日内已经登记的发送条数。
	sentToday int
	// lastSentAt 是上一次登记发送的时刻，用于推算下一次最早可发送时间。
	lastSentAt time.Time
}

// newSendGate 按给定配置构造闸门，并使用生产实现填充取时、等待与抖动三个注入点。
func newSendGate(cfg sendGateConfig) *sendGate {
	return &sendGate{cfg: cfg, now: time.Now, sleep: sleepWithContext, jitter: randomSendGateJitter}
}

// acquire 在需要时等待，使本次出站发送符合配置的静默时段、日额度与最小间隔约束。
// ctx 是调用方的取消边界；返回错误表示本次发送被拒绝，调用方应把它当作「确定未发送」处理。
func (g *sendGate) acquire(ctx context.Context) error {
	// 未装配的闸门（例如测试中直接构造的协调器）不施加任何限制。
	if g == nil || !g.cfg.enabled() {
		return nil
	}
	// now 是本次调用的取时实现；缺省时回落到生产实现，保证零值以外路径也安全。
	now := g.now
	if now == nil {
		now = time.Now
	}
	// sleep 是本次调用的等待实现；缺省时回落到生产实现，等待始终受调用方 ctx 约束。
	sleep := g.sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	// jitter 是本次调用的抖动实现；缺省时回落到生产实现，用于打散固定节奏。
	jitter := g.jitter
	if jitter == nil {
		jitter = randomSendGateJitter
	}
	// current 是本次发送的判定时刻；整个流程只取一次，避免临界区内外时间漂移。
	current := now()
	if g.cfg.quietEnabled() && inQuietHours(current, g.cfg) {
		return ErrSendGateQuietHours
	}
	g.mu.Lock()
	// today 是 current 所属的本地自然日；跨日时重置当天用量。
	today := current.Format("2006-01-02")
	if g.day != today {
		g.day = today
		g.sentToday = 0
	}
	if g.cfg.DailyLimit > 0 && g.sentToday >= g.cfg.DailyLimit {
		g.mu.Unlock()
		return ErrSendGateDailyLimit
	}
	// interval 是本次发送需要遵守的最小间隔，由基础间隔与随机抖动共同决定。
	interval := g.cfg.MinInterval + jitter(g.cfg.Jitter)
	// earliest 是本账号下一次最早可发送时刻，由上次登记时刻加上本次间隔得到。
	earliest := g.lastSentAt.Add(interval)
	// wait 是相对 current 仍需等待的时长；负值意味着间隔已满足，可以直接发送。
	wait := earliest.Sub(current)
	if wait < 0 {
		wait = 0
	}
	if wait > sendGateMaxWait {
		// 配置过于保守时按上限等待，避免把调用链拖死；调用方 ctx 仍可提前取消。
		wait = sendGateMaxWait
	}
	// 先占位再放锁：并发调用会依次排到各自的时间片，而不是同时醒来后一起抢跑。
	g.lastSentAt = current.Add(wait)
	g.sentToday++
	g.mu.Unlock()
	return sleep(ctx, wait)
}

// inQuietHours 判断给定时刻是否落在配置的静默时段内；时段允许跨越午夜。
func inQuietHours(at time.Time, cfg sendGateConfig) bool {
	// hour 是当前时刻的本地小时读数。
	hour := at.Hour()
	if cfg.QuietStartHour < cfg.QuietEndHour {
		return hour >= cfg.QuietStartHour && hour < cfg.QuietEndHour
	}
	// 跨越午夜：例如 23-7 表示 23 点之后直到次日 7 点之前。
	return hour >= cfg.QuietStartHour || hour < cfg.QuietEndHour
}

// sleepWithContext 按调用方 ctx 等待指定时长；d 为非正数时不等待。
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	// timer 是本次等待的计时器；提前返回时必须停止它，避免计时器泄漏。
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sendGateConfigFromEnv 从进程环境读取发送闸门配置；未设置或非法的项沿用默认值。
func sendGateConfigFromEnv() sendGateConfig {
	// cfg 保存本次解析出的配置，先取带默认值的项，再补齐静默时段。
	cfg := sendGateConfig{
		MinInterval: envDurationMillis(sendGateMinIntervalEnv, sendGateDefaultMinInterval),
		Jitter:      envDurationMillis(sendGateJitterEnv, sendGateDefaultJitter),
		DailyLimit:  envInt(sendGateDailyLimitEnv, 0),
	}
	cfg.QuietStartHour, cfg.QuietEndHour = envQuietHours(sendGateQuietHoursEnv)
	return cfg
}

// envDurationMillis 读取以毫秒为单位的时长环境变量；未设置、非法或负数时返回 fallback。
func envDurationMillis(name string, fallback time.Duration) time.Duration {
	// raw 是环境变量的原始文本；空值表示沿用默认，不视为配置错误。
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	// millis、err 是解析结果与解析错误；非法输入沿用默认，以免配置笔误阻断发送。
	millis, err := strconv.Atoi(raw)
	if err != nil || millis < 0 {
		return fallback
	}
	return time.Duration(millis) * time.Millisecond
}

// envInt 读取整数环境变量；未设置、非法或负数时返回 fallback。
func envInt(name string, fallback int) int {
	// raw 是环境变量的原始文本；空值表示沿用默认。
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	// value、err 是解析结果与解析错误；非法输入沿用默认。
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// envQuietHours 解析「起始小时-结束小时」形式的静默时段；未设置或格式非法时返回 (0, 0)。
func envQuietHours(name string) (int, int) {
	// raw 是环境变量的原始文本；空值或单值（无法表达区间）都视为不启用。
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, 0
	}
	// parts 是「起始-结束」两段文本；不是两段就无法表达区间。
	parts := strings.Split(raw, "-")
	if len(parts) != 2 {
		return 0, 0
	}
	// start、startErr 是起始小时的解析结果；非法或越界即整体不启用。
	start, startErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	// end、endErr 是结束小时的解析结果；非法或越界即整体不启用。
	end, endErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if startErr != nil || endErr != nil || start < 0 || start > 23 || end < 0 || end > 23 {
		return 0, 0
	}
	return start, end
}
