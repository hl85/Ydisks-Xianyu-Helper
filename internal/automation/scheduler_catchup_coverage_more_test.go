package automation

import (
	"context"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// selectiveSenderProvider 按账号决定发送器是否就绪，用于覆盖自动化发送前的账号就绪门禁。
// 真实环境里账号连接状态各不相同，映射能在一个扫描轮次里同时覆盖就绪与未就绪两侧分支。
type selectiveSenderProvider struct {
	// senders 保存账号到已创建发送器的映射；同一账号必须始终返回同一实例，测试才能读到发送记录。
	senders map[string]*readinessTestSender
	// ready 保存账号到就绪状态的映射；缺失的账号按未就绪处理。
	ready map[string]bool
}

// Sender 返回指定账号的测试发送器；首次访问时创建并缓存。
func (p selectiveSenderProvider) Sender(accountID string) (MessageSender, bool) {
	// sender 是该账号复用的测试发送器；映射是引用类型，值接收者也能写回缓存。
	sender, ok := p.senders[accountID]
	if !ok {
		sender = &readinessTestSender{testSender: &testSender{}, ready: p.ready[accountID]}
		p.senders[accountID] = sender
	}
	return sender, true
}

// catchupCard 写入一条含单条库存的数据卡密组并返回卡密组主键。
// order_paid 触发的动作计划必须包含匹配订单规格的发卡动作，否则运行会被判定为失败。
func catchupCard(t *testing.T, store *db.Store, userID int64) int64 {
	t.Helper()
	// cardID、cardErr 保存数据卡密组写入结果与错误。
	cardID, cardErr := store.Cards.Create(context.Background(), &db.CardFull{
		Name: "catchup-card", Type: "data", DataContent: "secret-catchup-1", Enabled: true, UserID: userID,
	})
	if cardErr != nil {
		t.Fatal(cardErr)
	}
	return cardID
}

// catchupCardAction 构造一个绑定卡密组、每次发放一条的启用发卡动作。
// 规格过滤与夹具订单的「颜色/黑」保持一致；留空规格只会匹配无规格订单。
func catchupCardAction(cardID int64) db.AutomationActionInput {
	return db.AutomationActionInput{
		ActionType: ActionSendCard, CardID: cardID, DeliveryCount: 1, Enabled: true,
		ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`,
	}
}

// catchupAccount 写入一个可被自动化扫描选中的账号夹具。
// 账号必须有 cookies 行，否则账号状态门禁会以“账号不存在”拒绝自动化。
func catchupAccount(t *testing.T, store *db.Store, userID int64, cookieID string) {
	t.Helper()
	// saveErr 保存账号 Cookie 夹具写入错误。
	if saveErr := store.Cookies.Save(context.Background(), cookieID, "cv=1", userID); saveErr != nil {
		t.Fatalf("写入账号夹具失败: %v", saveErr)
	}
}

// catchupRule 写入一条启用的 order_paid 自动化规则并返回规则主键。
// 规则的商品匹配条件与订单 item_id 共同决定订单是否进入兜底扫描候选。
func catchupRule(t *testing.T, store *db.Store, userID int64, cookieID, itemID string, actions []db.AutomationActionInput) int64 {
	t.Helper()
	// ruleID、ruleErr 保存规则写入结果与错误；规则必须绑定商品，中心侧匹配器只按商品命中。
	ruleID, ruleErr := store.Automation.Create(context.Background(), db.AutomationRuleInput{
		UserID: userID, CookieID: cookieID, Name: "catchup-" + cookieID + "-" + itemID, ItemID: itemID,
		TriggerType: TriggerOrderPaid, Enabled: true, Actions: actions,
	})
	if ruleErr != nil {
		t.Fatal(ruleErr)
	}
	return ruleID
}

// catchupOrder 写入一条待发货订单夹具。
// 订单必须处于 pending_ship、未删除且带会话标识，才能被兜底扫描选中。
func catchupOrder(t *testing.T, store *db.Store, cookieID, orderID, itemID, chatID string) {
	t.Helper()
	// upsertErr 保存订单夹具写入错误。
	if upsertErr := store.Orders.Upsert(context.Background(), orderID, db.OrderUpsertOpts{
		ItemID: itemID, BuyerID: "buyer-" + orderID, CookieID: cookieID, ChatID: chatID,
		OrderStatus: "pending_ship", Quantity: "1", Amount: "9.90", SpecName: "颜色", SpecValue: "黑",
	}); upsertErr != nil {
		t.Fatalf("写入订单夹具失败: %v", upsertErr)
	}
}

// catchupRun 为订单写入一条 order_paid 运行记录并返回运行主键。
// rawTask 是运行携带的任务快照 JSON；续跑恢复路径依赖它还原账号与订单事实。
func catchupRun(t *testing.T, store *db.Store, ruleID int64, cookieID, orderID, rawTask string) int64 {
	t.Helper()
	// runID、started、startErr 保存运行写入结果与错误。
	runID, started, startErr := store.Automation.TryStartRun(context.Background(), db.AutomationRun{
		RuleID: ruleID, CookieID: cookieID, OrderID: orderID, TriggerType: TriggerOrderPaid,
		TriggerKey: "catchup:" + orderID, RawEventJSON: rawTask, LeaseExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if startErr != nil || !started {
		t.Fatalf("写入运行夹具失败: started=%v err=%v", started, startErr)
	}
	return runID
}

// setRunState 把运行改写成指定的状态、代次与动作游标。
// 兜底续跑扫描按状态、代次和游标筛选可续跑运行，夹具必须精确控制这三项。
func setRunState(t *testing.T, store *db.Store, runID int64, status string, attempt, cursor int) {
	t.Helper()
	// updateErr 保存运行状态改写错误。
	if _, updateErr := store.DB.ExecContext(context.Background(),
		`UPDATE automation_runs SET status=?, attempt_count=?, action_cursor=?, action_started=0, next_retry_at=0, lease_expires_at=0 WHERE id=?`,
		status, attempt, cursor, runID); updateErr != nil {
		t.Fatalf("改写运行夹具失败: %v", updateErr)
	}
}

// TestScanPendingShipDeliveriesSkipsWhenDisabledOrUninitialized 验证兜底扫描在开关关闭与状态缺失时安全返回。
// 止血开关与空状态保护是部署异常时的最后一道防线，缺少它们会造成无法回滚的重复发货。
func TestScanPendingShipDeliveriesSkipsWhenDisabledOrUninitialized(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// 显式关闭止血开关时，兜底扫描必须整体跳过。
	t.Setenv(pendingShipCatchupEnv, "0")
	// disabledCenter 是带真实存储但开关已关闭的自动化中心。
	disabledCenter := New(store, testSenderProvider{sender: &testSender{}}, nil)
	(&Scheduler{center: disabledCenter}).scanPendingShipDeliveries(ctx)
	// 恢复默认开关后，空状态保护仍必须拦截未初始化的调度器。
	t.Setenv(pendingShipCatchupEnv, "")
	// nilScheduler 是未初始化的调度器接收者。
	var nilScheduler *Scheduler
	nilScheduler.scanPendingShipDeliveries(ctx)
	// missingCenter 是缺少自动化中心的调度器。
	(&Scheduler{}).scanPendingShipDeliveries(ctx)
	// missingAutomation 是自动化存储未注入的调度器。
	(&Scheduler{center: &Center{store: &db.Store{}}}).scanPendingShipDeliveries(ctx)
	// enabledCenter 是开关开启且存储完整的中心，用于确认空库时直接返回。
	enabledCenter := New(store, testSenderProvider{sender: &testSender{}}, nil)
	(&Scheduler{center: enabledCenter}).scanPendingShipDeliveries(ctx)
}

// TestScanPendingShipDeliveriesCoversGatesCooldownAndSuccess 验证兜底扫描的账号门禁、冷却与补触发主链路。
// 这条链路就是 09-13 线上“卡密已发出但发货状态未更新”问题的兜底修复，必须覆盖每一类跳过原因。
func TestScanPendingShipDeliveriesCoversGatesCooldownAndSuccess(t *testing.T) {
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
	// cardID 是三类账号共用的数据卡密组，order_paid 动作计划必须含发卡动作。
	cardID := catchupCard(t, store, admin.ID)
	// 就绪、未就绪与暂停三类账号各配一条发货规则和一个待发货订单。
	// 注：订单的 cookie_id 受外键约束必须指向真实账号，因此“账号在扫描后被删除”
	// 这类账号状态读取失败分支无法用一致夹具构造，已在核心链路清单登记为例外。
	for _, cookieID := range []string{"ready-acc", "notready-acc", "paused-acc"} {
		catchupAccount(t, store, admin.ID, cookieID)
		catchupRule(t, store, admin.ID, cookieID, "item-"+cookieID, []db.AutomationActionInput{catchupCardAction(cardID)})
		catchupOrder(t, store, cookieID, "o-"+cookieID, "item-"+cookieID, "chat-"+cookieID)
	}
	// 暂停账号被用户临时停用，自动化必须跳过而不是继续发送。
	if _, pauseErr := store.DB.ExecContext(ctx,
		`UPDATE cookies SET paused_until=? WHERE id=?`, time.Now().Add(time.Hour).Unix(), "paused-acc"); pauseErr != nil {
		t.Fatal(pauseErr)
	}
	// provider 只对 ready-acc 报告发送器就绪，并缓存各账号发送器实例。
	provider := selectiveSenderProvider{
		senders: map[string]*readinessTestSender{},
		ready:   map[string]bool{"ready-acc": true},
	}
	// scheduler 是使用选择性发送器的调度器。
	scheduler := &Scheduler{center: New(store, provider, nil), pendingShipCooldown: map[string]time.Time{}}
	// 首轮扫描应补触发就绪账号，并对其余账号分别给出跳过原因。
	scheduler.scanPendingShipDeliveries(ctx)
	// readySender 是就绪账号的缓存发送器，用于确认补触发确实执行了发送。
	readySender := provider.senders["ready-acc"]
	if readySender == nil {
		t.Fatal("兜底扫描没有为就绪账号创建发送器")
	}
	// 若就绪账号没有收到文本，说明补触发主链路没有走通。
	if len(readySender.texts) == 0 {
		t.Fatal("待发货兜底扫描未为就绪账号补触发发送")
	}
	// runs 保存补触发产生的运行数量，用于确认运行已落库。
	var runs int
	if countErr := store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM automation_runs WHERE order_id='o-ready-acc' AND trigger_type='order_paid'`).Scan(&runs); countErr != nil {
		t.Fatal(countErr)
	}
	if runs != 1 {
		t.Fatalf("补触发应恰好产生一条运行: %d", runs)
	}
	// sentBeforeCooldown 保存冷却前已发送的文本数量。
	sentBeforeCooldown := len(readySender.texts)
	// 第二轮扫描时同一订单仍处于待发货，但必须被冷却窗口拦住，避免高频重试。
	scheduler.scanPendingShipDeliveries(ctx)
	if got := len(readySender.texts); got != sentBeforeCooldown {
		t.Fatalf("冷却窗口内不应重复补触发: before=%d after=%d", sentBeforeCooldown, got)
	}
	// 补触发产生的运行让订单退出“完全没有运行”的候选集合，后续交由失败恢复链处理。
	afterRuns, runErr := store.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 200)
	if runErr != nil {
		t.Fatal(runErr)
	}
	for _, candidate := range afterRuns {
		if candidate.OrderID == "o-ready-acc" {
			t.Fatalf("已有 order_paid 运行的订单不应再进入兜底候选: %+v", candidate)
		}
	}
}

