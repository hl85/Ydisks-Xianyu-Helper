package engine

import (
	"context"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/ws"
)

// TestOutgoingEchoWaiterConfirmUnblocksWait 验证平台受理凭证能让等待项立即确认，无需等推送回显。
func TestOutgoingEchoWaiterConfirmUnblocksWait(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是本次登记的待确认发送。
	waiter := tracker.register("chat-1", "buyer-1", "text", "hello")
	if waiter == nil {
		t.Fatal("登记等待项失败")
	}
	waiter.confirm()
	if err := waiter.wait(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("confirm 后 wait 应立即成功: %v", err)
	}
	if len(tracker.pending) != 0 {
		t.Fatalf("确认后不应残留等待项: %d", len(tracker.pending))
	}
	// 重复确认必须安全：摘除动作已由第一次调用完成，不应重复关闭完成通道。
	waiter.confirm()
}

// TestOutgoingEchoWaiterConfirmAndObserveWakeOnce 验证响应路径与推送回显路径竞争时只唤醒一次且不 panic。
func TestOutgoingEchoWaiterConfirmAndObserveWakeOnce(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是本次登记的待确认发送。
	waiter := tracker.register("chat-1", "buyer-1", "text", "hello")
	waiter.confirm()
	// 推送回显在响应确认之后到达时不应重复唤醒，也不应留下残留等待项。
	tracker.observe(OutgoingChatMessage{ChatID: "chat-1", BuyerID: "buyer-1", MessageType: "text", Text: "hello"})
	if len(tracker.pending) != 0 {
		t.Fatalf("确认后再收到回显不应残留等待项: %d", len(tracker.pending))
	}
	// 已确认的等待项再被取消也必须安全。
	waiter.cancel()
}

// TestOutgoingEchoWaiterStillTimesOutWithoutEvidence 验证没有平台证据时仍保持原有的超时语义。
func TestOutgoingEchoWaiterStillTimesOutWithoutEvidence(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是本次登记的待确认发送。
	waiter := tracker.register("chat-1", "buyer-1", "text", "hello")
	if err := waiter.wait(context.Background(), 30*time.Millisecond); err == nil {
		t.Fatal("缺少平台证据时必须返回未确认错误")
	}
	waiter.cancel()
}

// TestConfirmEchoWithReceiptIgnoresUnusableReceipt 验证不可信凭证不会误唤醒等待项。
func TestConfirmEchoWithReceiptIgnoresUnusableReceipt(t *testing.T) {
	// tracker 是账号级出站回显跟踪器。
	tracker := newOutgoingEchoTracker()
	// waiter 是本次登记的待确认发送。
	waiter := tracker.register("chat-1", "buyer-1", "text", "hello")
	// 发送者非本人的凭证不足以证明这是当前账号的消息，必须继续等待推送回显。
	confirmEchoWithReceipt(waiter, ws.SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "other"}, "11873716")
	if err := waiter.wait(context.Background(), 30*time.Millisecond); err == nil {
		t.Fatal("凭证发送者非本人时不应确认")
	}
	waiter.cancel()
}
