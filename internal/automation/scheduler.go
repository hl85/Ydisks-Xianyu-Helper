package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"xianyu-go/internal/db"
)

// defaultReviewRequestScanInterval 用于本次流程后续判断的defaultReview请求ScanInterval
const defaultReviewRequestScanInterval = time.Minute

// defaultDeferredTaskScanInterval 是持久化延迟动作的轮询周期，保证秒级动作不会被分钟级业务扫描额外延后。
const defaultDeferredTaskScanInterval = time.Second

// legacySchedulerWaitTimeout 是兼容无 Context 等待入口的最长收束预算。
const legacySchedulerWaitTimeout = 10 * time.Second

// pendingShipCatchupEnv 控制「待发货兜底扫描」的开关：默认开启，显式设为 "0" 时整体跳过。
// 保留开关是为了在上游接口异常时能快速止血，而不必回滚二进制。
const pendingShipCatchupEnv = "XIANYU_PENDING_SHIP_CATCHUP"

// defaultPendingShipCooldown 是同一订单两次兜底触发之间的最小间隔。
// 付款事件在准备阶段失败时可能不会留下任何运行记录，若不设冷却会每分钟重试并放大上游压力。
const defaultPendingShipCooldown = 10 * time.Minute

// pendingShipTaskTimeout 是单次兜底任务的执行预算，避免上游接口挂起拖住整个分钟级扫描。
const pendingShipTaskTimeout = 90 * time.Second

// pendingShipResumeMaxAttempts 是同一运行允许被兜底续跑的最大代次，防止上游持续异常时无限重开。
const pendingShipResumeMaxAttempts = 5

// Scheduler 执行计划任务类自动化。
// 计划任务只负责“发现应该触发的任务”，具体动作仍交给 Center，避免形成第二套执行链。
// Scheduler 用于本次流程后续判断的Scheduler
type Scheduler struct {
	// center 是调度器唯一使用的自动化中心，负责实际执行延迟、恢复和求评价任务。
	center *Center
	// interval 是账号任务、恢复任务和求评价任务的分钟级扫描周期。
	interval time.Duration
	// deferredInterval 是已持久化延迟动作的秒级扫描周期，不影响其他计划任务的扫描频率。
	deferredInterval time.Duration
	// runOnce 保证一个调度器实例只启动一个由调用方 Context 管理的循环。
	runOnce sync.Once
	// done 在调度循环退出后关闭，供关闭流程等待全部调度工作停止。
	done chan struct{}
	// pendingShipMu 保护 pendingShipCooldown。
	pendingShipMu sync.Mutex
	// pendingShipCooldown 记录兜底扫描最近尝试过的订单时间，避免同一订单反复重试。
	pendingShipCooldown map[string]time.Time
}

// NewScheduler 构造计划任务调度器。
func NewScheduler(center *Center) *Scheduler {
	return &Scheduler{
		center:              center,
		interval:            defaultReviewRequestScanInterval,
		deferredInterval:    defaultDeferredTaskScanInterval,
		done:                make(chan struct{}),
		pendingShipCooldown: make(map[string]time.Time),
	}
}

// Run 周期扫描计划任务。调用方应在 goroutine 中启动，并用 ctx 控制生命周期。
func (s *Scheduler) Run(ctx context.Context) {
	// nil Context 无法提供调度器停止信号，拒绝启动以免创建无法回收的 goroutine。
	if ctx == nil {
		return
	}
	if s == nil || s.center == nil || s.center.store == nil {
		return
	}
	s.runOnce.Do(func() {
		defer close(s.done)
		if ctx.Err() != nil {
			return
		}
		// generalTicker 驱动分钟级的账号、恢复与求评价扫描。
		generalTicker := time.NewTicker(s.interval)
		defer generalTicker.Stop()
		// deferredTicker 只领取已到期的延迟动作，确保配置的秒数不会额外等待一分钟。
		deferredTicker := time.NewTicker(s.deferredInterval)
		defer deferredTicker.Stop()
		s.scanDeferredTasks(ctx)
		s.scan(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-generalTicker.C:
				s.scan(ctx)
			case <-deferredTicker.C:
				s.scanDeferredTasks(ctx)
			}
		}
	})
}

// Wait 等待调度器完成，并兼容不需要超时的旧调用方。
func (s *Scheduler) Wait() {
	// waitCtx、waitCancel 为兼容入口提供受限等待预算，避免调度器异常时永久阻塞调用方。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), legacySchedulerWaitTimeout)
	defer waitCancel()
	_ = s.WaitContext(waitCtx)
}