// TestScanPendingShipDeliveriesReportsStorageFailures 验证存储不可用时兜底扫描返回而不是静默跳过。
// 兜底扫描漏报比多报更危险：付款消息丢失的订单会永久停在待发货。
func TestScanPendingShipDeliveriesReportsStorageFailures(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 是仍引用已关闭数据库的自动化中心。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// closeErr 保存数据库关闭错误。
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// scheduler 是带失效存储的调度器；扫描必须安全返回。
	scheduler := &Scheduler{center: center, pendingShipCooldown: map[string]time.Time{}}
	scheduler.scanPendingShipDeliveries(context.Background())
}

// TestClaimPendingShipAttemptCooldownsAndCleansExpiredEntries 验证冷却窗口与过期清理逻辑。
func TestClaimPendingShipAttemptCooldownsAndCleansExpiredEntries(t *testing.T) {
	// nilScheduler 用于验证空接收者保护。
	var nilScheduler *Scheduler
	if nilScheduler.claimPendingShipAttempt("o-nil") {
		t.Fatal("空调度器应拒绝领取兜底尝试")
	}
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 是带真实存储的自动化中心。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// scheduler 是被测调度器。
	scheduler := &Scheduler{center: center, pendingShipCooldown: map[string]time.Time{}}
	if !scheduler.claimPendingShipAttempt("o-first") {
		t.Fatal("首次领取应成功")
	}
	if scheduler.claimPendingShipAttempt("o-first") {
		t.Fatal("冷却窗口内的重复领取应失败")
	}
	// 预填大量已过期条目，验证映射超过阈值时触发清理而不是无界增长。
	// expiredKeys 保存预填的过期订单键。
	expiredKeys := make([]string, 0, 4096)
	// index 标识当前预填条目的序号。
	for index := 0; index < 4096; index++ {
		// key 是预填的过期订单键。
		key := "o-expired-" + string(rune('a'+index%26)) + "-" + time.Now().Add(time.Duration(index)*time.Microsecond).Format("150405.000000000")
		scheduler.pendingShipCooldown[key] = time.Now().Add(-defaultPendingShipCooldown - time.Minute)
		expiredKeys = append(expiredKeys, key)
	}
	if len(scheduler.pendingShipCooldown) <= 4096 {
		t.Fatalf("预填条目数应超过清理阈值: %d", len(scheduler.pendingShipCooldown))
	}
	if !scheduler.claimPendingShipAttempt("o-cleanup") {
		t.Fatal("超过阈值后的新订单领取应成功")
	}
	if len(scheduler.pendingShipCooldown) > 4097 {
		t.Fatalf("清理后映射应显著收缩: %d", len(scheduler.pendingShipCooldown))
	}
	// 清理必须只移除过期条目，本次领取的新条目必须保留。
	if _, ok := scheduler.pendingShipCooldown["o-cleanup"]; !ok {
		t.Fatal("本次领取的订单不应被清理")
	}
	// expiredKeys 仅用于说明预填集合，清理结果以映射长度断言为准。
	_ = expiredKeys
}

