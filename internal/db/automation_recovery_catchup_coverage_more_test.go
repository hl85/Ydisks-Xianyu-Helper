package db

import (
	"context"
	"testing"
	"time"
)

// seedCatchupOrder 写入一条可被待发货兜底扫描选中的订单夹具。
// 订单必须处于 pending_ship、未删除且带会话标识，才会进入兜底扫描的候选集合。
func seedCatchupOrder(t *testing.T, s *Store, orderID, cookieID, itemID, chatID, orderStatus string) {
	t.Helper()
	// upsertErr 保存订单夹具写入错误。
	upsertErr := s.Orders.Upsert(context.Background(), orderID, OrderUpsertOpts{
		ItemID: itemID, BuyerID: "buyer-" + orderID, CookieID: cookieID, ChatID: chatID,
		OrderStatus: orderStatus, Quantity: "1", Amount: "9.90", SpecName: "颜色", SpecValue: "黑",
	})
	if upsertErr != nil {
		t.Fatalf("写入订单夹具失败: %v", upsertErr)
	}
}

// seedCatchupRule 写入一条 order_paid 自动化规则并返回规则主键。
// 规则必须绑定商品：item_id 为空的规则会被当成通配规则命中所有订单，会破坏过滤矩阵断言。
func seedCatchupRule(t *testing.T, s *Store, userID int64, cookieID, itemID, name string, enabled bool, actions []AutomationActionInput) int64 {
	t.Helper()
	// ruleID、ruleErr 保存规则写入结果与错误。
	ruleID, ruleErr := s.Automation.Create(context.Background(), AutomationRuleInput{
		UserID: userID, CookieID: cookieID, Name: name, ItemID: itemID,
		TriggerType: "order_paid", Enabled: enabled, Actions: actions,
	})
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	return ruleID
}

// seedCatchupRun 为订单写入一条 order_paid 运行记录，用于验证兜底扫描的排除边界。
// 也可以把 status/attempt_count/action_cursor 改写为待续跑形态，供续跑扫描测试复用。
func seedCatchupRun(t *testing.T, s *Store, ruleID int64, cookieID, orderID string) int64 {
	t.Helper()
	// runID、started、startErr 保存运行写入结果与错误。
	runID, started, startErr := s.Automation.TryStartRun(context.Background(), AutomationRun{
		RuleID: ruleID, CookieID: cookieID, OrderID: orderID, TriggerType: "order_paid",
		TriggerKey: "catchup:" + orderID, RawEventJSON: "{}", LeaseExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if startErr != nil || !started {
		t.Fatalf("写入运行夹具失败: started=%v err=%v", started, startErr)
	}
	return runID
}

// TestPendingShipOrdersWithoutPaidRunAfterReturnsUntouchedOrders 验证兜底扫描能选中完全没有运行的待发货订单。
// 这是 P2 修复的核心：付款系统消息丢失时订单不会留下任何运行记录，只能靠订单状态补触发。
func TestPendingShipOrdersWithoutPaidRunAfterReturnsUntouchedOrders(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// userID、cookieID 保存测试账号主键与账号标识。
	userID, cookieID := seedAccount(t, s)
	seedCatchupRule(t, s, userID, cookieID, "item-1", "发货规则", true,
		[]AutomationActionInput{{ActionType: "send_card", MessageTemplate: "card", Enabled: true}})
	seedCatchupOrder(t, s, "o-100", cookieID, "item-1", "chat-100", "pending_ship")	// candidates 保存本轮兜底扫描结果。
	candidates, err := s.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("应选中 1 条待发货订单: %d", len(candidates))
	}
	// got 保存选中的订单事实，用于校验扫描列完整回填。
	got := candidates[0]
	if got.OrderID != "o-100" || got.ItemID != "item-1" || got.ChatID != "chat-100" || got.OrderStatus != "pending_ship" || got.CookieID != cookieID {
		t.Fatalf("订单事实回填异常: %+v", got)
	}
	if got.Quantity != "1" || got.Amount != "9.90" || got.SpecName != "颜色" || got.SpecValue != "黑" {
		t.Fatalf("订单展示字段回填异常: %+v", got)
	}
	// afterPage 保存越过游标后的第二页结果，稳定游标必须排除已见订单。
	afterPage, err := s.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "o-100", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPage) != 0 {
		t.Fatalf("游标之后不应再有候选: %d", len(afterPage))
	}
}

