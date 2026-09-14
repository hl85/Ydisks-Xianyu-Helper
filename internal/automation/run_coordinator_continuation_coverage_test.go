package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// continuationNotifyRecorder 记录运行结果通知的状态序列，供测试断言人工核对通知确实发出。
type continuationNotifyRecorder struct {
	// calls 保存每次通知的状态字符串。
	calls []string
}

// record 按通知签名记录一次运行结果通知。
func (r *continuationNotifyRecorder) record(_ context.Context, _ Task, _ int64, status string, _ int, _ string) {
	r.calls = append(r.calls, status)
}

// newContinuationCoordinator 构造一个注入全部依赖的运行协调器，供直连测试运行编排分支。
// 依赖全部用测试替身，隔离订单补全、账号门禁与外部动作，只保留真实存储检查点链路。
func newContinuationCoordinator(store *db.Store, executeAction func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error), notifier *continuationNotifyRecorder) automationRunCoordinator {
	return automationRunCoordinator{
		store:   store,
		planner: actionPlanner{},
		logger:  slog.Default(),
		prepareTask: func(_ context.Context, task Task) (Task, error) {
			return task, nil
		},
		actionDelaySeconds: func(context.Context, db.AutomationAction) (int, error) {
			return 0, nil
		},
		accountAutomationAllowed: func(context.Context, string) (bool, error) {
			return true, nil
		},
		accountSenderReady: func(string) bool {
			return true
		},
		deferTask: func(context.Context, Task, int64) error {
			return nil
		},
		executeAction: executeAction,
		hasNotifier: func() bool {
			return notifier != nil
		},
		notifyResult: notifier.record,
	}
}

// continuationRunState 把运行改写成指定状态、代次与游标，并保留有效租约。
// 租约必须有效，否则动作检查点的归属锁会拒绝领取。
func continuationRunState(t *testing.T, store *db.Store, runID int64, status string, attempt, cursor int) {
	t.Helper()
	// updateErr 保存运行状态改写错误；租约延长到十分钟后保证检查点可领取。
	if _, updateErr := store.DB.ExecContext(context.Background(),
		`UPDATE automation_runs SET status=?, attempt_count=?, action_cursor=?, action_started=0, next_retry_at=0, lease_expires_at=? WHERE id=?`,
		status, attempt, cursor, time.Now().Add(10*time.Minute).Unix(), runID); updateErr != nil {
		t.Fatalf("改写运行夹具失败: %v", updateErr)
	}
}

// continuationRule 写入账号与一条绑定发卡/确认发货的 order_paid 规则并返回规则主键。
// 运行记录通过外键引用账号，因此规则夹具必须同时创建账号。
// 动作规格与夹具任务的「颜色/黑」一致，保证动作计划能通过订单规格匹配闸。
func continuationRule(t *testing.T, store *db.Store, userID int64, cookieID string, actions []db.AutomationActionInput) int64 {
	t.Helper()
	catchupAccount(t, store, userID, cookieID)
	// ruleID、ruleErr 保存规则写入结果与错误。
	ruleID, ruleErr := store.Automation.Create(context.Background(), db.AutomationRuleInput{
		UserID: userID, CookieID: cookieID, Name: "continuation-" + cookieID, ItemID: "item-continuation",
		TriggerType: TriggerOrderPaid, Enabled: true, Actions: actions,
	})
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	return ruleID
}