// TestScanPendingShipResumesContinuesIdempotentTails 验证续跑扫描能从检查点继续执行幂等收尾动作。
// 运行被隔离后剩余动作全是确认发货时，续跑不会再次联系买家，是安全的自动恢复路径。
func TestScanPendingShipResumesContinuesIdempotentTails(t *testing.T) {
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
	// catchupAccount 写入待续跑账号夹具。
	catchupAccount(t, store, admin.ID, "resume-acc")
	// ruleID 保存包含发卡与确认发货两类动作的发货规则主键。
	ruleID := catchupRule(t, store, admin.ID, "resume-acc", "item-resume",
		[]db.AutomationActionInput{catchupCardAction(catchupCard(t, store, admin.ID)), {ActionType: ActionConfirmShipment, Enabled: true}})
	catchupOrder(t, store, "resume-acc", "o-resume", "item-resume", "chat-resume")
	// rawTask 是运行携带的任务快照，续跑恢复路径依赖它还原账号与订单事实。
	rawTask := `{"Source":"scheduler","AccountID":"resume-acc","TriggerType":"order_paid","ChatID":"chat-resume","OrderID":"o-resume","ItemID":"item-resume","BuyerID":"buyer-o-resume","Text":"待发货运行未完成，按检查点续跑"}`
	// runID 保存被隔离的运行主键。
	runID := catchupRun(t, store, ruleID, "resume-acc", "o-resume", rawTask)
	// 运行已进入人工核对且游标越过发卡动作，属于可安全续跑候选。
	setRunState(t, store, runID, "needs_review", 1, 1)
	// provider 对待续跑账号报告发送器就绪。
	provider := selectiveSenderProvider{ready: map[string]bool{"resume-acc": true}}
	// scheduler 是使用选择性发送器的调度器。
	scheduler := &Scheduler{center: New(store, provider, nil), pendingShipCooldown: map[string]time.Time{}}
	scheduler.scanPendingShipResumes(ctx)
	// run 保存续跑后的运行记录，用于确认重开与续跑已经生效。
	run, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.AttemptCount != 2 {
		t.Fatalf("续跑应把代次递增到 2: %d", run.AttemptCount)
	}
	if run.Status == "needs_review" {
		t.Fatalf("续跑后运行不应仍停留在人工核对: %+v", run)
	}
	if run.ActionCursor == 0 && run.Status == "running" {
		t.Fatalf("续跑应推进动作游标或收口运行状态: %+v", run)
	}
	// 冷却窗口必须拦住同一订单的立即二次续跑，避免上游异常时每分钟重开一次。
	// 第二轮扫描后代次不得继续递增。
	scheduler.scanPendingShipResumes(ctx)
	// runAfterCooldown 保存冷却窗口内的运行记录，代次应保持不变。
	runAfterCooldown, cooldownErr := store.Automation.GetRun(ctx, runID)
	if cooldownErr != nil {
		t.Fatal(cooldownErr)
	}
	if runAfterCooldown.AttemptCount != run.AttemptCount {
		t.Fatalf("冷却窗口内不应重复续跑: before=%d after=%d", run.AttemptCount, runAfterCooldown.AttemptCount)
	}
}