// TestPendingShipOrdersWithoutPaidRunAfterFilters 验证兜底扫描的全部排除边界。
// 每一条排除规则都对应一次真实线上风险：重复发货、打给无会话订单或对停用规则空转。
func TestPendingShipOrdersWithoutPaidRunAfterFilters(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// userID、cookieID 保存测试账号主键与账号标识。
	userID, cookieID := seedAccount(t, s)
	// activeRuleID 保存与 item-1 匹配且启用的发货规则主键。
	activeRuleID := seedCatchupRule(t, s, userID, cookieID, "item-1", "启用规则", true,
		[]AutomationActionInput{{ActionType: "send_card", MessageTemplate: "card", Enabled: true}})
	// disabledRuleID 保存已停用规则主键，用于验证停用规则不再触发补发。
	seedCatchupRule(t, s, userID, cookieID, "item-2", "停用规则", false,
		[]AutomationActionInput{{ActionType: "send_card", MessageTemplate: "card", Enabled: true}})
	// mismatchRuleID 保存商品不匹配但启用的发货规则主键。
	seedCatchupRule(t, s, userID, cookieID, "item-other", "不匹配规则", true,
		[]AutomationActionInput{{ActionType: "send_card", MessageTemplate: "card", Enabled: true}})
	// 无运行记录且启用规则匹配 → 必须被选中。
	seedCatchupOrder(t, s, "o-hit", cookieID, "item-1", "chat-hit", "pending_ship")
	// 订单状态不是待发货 → 平台侧已发货或已完成，不能再补发。
	seedCatchupOrder(t, s, "o-shipped", cookieID, "item-1", "chat-shipped", "shipped")
	// 缺少会话标识 → 没有会话就无法向买家发起发送。
	seedCatchupOrder(t, s, "o-nochat", cookieID, "item-1", "", "pending_ship")
	// 商品没有任何匹配的启用规则 → 没有可执行动作，不能空触发。
	seedCatchupOrder(t, s, "o-mismatch", cookieID, "item-3", "chat-mismatch", "pending_ship")
	// 已有 order_paid 运行 → 交由失败运行恢复链处理，不能重复触发。
	seedCatchupOrder(t, s, "o-hasrun", cookieID, "item-1", "chat-hasrun", "pending_ship")
	seedCatchupRun(t, s, activeRuleID, cookieID, "o-hasrun")
	// 候选、排除三类夹具共同构成完整的过滤矩阵。
	candidates, err := s.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].OrderID != "o-hit" {
		t.Fatalf("兜底扫描过滤矩阵异常: %+v", candidates)
	}
	// activeRuleID 仅用于避免未使用变量告警；规则主键本身不参与查询断言。
	_ = activeRuleID
}

// TestPendingShipScansFallBackToDefaultBounds 验证两个兜底扫描在非法入参下回落到默认分页与代次上限。
// 调用方传入 0 时不得把 LIMIT 设为 0 或放开代次上限，否则会造成漏扫或无限重开。
func TestPendingShipScansFallBackToDefaultBounds(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// userID、cookieID 保存测试账号主键与账号标识。
	userID, cookieID := seedAccount(t, s)
	// ruleID 保存仅含确认发货动作的规则主键。
	ruleID := seedCatchupRule(t, s, userID, cookieID, "item-1", "默认边界规则", true,
		[]AutomationActionInput{{ActionType: "confirm_shipment", Enabled: true}})
	seedCatchupOrder(t, s, "o-bounds", cookieID, "item-1", "chat-bounds", "pending_ship")
	// 无运行记录的订单用于验证兜底扫描的默认分页回落，有运行的订单不进入该候选集合。
	seedCatchupOrder(t, s, "o-bounds-free", cookieID, "item-1", "chat-bounds-free", "pending_ship")
	// runID 保存待续跑运行主键。
	runID := seedCatchupRun(t, s, ruleID, cookieID, "o-bounds")
	// 把运行置为可续跑形态，使两个扫描都能命中同一条夹具。
	if _, updateErr := s.DB.ExecContext(ctx,
		`UPDATE automation_runs SET status='needs_review', attempt_count=1, action_cursor=0, next_retry_at=0, lease_expires_at=0 WHERE id=?`, runID); updateErr != nil {
		t.Fatal(updateErr)
	}
	// 非法 limit 与代次上限必须回落到默认值而不是清空结果。
	// orders 是 limit=0 时兜底扫描的结果，应命中没有运行记录的 o-bounds-free。
	orders, err := s.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 || orders[0].OrderID != "o-bounds-free" {
		t.Fatalf("limit=0 应回落默认分页并保留候选: %+v", orders)
	}
	// resumes 是 limit 与代次上限都为 0 时的续跑扫描结果。
	resumes, err := s.Automation.PendingShipResumableRunsAfter(ctx, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 1 {
		t.Fatalf("limit=0 与代次上限=0 应回落默认值并保留候选: %d", len(resumes))
	}
}

// TestPendingShipScansReportStorageFailures 验证存储不可用时两个兜底扫描都返回错误。
// 兜底扫描漏报比多报更危险：错误必须上抛给调度器记录，而不是当成“没有候选”。
func TestPendingShipScansReportStorageFailures(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// closeErr 保存数据库关闭错误；关闭后存储层必须以错误返回。
	if closeErr := s.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// _, ordersErr 是存储关闭后的兜底扫描结果。
	if _, ordersErr := s.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 200); ordersErr == nil {
		t.Fatal("存储不可用时兜底扫描应返回错误")
	}
	// _, resumesErr 是存储关闭后的续跑扫描结果。
	if _, resumesErr := s.Automation.PendingShipResumableRunsAfter(ctx, "", 5, 200); resumesErr == nil {
		t.Fatal("存储不可用时续跑扫描应返回错误")
	}
	// _, reopenErr 是存储关闭后的重开运行结果。
	if _, reopenErr := s.Automation.ReopenRunForRecovery(ctx, 1, 1, 0); reopenErr == nil {
		t.Fatal("存储不可用时重开运行应返回错误")
	}
}

