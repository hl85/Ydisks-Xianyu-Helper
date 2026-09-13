package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"xianyu-go/internal/automation"
	"xianyu-go/internal/xianyu/ws"
)

// receiptWSConn 在基础 WebSocket 替身之上实现带平台受理凭证的可选发送能力。
// 它用于验证「响应凭证优先、推送回显兜底」这条确认路径，以及两条路径各自的错误分支。
type receiptWSConn struct {
	// fakeWSConn 提供基础 WebSocket 生命周期与无凭证发送能力。
	*fakeWSConn
	// mu 保护本替身新增的可观察字段，避免与基础替身状态竞争。
	mu sync.Mutex
	// textReceipt、imageReceipt 是发送时回放给调用方的平台受理凭证。
	textReceipt  ws.SendReceipt
	imageReceipt ws.SendReceipt
	// textErr、imageErr 是可注入的发送失败原因。
	textErr  error
	imageErr error
	// textCalls、imageCalls 记录带凭证入口被调用的次数，用于证明未退回旧入口。
	textCalls  int
	imageCalls int
}

// SendTextWithReceipt 实现带受理凭证的文本发送，返回预设凭证与错误。
func (c *receiptWSConn) SendTextWithReceipt(_ context.Context, _, _, _, _ string) (ws.SendReceipt, error) {
	c.mu.Lock()
	c.textCalls++
	c.mu.Unlock()
	return c.textReceipt, c.textErr
}

// SendImageWithReceipt 实现带受理凭证的图片发送，返回预设凭证与错误。
func (c *receiptWSConn) SendImageWithReceipt(_ context.Context, _, _, _, _ string, _, _ int) (ws.SendReceipt, error) {
	c.mu.Lock()
	c.imageCalls++
	c.mu.Unlock()
	return c.imageReceipt, c.imageErr
}

// itemCardWSConn 在基础 WebSocket 替身之上实现商品卡片扩展协议。
type itemCardWSConn struct {
	// fakeWSConn 提供基础 WebSocket 生命周期能力。
	*fakeWSConn
	// mu 保护本替身新增的可观察字段。
	mu sync.Mutex
	// cardArgs 记录最后一次商品卡片发送的全部协议参数。
	cardArgs []string
	// cardErr 是可注入的商品卡片投递失败原因。
	cardErr error
}

// SendItemCard 实现商品卡片扩展协议，记录参数并返回预设错误。
func (c *itemCardWSConn) SendItemCard(_ context.Context, myID, chatID, toUserID, itemID, title, imageURL, price string) error {
	c.mu.Lock()
	c.cardArgs = []string{myID, chatID, toUserID, itemID, title, imageURL, price}
	c.mu.Unlock()
	return c.cardErr
}

// failingOutgoingHandler 是出站旁路必定失败的账号处理器，用于验证旁路失败不影响平台发送结论。
type failingOutgoingHandler struct {
	// outgoingObserverHandler 提供账号 Handler 契约的其余方法实现。
	outgoingObserverHandler
	// err 是出站旁路观察固定返回的错误。
	err error
}

// HandleOutgoingChatMessage 无论入参如何都返回注入的旁路错误。
func (h *failingOutgoingHandler) HandleOutgoingChatMessage(context.Context, OutgoingChatMessage) error {
	return h.err
}

// silentOutgoingHandler 是不实现出站旁路的账号处理器，用于验证旁路缺失时发送仍然成功。
type silentOutgoingHandler struct {
	// outgoingObserverHandler 提供账号 Handler 契约的其余方法实现。
	outgoingObserverHandler
}