// WaitContext 在 ctx 约束内等待调度器完成，避免关闭流程无限阻塞。
func (s *Scheduler) WaitContext(ctx context.Context) error {
	if s != nil && s.done != nil {
		if ctx == nil {
			return errors.New("等待自动化调度器需要关闭 Context")
		}
		select {
		case <-s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// scan 封装scan业务协调。
func (s *Scheduler) scan(ctx context.Context) {
	s.center.scanAccountTasks(ctx)
	if // recovered、err 用于本次流程后续判断的recovered、err
	recovered, err := s.center.store.Automation.RecoverDefinitelyUnsentReviewRuns(ctx); err != nil {
		s.center.logger.Warn("恢复历史求评价未发送任务失败", "err", err)
	} else if recovered > 0 {
		s.center.logger.Info("已恢复历史求评价未发送任务，等待安全重试", "count", recovered)
	}
	// recoveryErr 汇总恢复运行状态收口失败，避免数据库写错误只记录日志后丢失。
	recoveryErr := s.runRecoveryTasks(ctx)
	if recoveryErr != nil {
		// 单独记录恢复任务状态收口错误，延迟任务由秒级扫描函数独立记录。
		s.center.logger.Error("自动化恢复任务状态收口失败", "err", recoveryErr)
	}
	// 逐页执行，避免把所有到期订单一次性装入内存。稳定 ID 游标确保本轮有界。
	afterOrderID := ""
	// waitingForWS 按账号累加本轮因实时连接未就绪而跳过的求评价订单数。
	waitingForWS := map[string]int{}
	for {
		// orders、err 用于本次流程后续判断的orders、err
		orders, err := s.center.store.Automation.DueReviewRequestOrdersAfter(ctx, afterOrderID, 200)
		if err != nil {
			s.center.logger.Warn("扫描求评价计划任务失败", "err", err)
			return
		}
		// order 表示当前遍历过程中的订单
		for _, order := range orders {
			// allowed、allowErr 用于本次流程后续判断的allowed、allowErr
			allowed, allowErr := s.center.accountAutomationAllowed(ctx, order.CookieID)
			if allowErr != nil {
				s.center.logger.Warn("检查求评价账号状态失败", "account", order.CookieID, "err", allowErr)
				continue
			}
			if !allowed {
				continue
			}
			if !s.center.accountSenderReady(order.CookieID) {
				waitingForWS[order.CookieID]++
				continue
			}
			// rules、err 用于本次流程后续判断的rules、err
			rules, err := s.center.rules.match(ctx, Task{AccountID: order.CookieID, ItemID: order.ItemID, TriggerType: TriggerReviewMissingTimeout})
			if err != nil {
				s.center.logger.Warn("查询求评价自动化规则失败", "account", order.CookieID, "order_id", order.OrderID, "item_id", order.ItemID, "err", err)
				continue
			}
			if len(rules) == 0 {
				continue
			}
			// rule 表示当前遍历过程中的规则
			for _, rule := range rules {
				if !reviewRequestRuleDue(order, rule) {
					continue
				}
				// task 用于本次流程后续判断的任务
				task := Task{Source: "scheduler", AccountID: order.CookieID, TriggerType: TriggerReviewMissingTimeout,
					ChatID: order.ChatID, OrderID: order.OrderID, ItemID: order.ItemID, BuyerID: order.BuyerID,
					Text: "发货后一段时间未评价", Raw: map[string]any{"source": "scheduler", "rule_id": rule.ID,
						"order_id": order.OrderID, "attempt": order.ReviewRequestCount + 1}}
				if // err 用于本次流程后续判断的err
				err := s.center.executeRule(ctx, task, rule); err != nil {
					s.center.logger.Warn("求评价计划任务执行失败", "account", order.CookieID, "order_id", order.OrderID, "rule_id", rule.ID, "err", err)
				}
			}
		}
		if len(orders) < 200 {
			break
		}
		afterOrderID = orders[len(orders)-1].OrderID
	}
	// 待发货兜底扫描：付款系统消息丢失时，订单不会有任何运行记录，必须由订单状态补触发。
	s.scanPendingShipDeliveries(ctx)
	// 待发货续跑扫描：运行已经产生但未做完（例如消息动作结果不确定后被隔离），需从检查点继续。
	s.scanPendingShipResumes(ctx)
	// accountID、count 表示当前遍历过程中的账号ID、count
	for accountID, count := range waitingForWS {
		s.center.logger.Info("账号 WebSocket 尚未就绪，求评价任务等待下次扫描", "account", accountID, "orders", count)
	}
}

// scanPendingShipDeliveries 兜底扫描「已付款待发货、但没有任何 order_paid 运行」的订单并补触发自动发货。
//
// 存在意义：付款自动发货只由平台的付款系统消息（WebSocket 推送）触发。一旦该消息丢失、
// 或在准备阶段失败，订单会永久停留在待发货且没有任何重试入口（连运行记录都不会产生，
// 因此失败运行恢复扫描与延迟任务重放都捞不到它）。本扫描按订单状态补一次触发，
// 复用 Center 既有的执行链，不引入第二套执行逻辑。
//
// 幂等性：一旦产生任何 order_paid 运行（无论成功或失败），该订单就不再被本扫描选中，
// 后续交由失败的运行恢复扫描处理；配合内存冷却窗口避免上游故障时的高频重试。
func (s *Scheduler) scanPendingShipDeliveries(ctx context.Context) {
	// 默认开启；显式设为 "0" 时完全跳过，便于上游异常时快速止血。
	if strings.TrimSpace(os.Getenv(pendingShipCatchupEnv)) == "0" {
		return
	}
	if s == nil || s.center == nil || s.center.store == nil || s.center.store.Automation == nil {
		return
	}
	// afterOrderID 是逐页扫描的稳定游标，确保本轮有界。
	afterOrderID := ""
	for {
		// orders、err 用于本次流程后续判断的orders、err
		orders, err := s.center.store.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, afterOrderID, 200)
		if err != nil {
			s.center.logger.Warn("扫描待发货兜底订单失败", "err", err)
			return
		}
		if len(orders) == 0 {
			return
		}
		// order 表示当前遍历过程中的订单
		for _, order := range orders {
			afterOrderID = order.OrderID
			if !s.claimPendingShipAttempt(order.OrderID) {
				continue
			}
			// allowed、allowErr 用于本次流程后续判断的allowed、allowErr
			allowed, allowErr := s.center.accountAutomationAllowed(ctx, order.CookieID)
			if allowErr != nil {
				s.center.logger.Warn("检查待发货兜底账号状态失败", "account", order.CookieID, "order_id", order.OrderID, "err", allowErr)
				continue
			}
			if !allowed {
				continue
			}
			if !s.center.accountSenderReady(order.CookieID) {
				s.center.logger.Info("账号 WebSocket 尚未就绪，待发货兜底任务等待下次扫描", "account", order.CookieID, "order_id", order.OrderID)
				continue
			}
			s.center.logger.Info("付款系统消息缺失，按订单状态补触发自动发货",
				"account", order.CookieID, "order_id", order.OrderID, "item_id", order.ItemID)
			// taskCtx 限制单次执行预算，上游接口挂起时不能让分钟级扫描被永久拖住。
			taskCtx, cancel := context.WithTimeout(ctx, pendingShipTaskTimeout)
			// task 用于本次流程后续判断的任务
			task := Task{Source: "scheduler", AccountID: order.CookieID, TriggerType: TriggerOrderPaid,
				ChatID: order.ChatID, OrderID: order.OrderID, ItemID: order.ItemID, BuyerID: order.BuyerID,
				Text: "付款系统消息缺失，按订单状态补触发自动发货",
				Raw:  map[string]any{"source": "scheduler", "order_id": order.OrderID}}
			if err := s.center.HandleTask(taskCtx, task); err != nil {
				s.center.logger.Warn("待发货兜底任务执行失败",
					"account", order.CookieID, "order_id", order.OrderID, "err", err)
			}
			cancel()
		}
		if len(orders) < 200 {
			return
		}
	}
}

// scanPendingShipResumes 兜底续跑「有运行但剩余动作全部为幂等状态动作」的待发货订单。
//
// 存在意义：运行可能在没有完成全部动作的情况下收口（例如消息动作结果不确定被隔离，
// 或收口过程中进程退出）。这类订单不会被 PendingShipOrdersWithoutPaidRunAfter 选中
// （它已经有一条运行），于是永久停在待发货，且没有任何重试入口。
//
// 安全边界：续跑集合由 PendingShipResumableRunsAfter 限定为「游标已越过全部发卡/模板动作」，
// 因此这里实际执行的只可能是 confirm_shipment 这类不会再次联系买家、且平台侧幂等的动作。
func (s *Scheduler) scanPendingShipResumes(ctx context.Context) {
	// 与补触发共用同一个止血开关：上游异常时可一次性关闭全部待发货兜底行为。
	if strings.TrimSpace(os.Getenv(pendingShipCatchupEnv)) == "0" {
		return
	}
	if s == nil || s.center == nil || s.center.store == nil || s.center.store.Automation == nil {
		return
	}
	// afterOrderID 是逐页扫描的稳定游标，确保本轮有界。
	afterOrderID := ""
	for {
		// candidates、err 保存本页可续跑运行及查询错误。
		candidates, err := s.center.store.Automation.PendingShipResumableRunsAfter(ctx, afterOrderID, pendingShipResumeMaxAttempts, 200)
		if err != nil {
			s.center.logger.Warn("扫描待发货续跑运行失败", "err", err)
			return
		}
		if len(candidates) == 0 {
			return
		}
		// candidate 表示当前遍历过程中的可续跑运行。
		for _, candidate := range candidates {
			afterOrderID = candidate.Order.OrderID
			if !s.claimPendingShipAttempt(candidate.Order.OrderID) {
				continue
			}
			// allowed、allowErr 保存账号可用性检查结果。
			allowed, allowErr := s.center.accountAutomationAllowed(ctx, candidate.Order.CookieID)
			if allowErr != nil {
				s.center.logger.Warn("检查待发货续跑账号状态失败", "account", candidate.Order.CookieID, "order_id", candidate.Order.OrderID, "err", allowErr)
				continue
			}
			if !allowed {
				continue
			}
			// reopened、reopenErr 保存重开运行结果；失败说明状态或代次已变化，放弃本次续跑。
			reopened, reopenErr := s.center.store.Automation.ReopenRunForRecovery(ctx, candidate.RunID, candidate.Attempt, time.Now().UTC().Add(5*time.Minute).Unix())
			if reopenErr != nil || !reopened {
				s.center.logger.Warn("重开未完成待发货运行失败",
					"account", candidate.Order.CookieID, "order_id", candidate.Order.OrderID,
					"run_id", candidate.RunID, "attempt", candidate.Attempt, "reopened", reopened, "err", reopenErr)
				continue
			}
			s.center.logger.Info("待发货运行未完成，按检查点续跑剩余状态动作",
				"account", candidate.Order.CookieID, "order_id", candidate.Order.OrderID,
				"run_id", candidate.RunID, "action_cursor", candidate.ActionCursor, "previous_status", candidate.Status)
			// taskCtx 限制单次执行预算，上游接口挂起时不能让分钟级扫描被永久拖住。
			taskCtx, cancel := context.WithTimeout(ctx, pendingShipTaskTimeout)
			// task 携带运行与规则快照标识，使 Center 走既有恢复路径而非重新匹配规则。
			task := Task{Source: "scheduler", AccountID: candidate.Order.CookieID, TriggerType: TriggerOrderPaid,
				ChatID: candidate.Order.ChatID, OrderID: candidate.Order.OrderID,
				ItemID: candidate.Order.ItemID, BuyerID: candidate.Order.BuyerID,
				Text: "待发货运行未完成，按检查点续跑",
				Raw: map[string]any{"source": "scheduler", "order_id": candidate.Order.OrderID,
					"automation_run_id": candidate.RunID, "automation_rule_id": candidate.RuleID}}
			if err := s.center.HandleTask(taskCtx, task); err != nil {
				s.center.logger.Warn("待发货续跑任务执行失败",
					"account", candidate.Order.CookieID, "order_id", candidate.Order.OrderID, "err", err)
			}
			cancel()
		}
		if len(candidates) < 200 {
			return
		}
	}
}

// claimPendingShipAttempt 领取一次兜底尝试；处于冷却窗口内的订单返回 false。
func (s *Scheduler) claimPendingShipAttempt(orderID string) bool {
	if s == nil {
		return false
	}
	s.pendingShipMu.Lock()
	defer s.pendingShipMu.Unlock()
	// now 用于本次流程后续判断的now
	now := time.Now()
	if last, seen := s.pendingShipCooldown[orderID]; seen && now.Sub(last) < defaultPendingShipCooldown {
		return false
	}
	s.pendingShipCooldown[orderID] = now
	// 长期运行下顺手清理过期条目，避免映射无界增长。
	if len(s.pendingShipCooldown) > 4096 {
		// key、ts 表示当前遍历过程中的key、ts
		for key, ts := range s.pendingShipCooldown {
			if now.Sub(ts) >= defaultPendingShipCooldown {
				delete(s.pendingShipCooldown, key)
			}
		}
	}
	return true
}

// scanDeferredTasks 领取并重放已到期延迟动作；错误独立记录，避免影响分钟级扫描的调度节奏。
func (s *Scheduler) scanDeferredTasks(ctx context.Context) {
	// deferredErr 保存本轮延迟动作状态收口错误，必须记录以便管理员追踪人工核对任务。
	deferredErr := s.runDeferredTasks(ctx)
	if deferredErr != nil {
		s.center.logger.Error("自动化延迟任务状态收口失败", "err", deferredErr)
	}
}

// runRecoveryTasks 封装运行Recovery任务列表业务协调。
func (s *Scheduler) runRecoveryTasks(ctx context.Context) error {
	// resultErr 汇总本轮恢复任务的持久化错误，调用方可据此触发统一告警。
	var resultErr error
	// runs、err 用于本次流程后续判断的runs、err
	runs, err := s.center.store.Automation.DueRecoveryRuns(ctx, 100)
	if err != nil {
		s.center.logger.Warn("扫描失败自动化运行失败", "err", err)
		return err
	}
	// run 表示当前遍历过程中的运行
	for _, run := range runs {
		if run.ActionStarted {
			// reason 用于本次流程后续判断的原因
			reason := "进程在外部动作执行期间中断，发送结果未知，已禁止自动重放"
			// quarantineErr 表示把外部动作结果未知的运行转为人工核对状态时的错误。
			quarantineErr := s.quarantineRunForReview(ctx, run, reason)
			resultErr = errors.Join(resultErr, quarantineErr)
			continue
		}
		// task 用于本次流程后续判断的任务
		var task Task
		if // err 用于本次流程后续判断的err
		err := json.Unmarshal([]byte(run.RawEventJSON), &task); err != nil || task.AccountID == "" {
			// reason 用于本次流程后续判断的原因
			reason := "历史运行数据无法安全解析，已移入人工检查"
			// quarantineErr 表示历史任务无法解析时写入人工核对状态的错误。
			quarantineErr := s.quarantineRunForReview(ctx, run, reason)
			resultErr = errors.Join(resultErr, quarantineErr)
			continue
		}
		// allowed、err 用于本次流程后续判断的allowed、err
		allowed, err := s.center.accountAutomationAllowed(ctx, task.AccountID)
		if err != nil || !allowed {
			if // postponeErr 用于本次流程后续判断的postponeErr
			postponeErr := s.center.store.Automation.PostponeRecoveryRun(ctx, run.ID, run.AttemptCount, time.Now().UTC().Add(10*time.Minute).Unix()); postponeErr != nil {
				s.center.logger.Warn("延期自动化恢复任务失败", "run_id", run.ID, "err", postponeErr)
				resultErr = errors.Join(resultErr, fmt.Errorf("延期自动化恢复任务失败: %w", postponeErr))
			}
			continue
		}
		// rule、err 用于本次流程后续判断的rule、err
		rule, err := s.center.store.Automation.Get(ctx, run.RuleID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			if ctx.Err() != nil {
				return errors.Join(resultErr, ctx.Err())
			}
			s.center.logger.Warn("读取自动化恢复规则失败，保留任务等待重试", "run_id", run.ID, "rule_id", run.RuleID, "err", err)
			continue
		}
		if errors.Is(err, db.ErrNotFound) || rule == nil || !rule.Enabled {
			// reason 用于本次流程后续判断的原因
			reason := "自动化规则不存在或已停用，无法恢复"
			// quarantineErr 表示规则不可恢复时写入人工核对状态的错误。
			quarantineErr := s.quarantineRunForReview(ctx, run, reason)
			resultErr = errors.Join(resultErr, quarantineErr)
			continue
		}
		if recoveryNeedsSender(task, *rule, run.ActionCursor) && !s.center.accountSenderReady(task.AccountID) {
			if // postponeErr 用于本次流程后续判断的postponeErr
			postponeErr := s.center.store.Automation.PostponeRecoveryRun(ctx, run.ID, run.AttemptCount, time.Now().UTC().Add(defaultReviewRequestScanInterval).Unix()); postponeErr != nil {
				s.center.logger.Warn("等待 WebSocket 时延期自动化任务失败", "run_id", run.ID, "err", postponeErr)
				resultErr = errors.Join(resultErr, fmt.Errorf("等待 WebSocket 时延期自动化任务失败: %w", postponeErr))
			}
			continue
		}
		// claimed、claimErr 用于本次流程后续判断的claimed、claimErr
		claimed, claimErr := s.center.store.Automation.ClaimRecoveryRun(ctx, run.ID, time.Now().UTC().Add(5*time.Minute).Unix())
		if claimErr != nil {
			// claimFailure 表示领取恢复运行的状态写入失败，必须返回而不能被当作并发未领取。
			claimFailure := fmt.Errorf("领取自动化恢复任务失败: %w", claimErr)
			resultErr = errors.Join(resultErr, claimFailure)
		}
		if claimErr != nil || !claimed {
			continue
		}
		if task.Raw == nil {
			task.Raw = map[string]any{}
		}
		task.Raw["automation_run_id"] = run.ID
		task.Raw["automation_rule_id"] = run.RuleID
		if // err 用于本次流程后续判断的err
		err := s.center.executeRule(ctx, task, *rule); err != nil && !errors.Is(err, errAutomationDeferred) {
			s.center.logger.Warn("重试自动化运行失败", "run_id", run.ID, "err", err)
			resultErr = errors.Join(resultErr, err)
		}
	}
	return resultErr
}

// quarantineRunForReview 将恢复运行置为人工核对并发送运维通知；写入失败时返回统一 needs_review 错误，禁止调用方误认为状态已收口。
func (s *Scheduler) quarantineRunForReview(ctx context.Context, run db.AutomationRun, reason string) error {
	// quarantineErr 表示人工核对状态写入失败；失败时数据库中的原状态仍可能允许下一轮恢复。
	quarantineErr := s.center.store.Automation.QuarantineRun(ctx, run.ID, run.AttemptCount, reason)
	s.center.notifyRunNeedsReview(ctx, run, reason)
	if quarantineErr == nil {
		return nil
	}
	s.center.logger.Error("保存自动化恢复运行人工核对状态失败", "run_id", run.ID, "err", quarantineErr)
	return errors.Join(
		errAutomationNeedsReview,
		errAutomationQuarantine,
		fmt.Errorf("保存自动化恢复运行人工核对状态失败: %w", quarantineErr),
	)
}

// recoveryNeedsSender 封装recoveryNeedsSender业务协调。
func recoveryNeedsSender(task Task, rule db.AutomationRule, cursor int) bool {
	// actions 用于本次流程后续判断的动作列表
	actions := task.ActionPlan
	if len(actions) == 0 {
		actions = (actionPlanner{}).plan(task, rule.Actions)
	}
	if cursor < 0 || cursor >= len(actions) {
		return false
	}
	switch actions[cursor].ActionType {
	case ActionSendText, ActionSendCard, ActionSendTemplate:
		return true
	default:
		return false
	}
}

// runDeferredTasks 封装运行Deferred任务列表业务协调。
func (s *Scheduler) runDeferredTasks(ctx context.Context) error {
	// resultErr 汇总延迟任务最终状态写入失败，避免领取成功后状态异常被静默吞掉。
	var resultErr error
	// tasks、err 用于本次流程后续判断的tasks、err
	tasks, err := s.center.store.Automation.ClaimDueDeferredTasks(ctx, 100)
	if err != nil {
		s.center.logger.Warn("扫描暂停期间自动化事件失败", "err", err)
		return err
	}
	// pending 表示当前遍历过程中的pending
	for _, pending := range tasks {
		// task 用于本次流程后续判断的任务
		var task Task
		if // err 用于本次流程后续判断的err
		err := json.Unmarshal([]byte(pending.TaskJSON), &task); err != nil {
			// finishErr 表示解析失败后写入延迟任务重试或死信状态时的错误。
			finishErr := s.center.store.Automation.FinishDeferredTask(ctx, pending.ID, pending.ClaimVersion, false, "解析任务失败: "+err.Error())
			if finishErr != nil {
				s.center.logger.Error("保存解析失败的暂停事件状态失败", "task_id", pending.ID, "err", finishErr)
				resultErr = errors.Join(
					resultErr,
					errAutomationNeedsReview,
					fmt.Errorf("保存解析失败的暂停事件状态失败: %w", finishErr),
				)
			}
			continue
		}
		if task.Raw == nil {
			task.Raw = map[string]any{}
		}
		task.Raw["automation_deferred_replay"] = true
		// deferredAgain、runErr 用于本次流程后续判断的deferredAgain、runErr
		deferredAgain, runErr := s.center.handleTask(ctx, task)
		if deferredAgain {
			// handleTask 已按新的 paused_until 重置同一任务；当前 claim 不再删除。
			continue
		}
		if // err 用于本次流程后续判断的err
		err := s.center.store.Automation.FinishDeferredTask(ctx, pending.ID, pending.ClaimVersion, runErr == nil, errorString(runErr)); err != nil {
			s.center.logger.Warn("保存暂停事件重放结果失败", "task_id", pending.ID, "err", err)
			resultErr = errors.Join(resultErr, errAutomationNeedsReview, runErr, fmt.Errorf("保存暂停事件重放结果失败: %w", err))
		}
	}
	return resultErr
}

// errorString 封装错误String业务协调。
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// reviewRequestRuleDue 封装review请求规则Due业务协调。
func reviewRequestRuleDue(order db.Order, rule db.AutomationRule) bool {
	// cfg 用于本次流程后续判断的cfg
	cfg := parseReviewRuleConfig(rule.ConfigJSON)
	if cfg.MaxAttempts > 0 && order.ReviewRequestCount >= cfg.MaxAttempts {
		return false
	}
	// baseRaw 用于本次流程后续判断的base原始
	baseRaw := firstNonEmpty(order.ShippedAt, order.UpdatedAt, order.CreatedAt)
	// waitHours 用于本次流程后续判断的waitHours
	waitHours := cfg.AfterShippedHours
	if order.ReviewRequestCount > 0 && strings.TrimSpace(order.LastReviewRequestAt) != "" {
		baseRaw = order.LastReviewRequestAt
		waitHours = cfg.RepeatIntervalHours
	}
	// base 用于本次流程后续判断的base
	base := parseDBTime(baseRaw)
	if base.IsZero() {
		return false
	}
	return time.Since(base) >= time.Duration(waitHours)*time.Hour
}

// reviewRuleConfig 用于本次流程后续判断的review规则配置
type reviewRuleConfig struct {
	AfterShippedHours   int
	RepeatIntervalHours int
	MaxAttempts         int
}

// parseReviewRuleConfig 封装parseReview规则配置业务协调。
func parseReviewRuleConfig(raw string) reviewRuleConfig {
	// cfg 用于本次流程后续判断的cfg
	cfg := reviewRuleConfig{AfterShippedHours: 72, RepeatIntervalHours: 24, MaxAttempts: 1}
	if strings.TrimSpace(raw) == "" {
		return cfg
	}
	// m 用于本次流程后续判断的m
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return cfg
	}
	if // v 用于本次流程后续判断的v
	v := intFromAny(m["after_shipped_hours"]); v > 0 {
		cfg.AfterShippedHours = v
	}
	if // v 用于本次流程后续判断的v
	v := intFromAny(m["first_delay_hours"]); v > 0 {
		cfg.AfterShippedHours = v
	}
	if // v 用于本次流程后续判断的v
	v := intFromAny(m["repeat_interval_hours"]); v > 0 {
		cfg.RepeatIntervalHours = v
	}
	if // v 用于本次流程后续判断的v
	v := intFromAny(m["max_attempts"]); v > 0 {
		cfg.MaxAttempts = v
	}
	return cfg
}

// intFromAny 封装intFromAny业务协调。
func intFromAny(v any) int {
	switch // x 用于本次流程后续判断的x
	x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		// n 用于本次流程后续判断的n
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

// parseDBTime 封装parseDB时间业务协调。
func parseDBTime(s string) time.Time {
	// layout 表示当前遍历过程中的layout
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00", // Postgres TEXT(CURRENT_TIMESTAMP)
		"2006-01-02 15:04:05.999999999Z07",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05Z07",
		"2006-01-02 15:04:05", // SQLite/MySQL 历史值；按既有 UTC 约定解释
	} {
		if // t、err 用于本次流程后续判断的t、err
		t, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}