// continuationRun 通过真实存储创建一条 order_paid 运行并返回运行主键。
func continuationRun(t *testing.T, store *db.Store, ruleID int64, cookieID, orderID string) int64 {
	t.Helper()
	// runID、started、startErr 保存运行写入结果与错误。
	runID, started, startErr := store.Automation.TryStartRun(context.Background(), db.AutomationRun{
		RuleID: ruleID, CookieID: cookieID, OrderID: orderID, ItemID: "item-continuation",
		TriggerType: TriggerOrderPaid, TriggerKey: "continuation:" + orderID,
		RawEventJSON: "{}", LeaseExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	if startErr != nil || !started {
		t.Fatalf("写入运行夹具失败: started=%v err=%v", started, startErr)
	}
	return runID
}

// TestExecuteRunActionsContinuesIdempotentTailAfterUncertainMessage 验证 P1 修复主链路：
// 消息动作结果不确定但剩余动作全是幂等状态动作时，运行推进游标继续执行，最后仍收口为人工核对。
// 这是 09-13「卡密已发出但发货状态未更新」问题的核心修复语义，必须逐分支锁死。
func TestExecuteRunActionsContinuesIdempotentTailAfterUncertainMessage(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含发卡与确认发货两类动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "continuation-acc",
		[]db.AutomationActionInput{
			{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionConfirmShipment, Enabled: true},
		})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "continuation-acc", "o-continuation")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态，游标与代次必须与夹具一致。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// executed 收集实际执行过的动作类型，用于证明确认发货确实被放行执行。
	executed := []string{}
	// coordinator 是注入不确定动作的运行协调器。
	coordinator := newContinuationCoordinator(store, func(_ context.Context, _ Task, action db.AutomationAction, _ shipmentDeliveryProof) (actionExecutionResult, error) {
		executed = append(executed, action.ActionType)
		if action.ActionType != ActionSendCard {
			return actionExecutionResult{}, nil
		}
		// 卡密内容可能已经发出但平台响应中断：这是结果不确定而非确定未发送。
		return actionExecutionResult{reviewProof: shipmentDeliveryProof{messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "卡密内容"}}}},
			uncertainAction(errors.New("平台发送响应中断"))
	}, nil)
	// task 是与动作规格匹配的付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "continuation-acc", OrderID: "o-continuation",
		ChatID: "chat-continuation", BuyerID: "buyer-continuation", ItemID: "item-continuation",
		SpecName: "颜色", SpecValue: "黑"}
	// actions 是运行要执行的动作计划。
	actions := []db.AutomationAction{
		{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		{ActionType: ActionConfirmShipment, Enabled: true},
	}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if deferred {
		t.Fatal("结果不确定时不得进入延迟队列")
	}
	if !errors.Is(runErr, errAutomationNeedsReview) {
		t.Fatalf("放行后仍必须收口为人工核对: %v", runErr)
	}
	if sent != 0 {
		t.Fatalf("不确定动作不得计入已确认数量: %d", sent)
	}
	if len(executed) != 2 || executed[0] != ActionSendCard || executed[1] != ActionConfirmShipment {
		t.Fatalf("确认发货未被放行执行: %v", executed)
	}
	// resumed 保存落库后的运行状态，游标必须推进到计划末尾且状态为人工核对。
	resumed, resumedErr := store.Automation.GetRun(ctx, runID)
	if resumedErr != nil {
		t.Fatal(resumedErr)
	}
	if resumed.ActionCursor != len(actions) {
		t.Fatalf("放行后游标必须推进到计划末尾: %d", resumed.ActionCursor)
	}
	if resumed.Status != "needs_review" {
		t.Fatalf("放行链路的运行必须收口为人工核对: %+v", resumed)
	}
	if resumed.SentCount != 0 {
		t.Fatalf("不确定动作不得落库为已发送: %d", resumed.SentCount)
	}
	if resumed.DeliveryProof.TradeText == "" && len(resumed.DeliveryProof.Messages) == 0 && len(resumed.DeliveryProof.PicList) == 0 {
		t.Fatalf("结果不确定的卡密内容必须保留为发货凭证: %+v", resumed.DeliveryProof)
	}
}

// TestExecuteRunActionsQuarantinesUncertainMessageWithoutIdempotentTail 验证剩余动作仍会联系买家时维持熔断。
// 只要还有会再次联系买家的动作未完成，结果不确定就必须整体隔离，不能放行。
func TestExecuteRunActionsQuarantinesUncertainMessageWithoutIdempotentTail(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含两个消息动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "quarantine-acc",
		[]db.AutomationActionInput{
			{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionSendText, MessageTemplate: "后续通知", Enabled: true},
		})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "quarantine-acc", "o-quarantine")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是注入不确定动作的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		return actionExecutionResult{}, uncertainAction(errors.New("平台发送响应中断"))
	}, nil)
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "quarantine-acc", OrderID: "o-quarantine", ChatID: "chat", BuyerID: "buyer"}
	// actions 是仍包含消息动作的计划。
	actions := []db.AutomationAction{
		{ActionType: ActionSendCard, Enabled: true, CardID: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		{ActionType: ActionSendText, Enabled: true},
	}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if deferred || sent != 0 {
		t.Fatalf("熔断路径不得延迟或计入发送: sent=%d deferred=%v", sent, deferred)
	}
	if !errors.Is(runErr, errAutomationNeedsReview) {
		t.Fatalf("结果不确定必须收口为人工核对: %v", runErr)
	}
	// quarantined 保存落库后的运行状态；熔断时游标不得推进。
	quarantined, quarantinedErr := store.Automation.GetRun(ctx, runID)
	if quarantinedErr != nil {
		t.Fatal(quarantinedErr)
	}
	if quarantined.Status != "needs_review" || quarantined.ActionCursor != 0 {
		t.Fatalf("熔断路径游标不得推进: %+v", quarantined)
	}
}

