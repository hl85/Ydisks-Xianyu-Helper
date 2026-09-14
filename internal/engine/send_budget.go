// send_budget.go 进程级全局发送日额度（多账号合计）。
// 归属：由账号管理器（internal/account Manager）构造并持有，经 engine.Config.GlobalBudget 注入每个账号的发送闸门共享。
// 锁：mu 只保护 day 与 used；临界区内仅做算术，持久化写穿在锁外执行，且与发送闸门锁永不嵌套持有。
// 关停：不启动后台协程，没有需要显式释放的资源，随持有方（Manager）一起退出。

package engine

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrSendGateGlobalDailyLimit 表示全部账号合计的当日发送额度已用尽，需要等到次日或由人工提高额度。
var ErrSendGateGlobalDailyLimit = errors.New("全局当日发送额度已用尽")

// sendGateGlobalDailyLimitEnv 配置全部账号合计的每日发送条数上限；留空或 0 表示不限制。
const sendGateGlobalDailyLimitEnv = "XIANYU_GLOBAL_SEND_DAILY_LIMIT"

// SendBudgetRestoreTimeout 是构造期从 DB 恢复全局用量的有限收口预算，供外部接线方复用。
const SendBudgetRestoreTimeout = 5 * time.Second

// globalSendDailyLimitSetting 是数据库系统设置中保存多账号全局日发送额度的键；
// 未配置时回落到环境变量 XIANYU_GLOBAL_SEND_DAILY_LIMIT，再回落到不限（0）。
const globalSendDailyLimitSetting = "global_send_daily_limit"

// ResolveGlobalSendDailyLimit 解析多账号全局日发送额度，优先级：数据库设置 > 环境变量 > 缺省不限（0）。
// getSetting 从系统设置读取；为 nil 时跳过设置层直接读环境变量。getenv 读取环境变量。
// 设置或环境变量非法（非非负整数）时按缺省不限处理，避免手滑配置悄悄关掉限额。
func ResolveGlobalSendDailyLimit(ctx context.Context, getSetting func(context.Context, string) (string, error), getenv func(string) string) int {
	if getSetting != nil {
		// dbValue、dbErr 是数据库设置读取结果；读取成功且为合法非负整数时优先采用。
		if dbValue, dbErr := getSetting(ctx, globalSendDailyLimitSetting); dbErr == nil {
			// limit 是设置解析出的额度；合法即采用，保证界面配置优先于环境变量。
			if limit, parseErr := strconv.Atoi(strings.TrimSpace(dbValue)); parseErr == nil && limit >= 0 {
				return limit
			}
		}
	}
	// 设置缺失或非法时回落到环境变量，0 表示不限。
	return envInt(sendGateGlobalDailyLimitEnv, 0)
}

// SendBudget 是多账号共享的进程级日发送预算。
type SendBudget struct {
	// limit 是全部账号合计的每日发送上限；小于等于 0 表示不限制。
	limit int
	// persist 是全局桶（cookieID 为空字符串）的计数持久化仓储；为空时退化为纯内存预算。
	persist SendCounterPersistence
	// now 返回当前时间；注入假时钟后测试可验证跨日滚动。
	now func() time.Time
	// logger 用于写穿与恢复失败的告警；为空时静默降级。
	logger *slog.Logger
	// mu 保护 day 与 used 两个字段。
	mu sync.Mutex
	// day 是 used 所属的本地自然日，格式为 2006-01-02。
	day string
	// used 是当日已被全部账号合计占用的发送条数。
	used int
}

// NewSendBudget 构造全局日发送预算；limit 小于等于 0 表示不限制，此时占用与写穿都不生效。
// persist 为全局桶计数仓储，可为空；logger 用于告警，可为空。
func NewSendBudget(limit int, persist SendCounterPersistence, logger *slog.Logger) *SendBudget {
	return &SendBudget{limit: limit, persist: persist, now: time.Now, logger: logger}
}

// NewSendBudgetFromEnv 读取 XIANYU_GLOBAL_SEND_DAILY_LIMIT 构造全局日发送预算；未设置或非法时为不限。
func NewSendBudgetFromEnv(persist SendCounterPersistence, logger *slog.Logger) *SendBudget {
	return NewSendBudget(envInt(sendGateGlobalDailyLimitEnv, 0), persist, logger)
}

// Restore 从持久化恢复今日全局用量，取内存与 DB 的较大值，防止重启后全局额度被放大。
// ctx 是恢复读取的取消边界；恢复失败只告警，不阻断启动（fail-open）。
func (b *SendBudget) Restore(ctx context.Context) {
	// 未启用额度或未接持久化时无事可做；nil 接收者视为未装配。
	if b == nil || b.limit <= 0 || b.persist == nil {
		return
	}
	// now 是本次恢复使用的取时实现；与 acquire 保持同一缺省策略。
	now := b.now
	if now == nil {
		now = time.Now
	}
	// today 是本次恢复对应的目标自然日；只恢复当天，历史桶不进内存。
	today := now().Format("2006-01-02")
	// dbCount、err 是 DB 中全局桶今日已占用条数与读取错误。
	dbCount, err := b.persist.GetSendCount(ctx, "", today)
	if err != nil {
		if b.logger != nil {
			b.logger.Warn("恢复全局发送额度用量失败，本次启动按纯内存预算运行", "day", today, "err", err)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.day == "" {
		b.day = today
	}
	// 恢复期间内存仍为构造初值；仅当 DB 更高时抬升，避免缩小任何已消耗的额度。
	if dbCount > b.used {
		b.used = dbCount
	}
}

// acquire 占用一个全局发送槽并返回所属自然日；超限返回 ErrSendGateGlobalDailyLimit。
// limit 小于等于 0 时直接放行且不计数；占用成功后在锁外写穿全局桶，失败只告警。
// nil 接收者视为未装配，直接放行。
func (b *SendBudget) acquire(ctx context.Context) (string, error) {
	if b == nil {
		return "", nil
	}
	// now 是本次占用使用的取时实现。
	now := b.now
	if now == nil {
		now = time.Now
	}
	// current 是本次占用的判定时刻。
	current := now()
	if b.limit <= 0 {
		return current.Format("2006-01-02"), nil
	}
	b.mu.Lock()
	// today 是 current 所属的本地自然日；跨日时重置全局用量。
	today := current.Format("2006-01-02")
	if b.day != today {
		b.day = today
		b.used = 0
	}
	if b.used >= b.limit {
		b.mu.Unlock()
		return "", ErrSendGateGlobalDailyLimit
	}
	b.used++
	b.mu.Unlock()
	// 写穿在锁外执行；失败只告警，预算退化为纯内存。
	if b.persist != nil {
		// total、err 是写穿返回的总条数与错误；total 暂不消费。
		if _, err := b.persist.AddSendCount(ctx, "", today, 1); err != nil {
			if b.logger != nil {
				b.logger.Warn("全局发送额度写穿失败，退化为纯内存预算", "day", today, "err", err)
			}
		}
	}
	return today, nil
}