// TestScanPendingShipResumesSkipsWhenDisabledOrUninitialized 验证续跑扫描在开关关闭与状态缺失时安全返回。
func TestScanPendingShipResumesSkipsWhenDisabledOrUninitialized(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// ctx 保存本测试共用的数据库上下文。
	ctx := context.Background()
	// 关闭共用止血开关时，续跑扫描必须整体跳过。
	t.Setenv(pendingShipCatchupEnv, "0")
	(&Scheduler{center: New(store, testSenderProvider{sender: &testSender{}}, nil)}).scanPendingShipResumes(ctx)
	// 恢复开关后，空状态保护仍必须拦截未初始化的调度器。
	t.Setenv(pendingShipCatchupEnv, "")
	// nilScheduler 是未初始化的调度器接收者。
	var nilScheduler *Scheduler
	nilScheduler.scanPendingShipResumes(ctx)
	// missingCenter 是缺少自动化中心的调度器。
	(&Scheduler{}).scanPendingShipResumes(ctx)
	// missingAutomation 是自动化存储未注入的调度器。
	(&Scheduler{center: &Center{store: &db.Store{}}}).scanPendingShipResumes(ctx)
}

// TestScanPendingShipResumesReportsStorageFailures 验证存储不可用时续跑扫描返回而不是静默跳过。
func TestScanPendingShipResumesReportsStorageFailures(t *testing.T) {
	// store、cleanup 保存测试数据库及关闭责任。
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 是仍引用已关闭数据库的自动化中心。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// closeErr 保存数据库关闭错误。
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// scheduler 是带失效存储的调度器；扫描必须安全返回。
	scheduler := &Scheduler{center: center, pendingShipCooldown: map[string]time.Time{}}
	scheduler.scanPendingShipResumes(context.Background())
}
