package engine

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestScanReplySafety_BlocksOffPlatformContact 验证引导站外联系的表达会被判定违规并替换为兜底话术。
func TestScanReplySafety_BlocksOffPlatformContact(t *testing.T) {
	t.Setenv(replySafetyDisableEnv, "1")
	// cases 是需要被拦截的站外导流表达样例。
	cases := []string{
		"加我微信发你链接",
		"亲，这个可以微信号下单",
		"加QQ聊吧",
		"建议私下交易更便宜",
	}
	// text 是当前遍历到的样例文本。
	for _, text := range cases {
		// verdict 是单条样例的扫描结论。
		verdict := scanReplySafety(text)
		if !verdict.Blocked {
			t.Fatalf("文本 %q 应被拦截", text)
		}
		if verdict.Reason != "引导站外联系方式" {
			t.Fatalf("文本 %q 的违规原因应为引导站外联系方式，实际 %q", text, verdict.Reason)
		}
		if verdict.Text != replySafetyFallbackText {
			t.Fatalf("文本 %q 命中后应替换为兜底话术，实际 %q", text, verdict.Text)
		}
	}
}

// TestScanReplySafety_BlocksForbiddenPromise 验证超出售后承诺的表达会被判定违规。
func TestScanReplySafety_BlocksForbiddenPromise(t *testing.T) {
	// verdict 是违规承诺样例的扫描结论。
	verdict := scanReplySafety("我们保证学会，学不会退款秒到")
	if !verdict.Blocked {
		t.Fatal("违规承诺应被拦截")
	}
	if verdict.Reason != "超出售后承诺" {
		t.Fatalf("违规原因应为超出售后承诺，实际 %q", verdict.Reason)
	}
	if verdict.Text != replySafetyFallbackText {
		t.Fatalf("命中后应替换为兜底话术，实际 %q", verdict.Text)
	}
}

// TestScanReplySafety_CaseInsensitiveKeyword 验证英文关键词按大小写不敏感匹配。
func TestScanReplySafety_CaseInsensitiveKeyword(t *testing.T) {
	t.Setenv(replySafetyDisableEnv, "1")
	// verdict 是大小写混写样例的扫描结论。
	verdict := scanReplySafety("加QQ聊")
	if !verdict.Blocked {
		t.Fatal("大小写混写的关键词也应被拦截")
	}
	if verdict.Matched == "" {
		t.Fatal("命中时应记录命中的关键词")
	}
}

// TestScanReplySafety_AllowsNormalText 验证正常客服文本不会被误判，且原文保持不变。
func TestScanReplySafety_AllowsNormalText(t *testing.T) {
	// text 是一条完全正常的商品说明。
	text := "这份李诞工作手册共 128 页，付款后自动发送网盘链接和提取码，下载后永久可看。"
	// verdict 是正常文本的扫描结论。
	verdict := scanReplySafety(text)
	if verdict.Blocked {
		t.Fatalf("正常文本不应被拦截，实际命中 %q", verdict.Matched)
	}
	if verdict.Truncated {
		t.Fatal("正常文本不应被截断")
	}
	if verdict.Text != text {
		t.Fatalf("未命中时原文应保持不变，实际 %q", verdict.Text)
	}
}

// TestScanReplySafety_TruncatesLongText 验证超长文本会被按 rune 截断到上限。
func TestScanReplySafety_TruncatesLongText(t *testing.T) {
	// text 是超过上限的文本，用中文多字节字符构造以同时验证不会截坏字符。
	text := strings.Repeat("好", replySafetyMaxRunes+50)
	// verdict 是超长文本的扫描结论。
	verdict := scanReplySafety(text)
	if verdict.Blocked {
		t.Fatal("超长但无违规词的文本不应被判定违规")
	}
	if !verdict.Truncated {
		t.Fatal("超长文本应被截断")
	}
	// got 是截断后文本的字符数。
	got := len([]rune(verdict.Text))
	if got != replySafetyMaxRunes {
		t.Fatalf("截断后长度应为 %d，实际 %d", replySafetyMaxRunes, got)
	}
}

// TestScanReplySafety_DisabledByEnv 验证急停开关只关闭违规判定，长度保护仍然生效。
func TestScanReplySafety_DisabledByEnv(t *testing.T) {
	t.Setenv(replySafetyDisableEnv, "0")
	// blocked 是关闭闸门后含违规词文本的扫描结论。
	blocked := scanReplySafety("加我微信")
	if blocked.Blocked {
		t.Fatal("闸门关闭时不应判定违规")
	}
	if blocked.Text != "加我微信" {
		t.Fatalf("闸门关闭时原文应保持不变，实际 %q", blocked.Text)
	}
	// long 是关闭闸门后的超长文本扫描结论。
	long := scanReplySafety(strings.Repeat("长", replySafetyMaxRunes+1))
	if !long.Truncated {
		t.Fatal("闸门关闭时长度保护仍应生效")
	}
}

// newSafetyTestService 构造一个只用于安全闸门校验的回复服务，不依赖数据库与平台连接。
func newSafetyTestService() *ReplyService {
	return NewReplyService("cid-safety", nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestApplyReplySafety_ClearsPriceQuote 验证文本被替换时，AI 的自动报价承诺会一并作废。
func TestApplyReplySafety_ClearsPriceQuote(t *testing.T) {
	t.Setenv(replySafetyDisableEnv, "1")
	// service 是本次校验使用的回复服务。
	service := newSafetyTestService()
	// res 是一条命中安全闸门、且携带自动报价的 AI 回复。
	res := &ReplyResult{Text: "加我微信给你便宜点", Source: "AI", AutoPriceQuote: &AIPriceQuoteProposal{PriceCents: 880}}
	// verdict 是执行安全闸门后的结论。
	verdict := service.applyReplySafety(res)
	if !verdict.Blocked {
		t.Fatal("命中文本应被判定违规")
	}
	if res.Text != replySafetyFallbackText {
		t.Fatalf("正文应替换为兜底话术，实际 %q", res.Text)
	}
	if res.AutoPriceQuote != nil {
		t.Fatal("正文被替换后自动报价承诺应作废")
	}
}

// TestApplyReplySafety_SkipsEmptyText 验证纯图片回复没有正文时安全闸门不产生任何改动。
func TestApplyReplySafety_SkipsEmptyText(t *testing.T) {
	// service 是本次校验使用的回复服务。
	service := newSafetyTestService()
	// res 是一条只发图片、没有正文的回复。
	res := &ReplyResult{ImageURL: "https://example.com/a.png", Source: "默认"}
	// verdict 是执行安全闸门后的结论。
	verdict := service.applyReplySafety(res)
	if verdict.Blocked || verdict.Truncated {
		t.Fatal("无正文时不应产生任何安全处置")
	}
	if res.Text != "" {
		t.Fatalf("无正文时不应被填入内容，实际 %q", res.Text)
	}
}
