package ws

import (
	"context"
	"testing"
)

// TestExtractSendReceiptFromPlatformResponse 用真实发送响应的形状验证受理凭证提取。
// 该形状取自 2026-09-13 线上一次 sendByReceiverScope 的成功响应。
func TestExtractSendReceiptFromPlatformResponse(t *testing.T) {
	// response 是平台对该次发送的完整应答：headers.mid 与请求一致，body 携带消息身份与正文摘要。
	response := map[string]any{
		"code": 200,
		"headers": map[string]any{
			"dt":  "j",
			"mid": "2351789260661270 0",
			"sid": "212c42a76aa5de9324013b4716ad4977596039d9099b",
		},
		"body": map[string]any{
			"content":  map[string]any{"contentType": 101},
			"createAt": float64(1789260661412),
			"extension": map[string]any{
				"senderUserId":    "11873716",
				"reminderContent": "通过网盘分享的文件：李诞工作手册",
			},
			"messageId": "4294154215667.PNM",
		},
	}
	// receipt 是提取出的平台受理凭证。
	receipt := extractSendReceipt(response)
	if receipt.MessageID != "4294154215667.PNM" {
		t.Fatalf("messageId 提取异常: %q", receipt.MessageID)
	}
	if receipt.SenderUserID != "11873716" {
		t.Fatalf("senderUserId 提取异常: %q", receipt.SenderUserID)
	}
	if receipt.CreateAt != 1789260661412 {
		t.Fatalf("createAt 提取异常: %d", receipt.CreateAt)
	}
	if !receipt.ConfirmsOwnMessage("11873716") {
		t.Fatal("完整凭证应能确认自身消息")
	}
}

// TestSendReceiptRejectsIncompleteEvidence 验证凭证不完整时不会自欺地判为已确认。
func TestSendReceiptRejectsIncompleteEvidence(t *testing.T) {
	// cases 覆盖缺失、错配和带协议后缀三类边界。
	cases := []struct {
		name      string
		receipt   SendReceipt
		accountID string
		want      bool
	}{
		{name: "空凭证", receipt: SendReceipt{}, accountID: "11873716", want: false},
		{name: "缺少消息ID", receipt: SendReceipt{SenderUserID: "11873716"}, accountID: "11873716", want: false},
		{name: "缺少账号身份", receipt: SendReceipt{MessageID: "4294154215667.PNM"}, accountID: "", want: false},
		{name: "发送者非本人", receipt: SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "2218464965713"}, accountID: "11873716", want: false},
		{name: "账号带协议后缀", receipt: SendReceipt{MessageID: "4294154215667.PNM", SenderUserID: "11873716@goofish"}, accountID: "11873716", want: true},
	}
	for _, tc := range cases {
		if got := tc.receipt.ConfirmsOwnMessage(tc.accountID); got != tc.want {
			t.Errorf("%s: ConfirmsOwnMessage=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestExtractSendReceiptWithoutBody 验证响应缺少 body 时返回零值凭证而非 panic。
func TestExtractSendReceiptWithoutBody(t *testing.T) {
	if receipt := extractSendReceipt(map[string]any{"code": 200}); receipt.MessageID != "" {
		t.Fatalf("缺少 body 时应返回零值凭证: %+v", receipt)
	}
}

// TestSendTextWithReceiptKeepsSendTextSemantics 确保新增的凭证接口与原接口共用同一发送路径。
func TestSendTextWithReceiptKeepsSendTextSemantics(t *testing.T) {
	// conn 是未建立连接的零值连接；两个入口都应在参数校验阶段拒绝空账号。
	var conn Conn
	// ctx 是本次调用的取消边界。
	ctx := context.Background()
	if err := conn.SendText(ctx, "", "cid", "to", "text"); err == nil {
		t.Fatal("SendText 应拒绝空账号")
	}
	if _, err := conn.SendTextWithReceipt(ctx, "", "cid", "to", "text"); err == nil {
		t.Fatal("SendTextWithReceipt 应拒绝空账号")
	}
}