// TestOutgoingEchoNilReceiversDoNotPanic 验证回显确认链路的空接收者保护分支。
// 这些分支是账号关闭与竞态收尾时的必经路径，缺少保护会导致进程级 panic。
func TestOutgoingEchoNilReceiversDoNotPanic(t *testing.T) {
	// nilTracker 是未初始化的账号级回显确认器。
	var nilTracker *outgoingEchoTracker
	if nilTracker.register("chat", "buyer", "text", "hello") != nil {
		t.Fatal("未初始化的确认器不应登记等待项")
	}
	nilTracker.observe(OutgoingChatMessage{ChatID: "chat", MessageType: "text", Text: "hello"})
	// nilWaiter 是未登记的等待项。
	var nilWaiter *outgoingEchoWaiter
	if err := nilWaiter.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("未登记的等待项应立即返回成功: %v", err)
	}
	nilWaiter.cancel()
	nilWaiter.confirm()
	// trackerlessWaiter 是缺少所属确认器的等待项。
	trackerlessWaiter := &outgoingEchoWaiter{}
	trackerlessWaiter.cancel()
	trackerlessWaiter.confirm()
}

// TestOutgoingEchoWaiterWaitUsesDefaultTimeoutOnNonPositiveBudget 验证非正预算回落到固定确认窗口。
func TestOutgoingEchoWaiterWaitUsesDefaultTimeoutOnNonPositiveBudget(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是预先登记的自动化文本等待项。
	waiter := tracker.register("chat-default", "buyer-default", "text", "默认预算")
	waiter.confirm()
	// 预算为零时不得立即超时，而应使用默认窗口；此时等待项已确认，应立即返回成功。
	if err := waiter.wait(context.Background(), 0); err != nil {
		t.Fatalf("已确认等待项在零预算下应立即成功: %v", err)
	}
}

// TestOutgoingEchoWaiterWaitHonorsCanceledContext 验证取消上下文的等待返回取消原因。
func TestOutgoingEchoWaiterWaitHonorsCanceledContext(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是保持未确认状态的等待项。
	waiter := tracker.register("chat-cancel", "buyer-cancel", "text", "取消边界")
	// ctx、cancel 是本次等待的取消边界。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waiter.wait(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消上下文应返回 context.Canceled: %v", err)
	}
	waiter.cancel()
}

// TestOutgoingEchoWaiterRemovalKeepsSiblings 验证摘除同类等待项时保留其余登记项。
// 同一会话可能连续发送相同正文，摘除必须只影响自身，不能清空同键的其它发送。
func TestOutgoingEchoWaiterRemovalKeepsSiblings(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// first、second 是同一会话与正文下先后登记的两个等待项。
	first := tracker.register("chat-sibling", "buyer-sibling", "text", "重复正文")
	second := tracker.register("chat-sibling", "buyer-sibling", "text", "重复正文")
	// key 是两个等待项共享的索引键。
	key := first.key
	// 确认同键中的首项时必须回写剩余列表，而不是把整键删除。
	first.confirm()
	if len(tracker.pending[key]) != 1 {
		t.Fatalf("确认首项后应保留同键等待项: %d", len(tracker.pending[key]))
	}
	// 再次确认已摘除项必须无操作，不能重复关闭完成通道。
	first.confirm()
	if len(tracker.pending[key]) != 1 {
		t.Fatalf("重复确认不应影响同键等待项: %d", len(tracker.pending[key]))
	}
	// stray 是键匹配但未登记的等待项；摘除必须找不到目标而不是误删他人。
	stray := &outgoingEchoWaiter{tracker: tracker, key: key, done: make(chan struct{})}
	stray.cancel()
	stray.confirm()
	if len(tracker.pending[key]) != 1 {
		t.Fatalf("未登记等待项不应摘除同键等待项: %d", len(tracker.pending[key]))
	}
	second.cancel()
	if len(tracker.pending) != 0 {
		t.Fatalf("全部摘除后不应残留等待项: %d", len(tracker.pending))
	}
	// 重复取消必须安全。
	second.cancel()
}