// TestPendingShipResumableRunsAfterOnlySelectsIdempotentTail 验证续跑扫描只放行幂等收尾动作。
// 续跑的安全边界是游标必须已越过全部发卡/模板动作，否则继续执行就可能重复联系买家。
func TestPendingShipResumableRunsAfterOnlySelectsIdempotentTail(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// userID、cookieID 保存测试账号主键与账号标识。
	userID, cookieID := seedAccount(t, s)
	// fullRuleID 保存包含发卡与确认发货两类动作的规则主键。
	fullRuleID := seedCatchupRule(t, s, userID, cookieID, "item-1", "发货续跑规则", true,
		[]AutomationActionInput{{ActionType: "send_card", MessageTemplate: "card", Enabled: true}, {ActionType: "confirm_shipment", Enabled: true}})
	// 符合续跑条件：needs_review、代次 1、游标 1（已越过发卡动作）。
	seedCatchupOrder(t, s, "o-resume", cookieID, "item-1", "chat-resume", "pending_ship")
	// resumableRunID 保存符合续跑条件的运行主键。
	resumableRunID := seedCatchupRun(t, s, fullRuleID, cookieID, "o-resume")
	// 卡在发卡动作：游标 0 仍可能再次联系买家，必须排除。
	seedCatchupOrder(t, s, "o-cardpending", cookieID, "item-1", "chat-cardpending", "pending_ship")
	// cardPendingRunID 保存游标仍停在发卡动作的运行主键。
	cardPendingRunID := seedCatchupRun(t, s, fullRuleID, cookieID, "o-cardpending")
	// 已在执行中的运行不属于待恢复状态，必须排除。
	seedCatchupOrder(t, s, "o-running", cookieID, "item-1", "chat-running", "pending_ship")
	// runningRunID 保存状态为 running 的运行主键。
	runningRunID := seedCatchupRun(t, s, fullRuleID, cookieID, "o-running")
	// 代次已达上限的运行说明上游持续异常，继续自动重开会放大压力，必须排除。
	seedCatchupOrder(t, s, "o-exhausted", cookieID, "item-1", "chat-exhausted", "pending_ship")
	// exhaustedRunID 保存代次超限的运行主键。
	exhaustedRunID := seedCatchupRun(t, s, fullRuleID, cookieID, "o-exhausted")
	// 只包含确认发货动作的规则没有消息动作，游标 0 也应可续跑。
	shipOnlyRuleID := seedCatchupRule(t, s, userID, cookieID, "item-2", "仅确认发货规则", true,
		[]AutomationActionInput{{ActionType: "confirm_shipment", Enabled: true}})
	seedCatchupOrder(t, s, "o-shiponly", cookieID, "item-2", "chat-shiponly", "pending_ship")
	// shipOnlyRunID 保存仅含幂等动作规则的运行主键。
	shipOnlyRunID := seedCatchupRun(t, s, shipOnlyRuleID, cookieID, "o-shiponly")
	// updateRun 是夹具改造函数：把运行改成指定的状态、代次与游标。
	updateRun := func(runID int64, status string, attempt, cursor int) {
		// updateErr 保存运行状态改写错误。
		if _, updateErr := s.DB.ExecContext(context.Background(),
			`UPDATE automation_runs SET status=?, attempt_count=?, action_cursor=?, action_started=0, next_retry_at=0, lease_expires_at=0 WHERE id=?`,
			status, attempt, cursor, runID); updateErr != nil {
			t.Fatalf("改写运行夹具失败: %v", updateErr)
		}
	}	// 符合条件与三类排除条件分别落库。
	updateRun(resumableRunID, "needs_review", 1, 1)
	updateRun(cardPendingRunID, "needs_review", 1, 0)
	updateRun(runningRunID, "running", 1, 1)
	updateRun(exhaustedRunID, "needs_review", 5, 1)
	updateRun(shipOnlyRunID, "failed", 1, 0)
	// candidates 保存续跑扫描结果，应只包含两个幂等收尾候选。
	candidates, err := s.Automation.PendingShipResumableRunsAfter(ctx, "", 5, 200)
	if err != nil {
		t.Fatal(err)
	}
	// gotIDs 收集候选订单，便于断言排除矩阵。
	gotIDs := map[string]PendingShipResume{}
	for _, candidate := range candidates {
		gotIDs[candidate.Order.OrderID] = candidate
	}
	if len(candidates) != 2 {
		t.Fatalf("应只选中 2 条可续跑运行: %+v", candidates)
	}
	if gotIDs["o-resume"].RunID != resumableRunID || gotIDs["o-resume"].Status != "needs_review" || gotIDs["o-resume"].ActionCursor != 1 {
		t.Fatalf("needs_review 候选回填异常: %+v", gotIDs["o-resume"])
	}
	if gotIDs["o-shiponly"].RunID != shipOnlyRunID || gotIDs["o-shiponly"].Attempt != 1 {
		t.Fatalf("仅确认发货候选回填异常: %+v", gotIDs["o-shiponly"])
	}
	// 稳定游标必须跳过已见订单。
	// afterPage 保存越过第一个候选后的第二页结果。
	afterPage, err := s.Automation.PendingShipResumableRunsAfter(ctx, "o-resume", 5, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPage) != 1 || afterPage[0].Order.OrderID != "o-shiponly" {
		t.Fatalf("续跑扫描游标分页异常: %+v", afterPage)
	}
}

