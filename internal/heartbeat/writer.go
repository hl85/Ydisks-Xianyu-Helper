// Package heartbeat 实现进程级心跳：让宿主（Docker healthcheck / systemd / 运维脚本）
// 能判断「这个进程到底还活着吗」，补上业务看门狗（挂在自动化调度循环上）覆盖不到的场景——
// 当调度循环自身卡死时，看门狗跟着一起死，而独立的心跳 goroutine 由进程级生命周期统一拥有，
// 只要 Go 调度器还能推进就会持续打点，宿主据此即可发现进程停滞。
//
// 红线（用户明确关切，且由 platform_guard_test 证明）：心跳严禁发起任何平台（闲鱼）请求。
// 本包不 import 任何平台包，心跳写者只调用注入的 BeatWriter（由 db.HeartbeatStore 实现，
// 仅写入我们自己的数据库），不触达消息、mtop、cookie 刷新、登录或 WebSocket。
package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BeatWriter 定义心跳写者所需的最小持久化能力；由 db.HeartbeatStore 实现，测试注入假实现。
// 接口刻意只暴露 Beat，确保写者除了写自有数据库外没有任何外部副作用。
type BeatWriter interface {
	// Beat 把给定时刻写入本进程的心跳行；at 由写者时钟提供。
	Beat(ctx context.Context, at time.Time) error
}

// heartbeatIntervalEnv 控制心跳写周期（秒）：默认 30，设为 0 关闭，非法值回落默认。
const heartbeatIntervalEnv = "XIANYU_HEARTBEAT_INTERVAL_SECONDS"

// defaultHeartbeatInterval 是未配置或非法时的默认心跳周期（秒）。
const defaultHeartbeatInterval = 30 * time.Second

// minHeartbeatInterval 是允许的最小心跳周期，避免配置过密拖垮数据库。
const minHeartbeatInterval = 1 * time.Second

// Writer 周期性把当前时间写入心跳表，供宿主判断进程是否存活。
//
// 并发与生命周期（AGENTS 1.6）：
//   - 归属：由 cmd 组合根登记为生命周期组件，通过 FuncComponent 的 StartFunc 启 goroutine、
//     CloseFunc 等待退出；归属明确，不新创建无人管的后台协程。
//   - 取消：Run 持有共享生命周期 ctx，ctx 取消即停止写循环并关闭 done。
//   - 等待：WaitContext 在 done 关闭前阻塞，关闭流程据此 Join，不留 goroutine。
//   - 时钟：now 可注入，测试用假时钟驱动周期与判定。
//   - 失败语义：写失败只记告警（fail-open），绝不阻塞业务循环或向上抛错。
type Writer struct {
	// mu 保护运行期只读字段；本类型运行期不修改可变状态，mu 主要约束并发构造。
	mu sync.Mutex
	// store 是心跳写出的持久化边界；为 nil 时 Writer 视为未装配，Run 直接返回。
	store BeatWriter
	// interval 是心跳写周期；构造期固化，运行期不再读环境变量。
	interval time.Duration
	// runOnce 保证一个 Writer 只启动一个由 ctx 管理的循环。
	runOnce sync.Once
	// done 在写循环退出后关闭，供关闭流程等待 goroutine 收束。
	done chan struct{}
	// now 返回当前时间；生产为 time.Now，测试注入假时钟。
	now func() time.Time
	// logger 记录心跳写失败；不含任何凭证内容。
	logger *slog.Logger
}

// NewWriter 构造进程心跳写者；store 为 nil 时返回 nil。
// 周期从环境变量 XIANYU_HEARTBEAT_INTERVAL_SECONDS 读取：0 表示显式关闭，返回 nil 让调用方跳过启动；
// 未配置或非法（非数字、负数）回落默认 30 秒；有效正数低于下限时提升到下限。
func NewWriter(store BeatWriter, logger *slog.Logger) *Writer {
	if store == nil {
		return nil
	}
	// interval 是本次构造确定的心跳周期；为 0 表示关闭。
	interval := heartbeatIntervalFromEnv(os.Getenv)
	if interval <= 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Writer{
		store:    store,
		interval: interval,
		done:     make(chan struct{}),
		now:      time.Now,
		logger:   logger,
	}
}

// heartbeatIntervalFromEnv 从环境变量解析心跳周期（秒）。
// 未配置回落默认；0 表示关闭，由调用方据此返回 nil；非法（非数字、负数）回落默认；
// 有效正数低于下限时提升到下限，避免配置过密。
func heartbeatIntervalFromEnv(getenv func(string) string) time.Duration {
	// raw 是环境变量的原始文本；空串表示用户未配置。
	raw := strings.TrimSpace(getenv(heartbeatIntervalEnv))
	if raw == "" {
		return defaultHeartbeatInterval
	}
	// seconds 是解析出的秒数；解析失败视为非法，回落默认。
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return defaultHeartbeatInterval
	}
	// 显式配置 0 表示用户主动关闭心跳写者，返回 0 让上层跳过启动。
	if seconds == 0 {
		return 0
	}
	// 负数属于非法配置：按契约回落默认周期，而不是被下限保护当成极密周期。
	if seconds < 0 {
		return defaultHeartbeatInterval
	}
	// interval 是把秒数换算成的周期；低于下限时提升到下限保护数据库。
	interval := time.Duration(seconds) * time.Second
	if interval < minHeartbeatInterval {
		return minHeartbeatInterval
	}
	return interval
}

// Run 启动心跳写循环；调用方应在 goroutine 中启动并用 ctx 控制生命周期。
// nil 接收者或 store 未装配时直接返回，不创建任何 goroutine。
func (w *Writer) Run(ctx context.Context) {
	if w == nil {
		return
	}
	if w.store == nil {
		// 未装配存储时视作「已结束」：关闭 done 让关闭流程的 Join 立即返回，
		// 否则 WaitContext 会永久阻塞在一个永远不会开始的循环上。
		w.runOnce.Do(func() {
			if w.done != nil {
				close(w.done)
			}
		})
		return
	}
	w.runOnce.Do(func() {
		// 无论循环因何退出，都必须关闭 done，供 WaitContext 解除等待避免 goroutine 泄漏。
		defer close(w.done)
		if ctx.Err() != nil {
			return
		}
		// 启动即写一次，缩短健康检查发现空窗（避免进程已起但首跳未到的误判）。
		w.beat(ctx)
		// ticker 按配置周期推进心跳；周期过短已被构造期下限保护。
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				// ctx 取消即停止写循环并退出 goroutine，不做任何阻塞等待。
				return
			case <-ticker.C:
				w.beat(ctx)
			}
		}
	})
}

// beat 写出一次心跳；写失败只记告警不阻塞循环，保证 fail-open。
func (w *Writer) beat(ctx context.Context) {
	// at 是本次心跳的时间基准；使用可注入时钟，保证测试确定性。
	at := w.now()
	// err 保存心跳写入失败；失败只告警，绝不中断业务或向上抛错。
	if err := w.store.Beat(ctx, at); err != nil {
		w.logger.Warn("写入进程心跳失败", "err", err)
	}
}

// WaitContext 在 ctx 约束内等待心跳写循环退出，避免关闭流程无限阻塞。
// Writer 未装配或已退出时立即返回 nil。
func (w *Writer) WaitContext(ctx context.Context) error {
	if w == nil || w.done == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("等待心跳写者需要关闭 Context")
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