// TestOutgoingEchoTrackerObserveSkipsMismatchedBuyer 验证已有接收人身份时跳过不匹配的回显。
func TestOutgoingEchoTrackerObserveSkipsMismatchedBuyer(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是预期接收人为 buyer-a 的等待项。
	waiter := tracker.register("chat-buyer", "buyer-a", "text", "买家过滤")
	// 接收人不匹配的同类回显不能唤醒等待项，否则会把发给其他买家的消息当成自身确认。
	tracker.observe(OutgoingChatMessage{ChatID: "chat-buyer", BuyerID: "buyer-b", MessageType: "text", Text: "买家过滤"})
	if err := waiter.wait(context.Background(), 20*time.Millisecond); err == nil {
		t.Fatal("接收人不匹配的回显不应确认等待项")
	}
	// key 是等待项的索引键；此处同键登记两个等待项以覆盖非空切片回写分支。
	key := waiter.key
	// sibling 是与 waiter 同键的第二个等待项。
	sibling := tracker.register("chat-buyer", "buyer-a", "text", "买家过滤")
	tracker.observe(OutgoingChatMessage{ChatID: "chat-buyer", BuyerID: "buyer-a", MessageType: "text", Text: "买家过滤"})
	if len(tracker.pending[key]) != 1 {
		t.Fatalf("单个回显只应消费一个等待项: %d", len(tracker.pending[key]))
	}
	sibling.cancel()
	tracker.observe(OutgoingChatMessage{ChatID: "chat-buyer", BuyerID: "", MessageType: "text", Text: "买家过滤"})
	if err := waiter.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("接收人缺失时应按会话与正文确认: %v", err)
	}
}

// TestOutgoingEchoContentAndTypeNormalization 验证回显类型与比较正文的归一化。
func TestOutgoingEchoContentAndTypeNormalization(t *testing.T) {
	if got := normalizeOutgoingEchoType(""); got != "text" {
		t.Fatalf("空类型应回落为 text: %q", got)
	}
	if got := normalizeOutgoingEchoType("   "); got != "text" {
		t.Fatalf("空白类型应回落为 text: %q", got)
	}
	if got := normalizeOutgoingEchoType("image"); got != "image" {
		t.Fatalf("图片类型应原样返回: %q", got)
	}
	// 图片回显的正文来自 Content 字段，而不是文本字段。
	if got := outgoingEchoContent(OutgoingChatMessage{MessageType: "image", Content: "  https://cdn.example/gift.png  "}); got != "https://cdn.example/gift.png" {
		t.Fatalf("图片回显正文应取自 Content: %q", got)
	}
}

// TestSendTextWithReceiptPrefersReceiptCapability 验证连接支持受理凭证时走带凭证入口。
func TestSendTextWithReceiptPrefersReceiptCapability(t *testing.T) {
	// want 是连接回放的平台受理凭证。
	want := ws.SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "11873716"}
	// conn 是支持带凭证文本发送的连接替身。
	conn := &receiptWSConn{fakeWSConn: &fakeWSConn{}, textReceipt: want}
	// receipt、err 是带凭证发送入口的返回值。
	receipt, err := sendTextWithReceipt(conn, context.Background(), "11873716", "chat", "buyer", "正文")
	if err != nil || receipt.MessageID != want.MessageID {
		t.Fatalf("带凭证文本发送异常: receipt=%+v err=%v", receipt, err)
	}
	if conn.textCalls != 1 {
		t.Fatalf("应优先调用带凭证入口: %d", conn.textCalls)
	}
	// fallback 是不支持带凭证能力的连接替身，必须回落到原有发送语义。
	fallback := &fakeWSConn{}
	if _, err := sendTextWithReceipt(fallback, context.Background(), "me", "chat", "buyer", "正文"); err != nil {
		t.Fatalf("不支持凭证能力的连接应回落到原入口: %v", err)
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if len(fallback.sentTexts) != 1 {
		t.Fatalf("回落路径未真正发送文本: %d", len(fallback.sentTexts))
	}
}

