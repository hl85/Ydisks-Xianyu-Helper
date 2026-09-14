package automation

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"xianyu-go/internal/db"
)

// silenceAlertEnv 控制业务静默看门狗的告警阈值（分钟）：默认 180，0 表示整体关闭。
// 阈值用环境变量而非系统设置，是为了在上游接口长时间无事件时与其它止血开关保持同一运维方式。
const silenceAlertEnv = "XIANYU_SILENCE_ALERT_MINUTES"

// silenceAlertSetting 是数据库系统设置中保存业务静默告警阈值的键；
// 未配置时回落到环境变量 XIANYU_SILENCE_ALERT_MINUTES，再回落到默认 180 分钟。
const silenceAlertSetting = "silence_alert_minutes"

// silenceWatchdogResolveTimeout 是构造期从 DB 读取静默阈值的有限收口预算；
// 按架构门禁要求显式限时，防止系统设置读取无限等待阻塞自动化中心构造。
const silenceWatchdogResolveTimeout = 5 * time.Second

// defaultSilenceAlertMinutes 是未配置环境变量时的默认静默告警阈值（分钟），
// 覆盖闲鱼卖家夜间无买家的真实闲时，避免正常低谷误报。
const defaultSilenceAlertMinutes = 180

// businessSilenceEventType 是业务静默告警的通知事件类型编码；
// 与 notify.EventBusinessSilence 保持同一字符串，automation 包不反向依赖 notify。
const businessSilenceEventType = "business_silence"

// businessSilenceAlertLevel 是业务静默告警的通知级别；静默属于需要人工介入的警告而非错误。
const businessSilenceAlertLevel = "warn"

// BusinessSilenceActivityReader 读取最近一次业务活动时间；返回零值 time.Time 表示数据库尚无任何业务记录。
// 由装配层把 db.AnalyticsQueries.LatestBusinessActivityAt 注入为函数值，本包不直接依赖具体仓储。
type BusinessSilenceActivityReader func(ctx context.Context) (time.Time, error)

// BusinessSilenceAlerter 发送进程级业务静默告警；实现负责把无归属账号的事件路由到可达的通知渠道。
type BusinessSilenceAlerter interface {
	// NotifyBusinessSilence 发送一条业务静默告警；ctx 只约束本次发送前的查询，不影响通知器内部入队。
	NotifyBusinessSilence(ctx context.Context, title, body string)
}

// silenceWatchdog 是业务静默看门狗的状态持有者。
//
// 并发与生命周期（AGENTS 1.6）：
//   - 归属：由 Center 在构造时创建并持有，仅被 automation.Scheduler 的分钟级扫描循环调用（单 goroutine 串行入口）。
//   - 锁：mu 保护 alerted 与 lastAlertAt；测试并发调用时同样安全。
//   - 关停：本类型不启动任何 goroutine；活动查询使用调用方传入的 ctx，Scheduler 的 ctx 取消时循环退出，
//     看门狗随之停止，无需独立取消路径。
type silenceWatchdog struct {
	// mu 保护 alerted 与 lastAlertAt；不加读写锁是因为调用频率为分钟级，写多读少。
	mu sync.Mutex
	// alerted 表示当前静默周期内是否已发出过告警；业务恢复后重置为 false，保证静默期内只告警一次。
	alerted bool
	// lastAlertAt 记录最近一次告警发出时刻，供日志与测试核对去重窗口。
	lastAlertAt time.Time
	// threshold 是触发告警的最小静默时长；小于等于 0 表示看门狗整体关闭，不做任何查询。
	threshold time.Duration
	// readActivity 读取最近业务活动时间；为 nil 时看门狗视为未装配，直接跳过。
	readActivity BusinessSilenceActivityReader
	// alerter 是可选的进程级告警发送器；为 nil 时只记日志不发通知，便于测试与降级运行。
	alerter BusinessSilenceAlerter
	// now 返回当前时间；生产为 time.Now，测试注入假时钟驱动静默判定。
	now func() time.Time
	// logger 记录告警、恢复与查询失败；不包含任何凭证内容。
	logger *slog.Logger
}

// newSilenceWatchdog 根据装配依赖构造看门狗；readActivity 为 nil 表示未装配，返回 nil 且扫描循环跳过。
// threshold 在构造期从系统设置读取（数据库设置优先），未配置时回落环境变量，再回落默认值；构造期固化，运行期不再读取。
// store 用于读取系统设置；为 nil 时退化为仅读环境变量。
func newSilenceWatchdog(store *db.Store, readActivity BusinessSilenceActivityReader, alerter BusinessSilenceAlerter, logger *slog.Logger) *silenceWatchdog {
	if readActivity == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	// getSetting 是系统设置读取函数；store 未装配设置仓储时退化为仅读环境变量。
	var getSetting func(context.Context, string) (string, error)
	if store != nil && store.Settings != nil {
		getSetting = store.Settings.Get
	}
	// resolveCtx、resolveCancel 是构造期读取静默阈值的有限收口预算；Center 无 owner Context 可继承，
	// 按架构门禁要求显式限时，防止系统设置读取无限等待阻塞自动化中心构造。
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), silenceWatchdogResolveTimeout)
	defer resolveCancel()
	// threshold 是解析出的静默告警阈值：系统设置优先、环境变量兜底、缺省 180 分钟。
	threshold := resolveSilenceThreshold(resolveCtx, getSetting, os.Getenv)
	return &silenceWatchdog{
		threshold:    threshold,
		readActivity: readActivity,
		alerter:      alerter,
		now:          time.Now,
		logger:       logger,
	}
}