// TestExecuteRunActionsDefersActionWithDelay 验证带延迟的动作会写入延迟队列并交还调度器。
func TestExecuteRunActionsDefersActionWithDelay(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含延迟发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "defer-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, DelaySeconds: 30}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "defer-acc", "o-defer")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是动作延迟按配置生效的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("延迟动作不应立即执行")
		return actionExecutionResult{}, nil
	}, nil)
	// 让延迟计算读取动作配置，模拟真实的延迟语义。
	coordinator.actionDelaySeconds = func(_ context.Context, action db.AutomationAction) (int, error) {
		return action.DelaySeconds, nil
	}
	// deferredTaskID 记录延迟任务写入时携带的运行标识，用于确认延期续租与任务写入成对发生。
	var deferredTaskID int64
	coordinator.deferTask = func(_ context.Context, task Task, dueAt int64) error {
		deferredTaskID = taskAutomationRunID(task)
		return nil
	}
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "defer-acc", OrderID: "o-defer", ChatID: "chat", BuyerID: "buyer"}
	// actions 是带延迟的动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1, DelaySeconds: 30}}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if runErr != nil {
		t.Fatalf("延迟动作应安全交还调度器: %v", runErr)
	}
	if !deferred || sent != 0 {
		t.Fatalf("延迟动作应标记为已延期: sent=%d deferred=%v", sent, deferred)
	}
	if deferredTaskID != runID {
		t.Fatalf("延迟任务应携带运行标识: got %d want %d", deferredTaskID, runID)
	}
}

// TestExecuteRunActionsReportsDelayLookupFailure 验证延迟计算失败会中止本次运行。
func TestExecuteRunActionsReportsDelayLookupFailure(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "delay-fail-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "delay-fail-acc", "o-delay-fail")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是延迟计算必定失败的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("延迟计算失败后不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	// delayErr 是注入的延迟计算失败原因。
	delayErr := errors.New("读取延迟配置失败")
	coordinator.actionDelaySeconds = func(context.Context, db.AutomationAction) (int, error) {
		return 0, delayErr
	}
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "delay-fail-acc", OrderID: "o-delay-fail"}
	// actions 是动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1}}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if !errors.Is(runErr, delayErr) || deferred || sent != 0 {
		t.Fatalf("延迟计算失败应原样上抛: sent=%d deferred=%v err=%v", sent, deferred, runErr)
	}
}

// TestExecuteRunActionsRejectsActionClaimedByOtherWorker 验证动作检查点被占用时立即放弃本次执行。
// 两个 worker 同时处理同一运行时，后到者必须退出而不是重复执行外部动作。
func TestExecuteRunActionsRejectsActionClaimedByOtherWorker(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "claimed-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "claimed-acc", "o-claimed")
	continuationRunState(t, store, runID, "running", 1, 0)
	// 模拟另一个 worker 已领取游标 0 的动作检查点。
	if _, claimErr := store.DB.ExecContext(ctx,
		`UPDATE automation_runs SET action_started=1, action_cursor=0 WHERE id=?`, runID); claimErr != nil {
		t.Fatal(claimErr)
	}
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是不执行任何外部动作的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("动作已被其他 worker 领取，不得重复执行")
		return actionExecutionResult{}, nil
	}, nil)
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "claimed-acc", OrderID: "o-claimed"}
	// actions 是动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1}}
	// _, _, runErr 是运行编排的返回结果；被占用时必须返回明确错误。
	_, _, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if runErr == nil || !strings.Contains(runErr.Error(), "已被其他 worker 领取") {
		t.Fatalf("动作被占用应返回明确错误: %v", runErr)
	}
}