// TestSendImageWithReceiptPrefersReceiptCapability 验证连接支持受理凭证时走带凭证图片入口。
func TestSendImageWithReceiptPrefersReceiptCapability(t *testing.T) {
	// want 是连接回放的平台受理凭证。
	want := ws.SendReceipt{MessageID: "4294154215668.PNM", SenderUserID: "11873716"}
	// conn 是支持带凭证图片发送的连接替身。
	conn := &receiptWSConn{fakeWSConn: &fakeWSConn{}, imageReceipt: want}
	// receipt、err 是带凭证图片发送入口的返回值。
	receipt, err := sendImageWithReceipt(conn, context.Background(), "11873716", "chat", "buyer", "https://cdn.example/gift.png", 100, 200)
	if err != nil || receipt.MessageID != want.MessageID {
		t.Fatalf("带凭证图片发送异常: receipt=%+v err=%v", receipt, err)
	}
	if conn.imageCalls != 1 {
		t.Fatalf("应优先调用带凭证入口: %d", conn.imageCalls)
	}
	// fallback 是不支持带凭证能力的连接替身，必须回落到原有发送语义。
	fallback := &fakeWSConn{}
	if _, err := sendImageWithReceipt(fallback, context.Background(), "me", "chat", "buyer", "https://cdn.example/gift.png", 100, 200); err != nil {
		t.Fatalf("不支持凭证能力的连接应回落到原入口: %v", err)
	}
	fallback.mu.Lock()
	defer fallback.mu.Unlock()
	if len(fallback.sentImages) != 1 {
		t.Fatalf("回落路径未真正发送图片: %d", len(fallback.sentImages))
	}
}

// TestConfirmEchoWithReceiptConfirmsOnTrustedEvidence 验证可信凭证直接唤醒等待项。
func TestConfirmEchoWithReceiptConfirmsOnTrustedEvidence(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是等待自身回显的自动化发送。
	waiter := tracker.register("chat-receipt", "buyer-receipt", "text", "凭证确认")
	confirmEchoWithReceipt(waiter, ws.SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "11873716"}, "11873716")
	if err := waiter.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("可信凭证应立即确认等待项: %v", err)
	}
	// 空等待项必须安全忽略，缺少保护会让人工聊天路径 panic。
	confirmEchoWithReceipt(nil, ws.SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "11873716"}, "11873716")
}

// TestSendTextReportsUnavailableConnection 验证没有可用连接时文本发送返回确定未发送错误。
func TestSendTextReportsUnavailableConnection(t *testing.T) {
	// account 是尚未建立连接但已具备账号身份的运行时。
	account := New(Config{CookieID: "no-conn-text", CookieStr: "unb=me"})
	if err := account.SendText(context.Background(), "chat", "buyer", "正文"); err == nil {
		t.Fatal("缺少连接时文本发送应失败")
	}
}

// TestSendTextPropagatesPlatformFailure 验证平台确定未发送的失败被归入可安全处理错误。
func TestSendTextPropagatesPlatformFailure(t *testing.T) {
	// account 是绑定了可注入失败连接的运行时。
	account := New(Config{CookieID: "text-fail", CookieStr: "unb=me"})
	// conn 是文本发送必定返回确定未发送错误的连接替身。
	conn := &receiptWSConn{fakeWSConn: &fakeWSConn{}, textErr: &ws.SendError{Kind: ws.SendNotSent, Err: errors.New("transport down")}}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	// err 是文本发送的最终错误；必须可由自动化按确定未发送处理。
	err := account.SendText(context.Background(), "chat", "buyer", "正文")
	if !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("确定未发送应归入 ErrMessageNotSent: %v", err)
	}
}