// resolveSilenceThreshold 解析业务静默看门狗阈值，优先级：系统设置 > 环境变量 > 默认值。
// getSetting 从系统设置读取；为 nil 时跳过设置层直接读环境变量。getenv 读取环境变量。
// 设置或环境变量非法（非非负整数）时回落默认值 180 分钟，避免手滑配置悄悄关掉告警。
func resolveSilenceThreshold(ctx context.Context, getSetting func(context.Context, string) (string, error), getenv func(string) string) time.Duration {
	if getSetting != nil {
		// dbValue、dbErr 是系统设置读取结果；读取成功且为合法非负整数时优先采用。
		if dbValue, dbErr := getSetting(ctx, silenceAlertSetting); dbErr == nil {
			// minutes 是设置解析出的分钟数；合法即采用，保证界面配置优先于环境变量。
			if minutes, parseErr := strconv.Atoi(strings.TrimSpace(dbValue)); parseErr == nil && minutes >= 0 {
				return time.Duration(minutes) * time.Minute
			}
		}
	}
	// 设置缺失或非法时回落到环境变量解析（env 为空或非法则回落默认）。
	return silenceThresholdFromEnv(getenv)
}

// silenceThresholdFromEnv 从环境变量解析静默告警阈值。
// 未配置或无法解析为非负整数时回落默认值，避免手滑写错配置后悄悄关掉告警；
// 显式配置 0 表示用户主动关闭看门狗，负数视为非法配置回落默认。
func silenceThresholdFromEnv(getenv func(string) string) time.Duration {
	// raw 是环境变量的原始文本；空串表示用户未配置。
	raw := strings.TrimSpace(getenv(silenceAlertEnv))
	if raw == "" {
		return defaultSilenceAlertMinutes * time.Minute
	}
	// minutes 是解析出的分钟数；解析失败或为负数时回落默认值。
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes < 0 {
		return defaultSilenceAlertMinutes * time.Minute
	}
	return time.Duration(minutes) * time.Minute
}

// silenceShouldAlert 判定业务静默是否需要告警，是看门狗的核心纯函数。
// threshold 小于等于 0 表示看门狗关闭，永不告警；
// lastActivity 为零值表示数据库尚无任何业务记录（例如全新部署），此时无法区分「没生意」与「停摆」，不告警以免冷启动误报；
// 静默时长恰好等于阈值即视为超阈，保证阈值语义可被测试精确锚定。
func silenceShouldAlert(lastActivity, now time.Time, threshold time.Duration) bool {
	if threshold <= 0 {
		return false
	}
	if lastActivity.IsZero() {
		return false
	}
	return now.Sub(lastActivity) >= threshold
}

// check 执行一次静默检查：读取最近业务活动、判定是否告警或在业务恢复后重置状态。
// 活动查询失败只记日志并保持既有状态不变，绝不向扫描循环返回错误，
// 保证看门狗自身的故障不会影响分钟级业务扫描的调度节奏。
func (w *silenceWatchdog) check(ctx context.Context) {
	if w == nil {
		return
	}
	// threshold 是构造期固化的告警阈值快照；关闭时连活动查询都不发起，避免无谓数据库压力。
	threshold := w.threshold
	if threshold <= 0 {
		return
	}
	// last、readErr 保存最近业务活动时间及查询错误；查询失败时看门狗维持原状等待下一轮。
	last, readErr := w.readActivity(ctx)
	if readErr != nil {
		w.logger.Warn("业务静默检查读取最近业务活动失败", "err", readErr)
		return
	}
	// now 是本次检查的时间基准；由可注入时钟提供，保证测试确定性。
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if silenceShouldAlert(last, now, threshold) {
		if w.alerted {
			return
		}
		// silentFor 是截至本次检查的静默时长，用于告警正文给出可核对的量化信息。
		silentFor := now.Sub(last).Round(time.Minute)
		w.logger.Warn("业务静默告警：业务表长时间零事件", "last_activity", last.UTC().Format(time.RFC3339), "silent_for", silentFor.String(), "threshold", threshold.String())
		if w.alerter != nil {
			w.alerter.NotifyBusinessSilence(ctx,
				"业务静默告警",
				fmt.Sprintf("进程健康但业务表已 %s 无新事件（最近业务活动 %s，阈值 %s），请检查 WebSocket 连接与自动化运行状态。",
					silentFor, last.UTC().Format("2006-01-02 15:04:05 MST"), threshold))
		}
		w.alerted = true
		w.lastAlertAt = now
		return
	}
	if w.alerted {
		// 业务已恢复：重置告警状态，下一次静默超阈时才能再次告警；恢复本身只记日志不发通知。
		w.logger.Info("业务静默已恢复，重置看门狗告警状态", "last_activity", last.UTC().Format(time.RFC3339))
		w.alerted = false
	}
}
