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
	"log/slog"
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

// SendCounterPersistence 是发送闸门对计数持久化的最小使用方接口；
// 由使用方（如 *db.SendCounterStore）隐式实现，engine 不依赖具体存储类型。
type SendCounterPersistence interface {
	// GetSendCount 读取指定账号在指定自然日已登记的发送条数；无记录返回 0。
	GetSendCount(ctx context.Context, cookieID, day string) (int, error)
	// AddSendCount 在指定账号指定自然日累加 delta 条，并返回累加后的总条数。
	AddSendCount(ctx context.Context, cookieID, day string, delta int) (int64, error)
}

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
	// persist 是可选的发送计数持久化仓储；为空时闸门退化为纯内存日额度。
	persist SendCounterPersistence
	// cookieID 是当前账号标识，与 day 一起构成持久化计数键；由接线方在构造后注入。
	cookieID string
	// logger 用于记录写穿与恢复失败的告警；为空时静默降级。
	logger *slog.Logger
}

// attachPersistence 为闸门接入计数持久化，并按 (cookieID, 今天) 从 DB 恢复内存计数。
// 恢复取内存与 DB 的较大值：进程重启后内存从零重新累积，若 DB 已有更高用量必须继承，
// 防止重启成为绕过日额度的漏洞。恢复失败只告警，不阻断账号启动（fail-open）。
// ctx 是恢复读取的取消边界；global 预算的接入由独立方法完成，保持本方法只管账号桶。
func (g *sendGate) attachPersistence(ctx context.Context, persist SendCounterPersistence, cookieID string, logger *slog.Logger) {
	// now 取当前时刻使用的取时实现；与 acquire 保持同一缺省策略。
	now := g.now
	if now == nil {
		now = time.Now
	}
	g.persist = persist
	g.cookieID = cookieID
	g.logger = logger
	if persist == nil {
		return
	}
	// today 是本次恢复对应的目标自然日；只恢复当天，历史桶不进内存。
	today := now().Format("2006-01-02")
	// dbCount、err 是 DB 中今日已登记条数与读取错误。
	dbCount, err := persist.GetSendCount(ctx, cookieID, today)
	if err != nil {
		if logger != nil {
			logger.Warn("恢复发送计数失败，本次启动按纯内存额度运行", "account", cookieID, "day", today, "err", err)
		}
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// 恢复期间内存计数仍为构造初值；仅当 DB 更高时抬升，避免缩小任何已消耗的额度。
	if g.day == "" {
		g.day = today
	}
	if dbCount > g.sentToday {
		g.sentToday = dbCount
	}
}

// writeThrough 把本次已登记的账号发送写穿到持久化计数；I/O 必须在闸门锁外执行。
// 写入失败只记录告警，不阻断发送：持久化缺位时行为退化为纯内存日额度。
func (g *sendGate) writeThrough(ctx context.Context, day string) {
	// persist 是本次写穿使用的仓储快照；未接线时无事可做。
	persist := g.persist
	if persist == nil {
		return
	}
	// total、err 是累加后的总条数与写穿错误；total 暂不消费，保留给诊断日志。
	if _, err := persist.AddSendCount(ctx, g.cookieID, day, 1); err != nil {
		if g.logger != nil {
			g.logger.Warn("发送计数写穿失败，退化为纯内存日额度", "account", g.cookieID, "day", day, "err", err)
		}
	}
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
	// 写穿在锁外执行：闸门临界区不做 I/O；写穿失败只告警，行为退化为纯内存额度。
	g.writeThrough(ctx, today)
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