// TestSendTextKeepsSuccessWhenObserverFails 验证出站旁路失败不改变平台发送成功结论。
func TestSendTextKeepsSuccessWhenObserverFails(t *testing.T) {
	// observerErr 是出站旁路模拟的持久化失败。
	observerErr := errors.New("observer failed")
	// account 是出站旁路必定失败的运行时。
	account := New(Config{CookieID: "observer-fail", CookieStr: "unb=me", Handler: &failingOutgoingHandler{err: observerErr}})
	// conn 是文本发送成功的连接替身。
	conn := &receiptWSConn{fakeWSConn: &fakeWSConn{}, textReceipt: ws.SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "me"}}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	if err := account.SendText(WithOutgoingEchoConfirmation(context.Background()), "chat", "buyer", "正文"); err != nil {
		t.Fatalf("旁路失败不应让平台发送失败: %v", err)
	}
	// 不实现出站旁路的处理器同样必须让发送成功返回。
	plain := New(Config{CookieID: "observer-absent", CookieStr: "unb=me", Handler: &silentOutgoingHandler{}})
	plain.runtimeMu.Lock()
	plain.conn = conn
	plain.runtimeMu.Unlock()
	if err := plain.SendText(WithOutgoingEchoConfirmation(context.Background()), "chat", "buyer", "正文"); err != nil {
		t.Fatalf("缺少旁路处理器不应让平台发送失败: %v", err)
	}
}

// TestSendImageReportsUnavailableConnectionAndFailure 验证图片发送的连接缺失与平台失败分支。
func TestSendImageReportsUnavailableConnectionAndFailure(t *testing.T) {
	// account 是尚未建立连接但已具备账号身份的运行时。
	account := New(Config{CookieID: "no-conn-image", CookieStr: "unb=me"})
	if err := account.SendImage(context.Background(), "chat", "buyer", "https://cdn.example/gift.png", 0, 100, 200); err == nil {
		t.Fatal("缺少连接时图片发送应失败")
	}
	// failing 是图片发送必定返回确定未发送错误的运行时。
	failing := New(Config{CookieID: "image-fail", CookieStr: "unb=me"})
	// conn 是图片发送必定失败的连接替身。
	conn := &receiptWSConn{fakeWSConn: &fakeWSConn{}, imageErr: &ws.SendError{Kind: ws.SendNotSent, Err: errors.New("transport down")}}
	failing.runtimeMu.Lock()
	failing.conn = conn
	failing.runtimeMu.Unlock()
	if err := failing.SendImage(context.Background(), "chat", "buyer", "https://cdn.example/gift.png", 0, 100, 200); !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("图片确定未发送应归入 ErrMessageNotSent: %v", err)
	}
}

// TestConfirmOutgoingEchoFallsBackToDefaultBudget 验证协调器未配置预算时使用固定确认窗口。
func TestConfirmOutgoingEchoFallsBackToDefaultBudget(t *testing.T) {
	// coordinator 是未配置回显等待预算的协调器。
	coordinator := &outgoingMessageCoordinator{account: New(Config{CookieID: "budget-default", CookieStr: "unb=me"}), echoTracker: newOutgoingEchoTracker()}
	// waiter 是已经确认的等待项；零预算必须回落到默认窗口而不是立即超时。
	waiter := coordinator.echoTracker.register("chat-budget", "buyer-budget", "text", "默认预算")
	waiter.confirm()
	if err := coordinator.confirmOutgoingEcho(context.Background(), waiter, "chat-budget"); err != nil {
		t.Fatalf("已确认等待项在默认预算下应成功: %v", err)
	}
	// 空等待项在人工聊天路径上必须直接成功。
	if err := coordinator.confirmOutgoingEcho(context.Background(), nil, "chat-budget"); err != nil {
		t.Fatalf("空等待项应直接成功: %v", err)
	}
}