// TestExecuteRuleQuarantinesPartialSuccessAndNotifies 验证部分动作成功后失败的运行进入人工核对并通知。
// 部分成功意味着重放会重复发货，必须隔离并通知管理员人工核对。
func TestExecuteRuleQuarantinesPartialSuccessAndNotifies(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含发卡与确认发货两个动作的规则主键。
	// order_paid 的动作计划只保留匹配规格的发卡动作与确认发货动作，因此第二个动作必须是确认发货。
	ruleID := continuationRule(t, store, admin.ID, "partial-acc",
		[]db.AutomationActionInput{
			{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionConfirmShipment, Enabled: true},
		})
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// notifications 记录运行结果通知。
	notifications := &continuationNotifyRecorder{}
	// coordinator 是第一个动作成功、第二个动作普通失败的运行协调器。
	coordinator := newContinuationCoordinator(store, func(_ context.Context, _ Task, action db.AutomationAction, _ shipmentDeliveryProof) (actionExecutionResult, error) {
		if action.ActionType == ActionSendCard {
			return actionExecutionResult{sent: 1, proof: shipmentDeliveryProof{tradeText: "卡密内容"}}, nil
		}
		return actionExecutionResult{}, errors.New("发货状态写入通道异常")
	}, notifications)
	// task 是与动作规格匹配的付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "partial-acc", OrderID: "o-partial",
		ChatID: "chat", BuyerID: "buyer", ItemID: "item-continuation", SpecName: "颜色", SpecValue: "黑"}
	// runErr 是运行编排的最终错误。
	runErr := coordinator.executeRule(ctx, task, *rule)
	if !errors.Is(runErr, errAutomationNeedsReview) {
		t.Fatalf("部分成功后失败必须收口为人工核对: %v", runErr)
	}
	if len(notifications.calls) != 1 || notifications.calls[0] != "needs_review" {
		t.Fatalf("人工核对通知缺失或状态异常: %v", notifications.calls)
	}
	// run 保存隔离后的运行记录。
	run, getErr := store.Automation.GetRun(ctx, 1)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != "needs_review" || run.SentCount != 1 {
		t.Fatalf("部分成功运行应记录已发送数量并隔离: %+v", run)
	}
}

// TestExecuteRuleFailsFastOnCertainNotSent 验证确定未发送的错误按可安全重试收口。
func TestExecuteRuleFailsFastOnCertainNotSent(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存只含发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "notsent-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`}})
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// coordinator 是发送被确定拒绝的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		return actionExecutionResult{}, fmt.Errorf("%w: 账号连接未就绪", ErrMessageNotSent)
	}, nil)
	// task 是与动作规格匹配的付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "notsent-acc", OrderID: "o-notsent",
		ChatID: "chat", BuyerID: "buyer", ItemID: "item-continuation", SpecName: "颜色", SpecValue: "黑"}
	// runErr 是运行编排的最终错误。
	runErr := coordinator.executeRule(ctx, task, *rule)
	if !errors.Is(runErr, ErrMessageNotSent) {
		t.Fatalf("确定未发送应保留原错误链: %v", runErr)
	}
	// run 保存失败收口后的运行记录。
	run, getErr := store.Automation.GetRun(ctx, 1)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != "failed" || run.ErrorMessage == "" || !strings.Contains(run.ErrorMessage, db.SafeRetryErrorPrefix) {
		t.Fatalf("确定未发送应标记为可安全重试失败: %+v", run)
	}
}