// TestReopenRunForRecoveryRequiresUnchangedAttempt 验证重开运行必须以原代次抢占成功。
// 代次变化说明另一个 worker 已经动过这条运行，抢占失败必须让调用方放弃，避免重复发货。
func TestReopenRunForRecoveryRequiresUnchangedAttempt(t *testing.T) {
	// s、cleanup 保存测试数据库及关闭责任。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// userID、cookieID 保存测试账号主键与账号标识。
	userID, cookieID := seedAccount(t, s)
	// ruleID 保存发货规则主键。
	ruleID := seedCatchupRule(t, s, userID, cookieID, "item-1", "重开规则", true,
		[]AutomationActionInput{{ActionType: "confirm_shipment", Enabled: true}})
	// runID 保存待重开的运行主键。
	runID := seedCatchupRun(t, s, ruleID, cookieID, "o-reopen")
	// statusErr 保存把运行置为人工核对状态的改写错误。
	if _, statusErr := s.DB.ExecContext(ctx, `UPDATE automation_runs SET status='needs_review', attempt_count=1, action_cursor=1 WHERE id=?`, runID); statusErr != nil {
		t.Fatal(statusErr)
	}
	// leaseAt 是本次恢复分配的租约到期时间。
	leaseAt := time.Now().UTC().Add(5 * time.Minute).Unix()
	// reopened、reopenErr 保存以原代次抢占的结果。
	reopened, reopenErr := s.Automation.ReopenRunForRecovery(ctx, runID, 1, leaseAt)
	if reopenErr != nil || !reopened {
		t.Fatalf("原代次抢占应成功: reopened=%v err=%v", reopened, reopenErr)
	}
	// run 保存重开后的运行记录，用于校验检查点已被重置。
	run, getErr := s.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != "running" || run.ActionStarted || run.AttemptCount != 2 || run.ErrorMessage != "" {
		t.Fatalf("重开后的运行状态异常: %+v", run)
	}
	if run.LeaseExpiresAt != leaseAt {
		t.Fatalf("重开后租约应更新为本次恢复的到期时间: %d", run.LeaseExpiresAt)
	}
	// 代次已变化，再次以旧代次抢占必须失败。
	staleReopened, staleErr := s.Automation.ReopenRunForRecovery(ctx, runID, 1, leaseAt)
	if staleErr != nil || staleReopened {
		t.Fatalf("旧代次抢占必须失败: reopened=%v err=%v", staleReopened, staleErr)
	}
	// 状态已变为 running，新代次抢占同样必须失败。
	raceReopened, raceErr := s.Automation.ReopenRunForRecovery(ctx, runID, 2, leaseAt)
	if raceErr != nil || raceReopened {
		t.Fatalf("running 状态不允许重开: reopened=%v err=%v", raceReopened, raceErr)
	}
}