// TestSendItemCardRequiresInitializedCoordinator 验证未初始化的协调器拒绝商品卡片发送。
func TestSendItemCardRequiresInitializedCoordinator(t *testing.T) {
	// coordinator 是未初始化账号的协调器。
	coordinator := &outgoingMessageCoordinator{}
	if err := coordinator.sendItemCard(context.Background(), "chat", "buyer", "item-1", "标题", "https://cdn.example/i.png", "99"); err == nil {
		t.Fatal("未初始化协调器应拒绝商品卡片发送")
	}
	// 已初始化但缺少连接时同样必须失败，而不是静默丢弃卡片。
	account := New(Config{CookieID: "card-no-conn", CookieStr: "unb=me"})
	if err := account.SendItemCard(context.Background(), "chat", "buyer", "item-1", "标题", "https://cdn.example/i.png", "99"); err == nil {
		t.Fatal("缺少连接时商品卡片发送应失败")
	}
}

// TestSendItemCardRejectsUnsupportedTransport 验证连接不支持扩展协议时返回确定未发送错误。
func TestSendItemCardRejectsUnsupportedTransport(t *testing.T) {
	// account 是绑定基础 WebSocket 替身的运行时。
	account := New(Config{CookieID: "card-unsupported", CookieStr: "unb=me"})
	account.runtimeMu.Lock()
	account.conn = &fakeWSConn{}
	account.runtimeMu.Unlock()
	if err := account.SendItemCard(context.Background(), "chat", "buyer", "item-1", "标题", "https://cdn.example/i.png", "99"); !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("不支持卡片协议应归入 ErrMessageNotSent: %v", err)
	}
}

// TestSendItemCardObservesCanonicalContent 验证商品卡片成功后向出站旁路投递规范化正文。
func TestSendItemCardObservesCanonicalContent(t *testing.T) {
	// handler 是记录出站消息的账号处理器。
	handler := &outgoingObserverHandler{}
	// account 是绑定卡片协议连接与观察器的运行时。
	account := New(Config{CookieID: "card-ok", CookieStr: "unb=me", Handler: handler})
	// conn 是支持商品卡片协议的连接替身。
	conn := &itemCardWSConn{fakeWSConn: &fakeWSConn{}}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	// ctx 携带 UI 待发送消息关联键，用于避免旁路重复插入。
	ctx := WithOutgoingMessageKey(context.Background(), "local-card-1")
	if err := account.SendItemCard(ctx, "chat-card", "buyer-card", " item-1 ", " 标题 ", " https://cdn.example/i.png ", "¥99"); err != nil {
		t.Fatalf("商品卡片发送失败: %v", err)
	}
	if len(conn.cardArgs) != 7 || conn.cardArgs[3] != " item-1 " {
		t.Fatalf("商品卡片协议参数未原样透传: %+v", conn.cardArgs)
	}
	if len(handler.messages) != 1 {
		t.Fatalf("商品卡片应产生一条出站观察: %+v", handler.messages)
	}
	// observed 是出站旁路收到的商品卡片消息。
	observed := handler.messages[0]
	if observed.MessageType != "item" || observed.MessageKey != "local-card-1" || observed.ChatID != "chat-card" || observed.BuyerID != "buyer-card" {
		t.Fatalf("商品卡片出站观察异常: %+v", observed)
	}
	// payload 是旁路正文解析结果，用于验证首尾空白与货币符号已规范化。
	payload := map[string]string{}
	if err := json.Unmarshal([]byte(observed.Content), &payload); err != nil {
		t.Fatalf("商品卡片正文不是合法 JSON: %v", err)
	}
	if payload["item_id"] != "item-1" || payload["title"] != "标题" || payload["image_url"] != "https://cdn.example/i.png" || payload["price"] != "99" {
		t.Fatalf("商品卡片正文未规范化: %+v", payload)
	}
}

// TestSendItemCardPropagatesTransportFailure 验证商品卡片投递失败被归入可安全处理错误。
func TestSendItemCardPropagatesTransportFailure(t *testing.T) {
	// account 是绑定失败卡片连接的运行时。
	account := New(Config{CookieID: "card-fail", CookieStr: "unb=me"})
	// conn 是商品卡片投递必定失败的连接替身。
	conn := &itemCardWSConn{fakeWSConn: &fakeWSConn{}, cardErr: &ws.SendError{Kind: ws.SendRejected, Code: 400}}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	if err := account.SendItemCard(context.Background(), "chat", "buyer", "item-1", "标题", "https://cdn.example/i.png", "99"); !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("明确拒绝应归入 ErrMessageNotSent: %v", err)
	}
}

