// ai_intent_test.go 意图注册表与 AI 接管策略的表驱动测试。

package engine

import (
	"reflect"
	"testing"
)

// TestClassifyIntent 验证各意图样例的命中集合与优先级排序。
func TestClassifyIntent(t *testing.T) {
	// cases 覆盖单一命中、负向短路、多命中与零命中四类形态。
	cases := []struct {
		// name 是用例名称。
		name string
		// text 是买家消息样例。
		text string
		// want 是期望的按优先级排序的命中意图。
		want []string
	}{
		{name: "纯砍价", text: "60 块能卖吗，能便宜点吗", want: []string{IntentBargain}},
		{name: "纯投诉", text: "这根本不是正品，我要举报你卖假货", want: []string{IntentComplaint}},
		{name: "投诉优先于砍价", text: "再便宜点，不然我差评举报", want: []string{IntentComplaint}},
		{name: "砍价叠加询价", text: "最低多少钱，包邮吗", want: []string{IntentBargain, IntentInquiry}},
		{name: "纯发货咨询", text: "付款后什么时候发货，提取码怎么用", want: []string{IntentOrder, IntentConsult}},
		{name: "纯询价", text: "这套资料怎么卖", want: []string{IntentInquiry}},
		{name: "零命中闲聊", text: "你好在吗", want: []string{}},
	}
	// testCase 是当前遍历到的输入样例。
	for _, testCase := range cases {
		// got 是实际命中的意图集合。
		got := classifyIntent(testCase.text)
		if !reflect.DeepEqual(got, testCase.want) {
			t.Fatalf("%s: classifyIntent(%q) = %v, want %v", testCase.name, testCase.text, got, testCase.want)
		}
	}
}

// TestClassifyIntentMixedLanguage 验证中英混排消息里的中文纠纷表达仍然命中负向意图。
func TestClassifyIntentMixedLanguage(t *testing.T) {
	// got 是中英混排投诉样例的命中集合。
	got := classifyIntent("this is fake, 我要退款 REFUND NOW")
	if len(got) == 0 || got[0] != IntentComplaint {
		t.Fatalf("中英混排投诉应命中负向意图，实际 %v", got)
	}
}

// TestAIShouldHandleIntent 验证 AI 接管策略的放行与拦截边界。
func TestAIShouldHandleIntent(t *testing.T) {
	// cases 覆盖放行、负向否决、零命中与其它意图不放行四类结论。
	cases := []struct {
		// name 是用例名称。
		name string
		// hits 是 classifyIntent 的输出。
		hits []string
		// want 是期望的接管结论。
		want bool
	}{
		{name: "明确砍价放行", hits: []string{IntentBargain}, want: true},
		{name: "砍价叠加询价仍放行", hits: []string{IntentBargain, IntentInquiry}, want: true},
		{name: "投诉否决", hits: []string{IntentComplaint}, want: false},
		{name: "投诉叠加砍价仍否决", hits: []string{IntentComplaint, IntentBargain}, want: false},
		{name: "零命中不放行", hits: []string{}, want: false},
		{name: "仅发货咨询不放行", hits: []string{IntentOrder}, want: false},
		{name: "仅询价不放行", hits: []string{IntentInquiry}, want: false},
	}
	// testCase 是当前遍历到的策略样例。
	for _, testCase := range cases {
		// got 是实际的接管结论。
		got := aiShouldHandleIntent(testCase.hits)
		if got != testCase.want {
			t.Fatalf("%s: aiShouldHandleIntent(%v) = %v, want %v", testCase.name, testCase.hits, got, testCase.want)
		}
	}
}

// TestAIShouldHandleIntentNilHits 验证空入参不接管且不 panic。
func TestAIShouldHandleIntentNilHits(t *testing.T) {
	if aiShouldHandleIntent(nil) {
		t.Fatal("空命中集合不应接管 AI")
	}
}

// TestClassifyIntentMatchesLegacyBargainGate 验证注册表的砍价规则与历史接管范围一致：
// 历史 bargainMessageRe 命中的消息，注册表也必须给出砍价意图，保证接管范围不回退。
func TestClassifyIntentMatchesLegacyBargainGate(t *testing.T) {
	// samples 是历史上会交给 AI 的砍价表达样例。
	samples := []string{
		"便宜点", "少点呗", "最低多少", "砍价", "降价", "打折",
		"能不能 50 元", "50 块卖不卖", "可以便宜吗",
	}
	// sample 是当前遍历到的历史样例。
	for _, sample := range samples {
		// hits 是样例的命中集合。
		hits := classifyIntent(sample)
		// found 表示样例是否命中了砍价意图。
		found := false
		// intent 是当前遍历到的命中意图。
		for _, intent := range hits {
			if intent == IntentBargain {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("历史砍价样例 %q 应命中砍价意图，实际 %v", sample, hits)
		}
	}
}
