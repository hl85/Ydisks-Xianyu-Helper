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

// TestParseComplaintKeywords 验证扩展词表解析：空集回落、非法正则忽略、正常正则编译、分隔符兼容。
func TestParseComplaintKeywords(t *testing.T) {
	// 空值回落 nil。
	if got := ParseComplaintKeywords("", nil); got != nil {
		t.Fatalf("空词表应返回 nil，实际 %v", got)
	}
	// 仅空白同样回落 nil。
	if got := ParseComplaintKeywords("   ；  ", nil); got != nil {
		t.Fatalf("纯空白词表应返回 nil，实际 %v", got)
	}
	// 非法正则被忽略，合法项保留；同时验证半角与全角分隔符、首尾空白。
	got := ParseComplaintKeywords(" 货不对板 ; [非法( ; 维权补 , 包退 ", nil)
	// want 是期望保留的合法正则数量（货不对板、维权补、包退 三项，[非法( 被忽略）。
	want := 3
	if len(got) != want {
		t.Fatalf("非法正则应被忽略，保留 %d 项，实际 %d 项: %v", want, len(got), got)
	}
	// re 表示当前遍历到的已编译正则，必须非 nil 且能匹配原关键词。
	for _, re := range got {
		if re == nil {
			t.Fatal("不应出现 nil 正则")
		}
	}
	// 两个分隔符之间夹纯空白段（空格非分隔符）会得到空项，应被跳过不计入结果。
	if empty := ParseComplaintKeywords("a; ;b", nil); len(empty) != 2 {
		t.Fatalf("分隔符间纯空白段应跳过，期望 2 项，实际 %d 项: %v", len(empty), empty)
	}
}

// TestClassifyIntentWithKeywordsUnion 验证扩展词表与内置取并集而非替换：内置仍生效、扩展新增也生效。
func TestClassifyIntentWithKeywordsUnion(t *testing.T) {
	// extra 是运维新增的投诉说法（货不对板）。
	extra := ParseComplaintKeywords("货不对板", nil)
	// 内置说法仍应命中负向意图（证明内置未被清空）。
	if got := classifyIntentWithKeywords("我要退款", extra); len(got) == 0 || got[0] != IntentComplaint {
		t.Fatalf("内置词表应仍生效，实际 %v", got)
	}
	// 扩展说法也应命中负向意图。
	if got := classifyIntentWithKeywords("这东西货不对板", extra); len(got) == 0 || got[0] != IntentComplaint {
		t.Fatalf("扩展词表应生效，实际 %v", got)
	}
	// 扩展词表不得误伤正常砍价：含砍价表达且命中扩展投诉时应被负向短路，而非当作砍价放行。
	if got := classifyIntentWithKeywords("货不对板，再便宜点", extra); len(got) != 1 || got[0] != IntentComplaint {
		t.Fatalf("投诉+砍价应被负向短路为 complaint，实际 %v", got)
	}
}

// TestClassifyIntentWithKeywordsEmptyFallsBack 验证空扩展词表等价于内置分类，行为不回退。
func TestClassifyIntentWithKeywordsEmptyFallsBack(t *testing.T) {
	// 空扩展应与原 classifyIntent 行为完全一致。
	if got := classifyIntentWithKeywords("便宜点", nil); !reflect.DeepEqual(got, classifyIntent("便宜点")) {
		t.Fatalf("空扩展词表应等价内置，实际 %v", got)
	}
}

// TestPrimaryIntentLabel 验证命中集合到历史标签的收敛语义：零命中/单命中/多命中的取值。
func TestPrimaryIntentLabel(t *testing.T) {
	// 零命中归并到 chitchat（旧值 "chat" 的更名）。
	if got := primaryIntentLabel(nil); got != IntentChitchat {
		t.Fatalf("零命中应为 chitchat，实际 %q", got)
	}
	// 单一命中直接写该意图。
	if got := primaryIntentLabel([]string{IntentBargain}); got != IntentBargain {
		t.Fatalf("单一命中应写该意图，实际 %q", got)
	}
	// 多命中归并到 ambiguous。
	if got := primaryIntentLabel([]string{IntentBargain, IntentOrder}); got != IntentAmbiguous {
		t.Fatalf("多命中应为 ambiguous，实际 %q", got)
	}
}