// TestSendItemCardToleratesObserverFailure 验证出站旁路失败不影响商品卡片发送结论。
func TestSendItemCardToleratesObserverFailure(t *testing.T) {
	// account 是出站旁路必定失败的运行时。
	account := New(Config{CookieID: "card-observer-fail", CookieStr: "unb=me", Handler: &failingOutgoingHandler{err: errors.New("observer failed")}})
	// conn 是商品卡片投递成功的连接替身。
	conn := &itemCardWSConn{fakeWSConn: &fakeWSConn{}}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	if err := account.SendItemCard(context.Background(), "chat", "buyer", "item-1", "标题", "https://cdn.example/i.png", "99"); err != nil {
		t.Fatalf("旁路失败不应让商品卡片发送失败: %v", err)
	}
}

// TestClassifyPlatformSendErrorBranches 验证平台发送错误分类的三个分支。
func TestClassifyPlatformSendErrorBranches(t *testing.T) {
	if err := classifyPlatformSendError(nil); err != nil {
		t.Fatalf("空错误应保持为空: %v", err)
	}
	// notSent 是协议层已经确定未发送的错误。
	notSent := &ws.SendError{Kind: ws.SendNotSent}
	if err := classifyPlatformSendError(notSent); !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("确定未发送应归入 ErrMessageNotSent: %v", err)
	}
	// uncertain 是无法判定送达结果的错误；必须原样保留，禁止自动重放。
	uncertain := &ws.SendError{Kind: ws.SendUncertain}
	if err := classifyPlatformSendError(uncertain); !errors.Is(err, uncertain) {
		t.Fatalf("不确定结果应原样保留: %v", err)
	}
}

// TestFetchChatConversationsReportsUnavailableConnection 验证缺少连接时会话查询失败。
func TestFetchChatConversationsReportsUnavailableConnection(t *testing.T) {
	// account 是尚未建立连接但已具备账号身份的运行时。
	account := New(Config{CookieID: "conv-no-conn", CookieStr: "unb=me"})
	// body、myID、err 是会话查询的三元返回结果。
	body, myID, err := account.FetchChatConversations(context.Background(), 0, 20)
	if err == nil || body != nil || myID != "" {
		t.Fatalf("缺少连接时会话查询应失败: body=%v user=%q err=%v", body, myID, err)
	}
}

// TestAutomationReadyWithUninitializedCoordinator 验证未绑定账号的协调器不提供自动化发送能力。
func TestAutomationReadyWithUninitializedCoordinator(t *testing.T) {
	// coordinator 是未绑定账号的协调器。
	coordinator := &outgoingMessageCoordinator{}
	if coordinator.automationReady() {
		t.Fatal("未绑定账号的协调器不应就绪")
	}
}

// TestSendTextIgnoresBlankBody 验证空白正文不会触发平台发送。
func TestSendTextIgnoresBlankBody(t *testing.T) {
	// account 是绑定记录型连接的运行时。
	account := New(Config{CookieID: "blank-text", CookieStr: "unb=me"})
	// conn 是记录文本写入的连接替身。
	conn := &fakeWSConn{}
	account.runtimeMu.Lock()
	account.conn = conn
	account.runtimeMu.Unlock()
	if err := account.SendText(context.Background(), "chat", "buyer", strings.Repeat(" ", 3)); err != nil {
		t.Fatalf("空白正文应直接成功返回: %v", err)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.sentTexts) != 0 {
		t.Fatalf("空白正文不应写入平台: %d", len(conn.sentTexts))
	}
}