// TestExecuteRuleSkipsWhenTriggerKeyMissing 验证缺少稳定事件键的任务不会创建运行。
func TestExecuteRuleSkipsWhenTriggerKeyMissing(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存发卡规则主键。
	ruleID := continuationRule(t, store, admin.ID, "nokey-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// coordinator 是标准依赖的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("缺少事件键的任务不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	// task 既没有订单也没有更新键，无法构造幂等事件键。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "nokey-acc"}
	if skipErr := coordinator.executeRule(ctx, task, *rule); skipErr != nil {
		t.Fatalf("缺少事件键应安全跳过: %v", skipErr)
	}
	// runs 保存运行数量，必须为零。
	var runs int
	if countErr := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_runs`).Scan(&runs); countErr != nil {
		t.Fatal(countErr)
	}
	if runs != 0 {
		t.Fatalf("缺少事件键不应创建运行: %d", runs)
	}
}

// TestExecuteRuleRejectsForeignRunSnapshot 验证恢复快照不能借用其他账号的运行检查点。
func TestExecuteRuleRejectsForeignRunSnapshot(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存发卡规则主键。
	ruleID := continuationRule(t, store, admin.ID, "owner-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// runID 保存属于 owner-acc 的运行主键。
	runID := continuationRun(t, store, ruleID, "owner-acc", "o-foreign")
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// coordinator 是标准依赖的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("借用他人运行检查点不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	// task 声明的账号与运行归属不一致。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "intruder-acc", OrderID: "o-foreign",
		Raw: map[string]any{"automation_run_id": runID, "automation_rule_id": ruleID}}
	// runErr 是运行编排的最终错误。
	runErr := coordinator.executeRule(ctx, task, *rule)
	if !errors.Is(runErr, db.ErrForbidden) {
		t.Fatalf("跨账号恢复必须拒绝: %v", runErr)
	}
}

// TestExecuteRuleNotifiesUncertainQuarantine 验证结果不确定的运行会向管理员发出人工核对通知。
// 通知必须明确"结果未知"，避免管理员把不确定当成可重试失败而重复发货。
func TestExecuteRuleNotifiesUncertainQuarantine(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含发卡与确认发货的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "notify-acc",
		[]db.AutomationActionInput{
			{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionConfirmShipment, Enabled: true},
		})
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// notifications 记录运行结果通知。
	notifications := &continuationNotifyRecorder{}
	// coordinator 是发卡动作结果不确定的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		return actionExecutionResult{}, uncertainAction(errors.New("平台发送响应中断"))
	}, notifications)
	// task 是与动作规格匹配的付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "notify-acc", OrderID: "o-notify",
		ChatID: "chat", BuyerID: "buyer", ItemID: "item-continuation", SpecName: "颜色", SpecValue: "黑"}
	// runErr 是运行编排的最终错误。
	runErr := coordinator.executeRule(ctx, task, *rule)
	if !errors.Is(runErr, errAutomationNeedsReview) {
		t.Fatalf("结果不确定必须收口为人工核对: %v", runErr)
	}
	if len(notifications.calls) != 1 || notifications.calls[0] != "needs_review" {
		t.Fatalf("人工核对通知缺失或状态异常: %v", notifications.calls)
	}
}

// TestExecuteRuleReportsStaleSnapshotStorageFailure 验证恢复快照读取失败时立即上抛。
func TestExecuteRuleReportsStaleSnapshotStorageFailure(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存发卡规则主键。
	ruleID := continuationRule(t, store, admin.ID, "snapshot-fail-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// closeErr 保存数据库关闭错误；关闭后恢复快照读取必须失败。
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// coordinator 是标准依赖的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("存储不可用时不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	// task 携带指向既有运行的恢复快照，但存储已不可用。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "snapshot-fail-acc", OrderID: "o-snapshot-fail",
		Raw: map[string]any{"automation_run_id": int64(1), "automation_rule_id": ruleID}}
	if runErr := coordinator.executeRule(ctx, task, *rule); runErr == nil {
		t.Fatal("恢复快照读取失败应上抛错误")
	}
}

// TestExecuteRunActionsReportsLeaseRenewalFailure 验证延迟续租失败时中止运行。
// 续租失败意味着其他 worker 可能已接管，继续执行会与它竞争同一动作。
func TestExecuteRunActionsReportsLeaseRenewalFailure(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含延迟发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "lease-fail-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, DelaySeconds: 30}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "lease-fail-acc", "o-lease-fail")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存关闭前读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// closeErr 保存数据库关闭错误；关闭后续租必须失败。
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// coordinator 是延迟计算正常但存储不可用的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("续租失败后不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	coordinator.actionDelaySeconds = func(_ context.Context, action db.AutomationAction) (int, error) {
		return action.DelaySeconds, nil
	}
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "lease-fail-acc", OrderID: "o-lease-fail"}
	// actions 是带延迟的动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1, DelaySeconds: 30}}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if runErr == nil || deferred || sent != 0 {
		t.Fatalf("续租失败应上抛错误: sent=%d deferred=%v err=%v", sent, deferred, runErr)
	}
}

// TestExecuteRunActionsReportsDeferTaskFailure 验证延迟任务写入失败会中止运行。
// 延迟任务没写进去就继续执行会丢失这次动作，必须让调用方按可重试失败处理。
func TestExecuteRunActionsReportsDeferTaskFailure(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存包含延迟发卡动作的规则主键。
	ruleID := continuationRule(t, store, admin.ID, "defer-fail-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, DelaySeconds: 30}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "defer-fail-acc", "o-defer-fail")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是延迟任务写入必定失败的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("延迟任务写入失败后不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	coordinator.actionDelaySeconds = func(_ context.Context, action db.AutomationAction) (int, error) {
		return action.DelaySeconds, nil
	}
	// deferErr 是注入的延迟任务写入失败原因。
	deferErr := errors.New("延迟队列写入失败")
	coordinator.deferTask = func(context.Context, Task, int64) error {
		return deferErr
	}
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "defer-fail-acc", OrderID: "o-defer-fail"}
	// actions 是带延迟的动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1, DelaySeconds: 30}}
	// sent、deferred、runErr 是运行编排的返回结果。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if !errors.Is(runErr, deferErr) || deferred || sent != 0 {
		t.Fatalf("延迟任务写入失败应上抛: sent=%d deferred=%v err=%v", sent, deferred, runErr)
	}
}

// TestExecuteRunActionsStopsWhenAccountBlocked 验证账号门禁在动作执行前再次拦截。
// 运行创建到动作执行之间账号可能被停用，必须在触达外部系统前停下来。
func TestExecuteRunActionsStopsWhenAccountBlocked(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存发卡规则主键。
	ruleID := continuationRule(t, store, admin.ID, "blocked-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// runID 保存待执行运行主键。
	runID := continuationRun(t, store, ruleID, "blocked-acc", "o-blocked")
	continuationRunState(t, store, runID, "running", 1, 0)
	// run 保存重新读取的运行状态。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	// coordinator 是账号门禁返回停用的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("账号停用后不得执行动作")
		return actionExecutionResult{}, nil
	}, nil)
	// blockedErr 是注入的门禁读取失败原因。
	blockedErr := errors.New("读取账号状态失败")
	coordinator.accountAutomationAllowed = func(context.Context, string) (bool, error) {
		return false, blockedErr
	}
	// task 是付款发货任务。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "blocked-acc", OrderID: "o-blocked"}
	// actions 是动作计划。
	actions := []db.AutomationAction{{ActionType: ActionSendCard, Enabled: true, CardID: 1}}
	// sent、deferred、runErr 是运行编排的返回结果；门禁失败按普通动作失败处理。
	sent, deferred, runErr := coordinator.executeRunActions(ctx, task, ruleID, run, actions, false)
	if !errors.Is(runErr, blockedErr) || deferred || sent != 0 {
		t.Fatalf("账号门禁失败应上抛: sent=%d deferred=%v err=%v", sent, deferred, runErr)
	}
	// 改为“账号被停用”的布尔拒绝路径，必须返回明确文案。
	coordinator.accountAutomationAllowed = func(context.Context, string) (bool, error) {
		return false, nil
	}
	// 重新领取动作检查点后再次执行，覆盖停用文案分支。
	continuationRunState(t, store, runID, "running", 1, 0)
	// refreshed 保存重新读取的运行状态。
	refreshed, refreshedErr := store.Automation.GetRun(ctx, runID)
	if refreshedErr != nil {
		t.Fatal(refreshedErr)
	}
	// _, _, retryErr 是第二次执行的返回结果。
	_, _, retryErr := coordinator.executeRunActions(ctx, task, ruleID, refreshed, actions, false)
	if retryErr == nil || !strings.Contains(retryErr.Error(), "账号已暂停或停用") {
		t.Fatalf("账号停用应返回明确错误: %v", retryErr)
	}
}

// TestExecuteRuleSkipsStaleRunSnapshot 验证非 running 状态的恢复快照不会被执行。
func TestExecuteRuleSkipsStaleRunSnapshot(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// admin 保存规则所属管理员账号。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 保存发卡规则主键。
	ruleID := continuationRule(t, store, admin.ID, "stale-acc",
		[]db.AutomationActionInput{{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true}})
	// runID 保存已被人工核对隔离的运行主键。
	runID := continuationRun(t, store, ruleID, "stale-acc", "o-stale")
	continuationRunState(t, store, runID, "needs_review", 1, 0)
	// rule 保存从存储读取的完整规则。
	rule, ruleErr := store.Automation.Get(ctx, ruleID)
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// coordinator 是标准依赖的运行协调器。
	coordinator := newContinuationCoordinator(store, func(context.Context, Task, db.AutomationAction, shipmentDeliveryProof) (actionExecutionResult, error) {
		t.Fatal("已隔离运行不得通过恢复快照继续执行")
		return actionExecutionResult{}, nil
	}, nil)
	// task 携带指向已隔离运行的恢复快照。
	task := Task{TriggerType: TriggerOrderPaid, AccountID: "stale-acc", OrderID: "o-stale",
		Raw: map[string]any{"automation_run_id": runID, "automation_rule_id": ruleID}}
	if staleErr := coordinator.executeRule(ctx, task, *rule); staleErr != nil {
		t.Fatalf("非 running 快照应安全跳过: %v", staleErr)
	}
}
