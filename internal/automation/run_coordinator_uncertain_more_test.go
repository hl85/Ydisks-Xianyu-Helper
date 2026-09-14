package automation

import (
	"testing"

	"xianyu-go/internal/db"
)

// TestCanContinueAfterUncertainAction 验证只有「剩余动作全部为幂等状态动作」时才放行。
// 这条判据是 P1 修复的安全边界：只要还有会再次联系买家的动作未完成，就必须维持原有熔断。
func TestCanContinueAfterUncertainAction(t *testing.T) {
	// cases 覆盖空集合、纯状态动作、以及混入消息动作三类关键边界。
	cases := []struct {
		name      string
		remaining []db.AutomationAction
		want      bool
	}{
		{name: "没有剩余动作", remaining: nil, want: false},
		{name: "空切片", remaining: []db.AutomationAction{}, want: false},
		{name: "仅确认发货", remaining: []db.AutomationAction{{ActionType: ActionConfirmShipment}}, want: true},
		{name: "多个确认发货", remaining: []db.AutomationAction{{ActionType: ActionConfirmShipment}, {ActionType: ActionConfirmShipment}}, want: true},
		{name: "含发卡动作", remaining: []db.AutomationAction{{ActionType: ActionSendCard}}, want: false},
		{name: "含模板动作", remaining: []db.AutomationAction{{ActionType: ActionSendTemplate}}, want: false},
		{name: "含文本动作", remaining: []db.AutomationAction{{ActionType: ActionSendText}}, want: false},
		{name: "状态动作后仍有发卡", remaining: []db.AutomationAction{{ActionType: ActionConfirmShipment}, {ActionType: ActionSendCard}}, want: false},
		{name: "含未知动作", remaining: []db.AutomationAction{{ActionType: "unknown_action"}}, want: false},
	}
	for _, tc := range cases {
		if got := canContinueAfterUncertainAction(tc.remaining); got != tc.want {
			t.Errorf("%s: canContinueAfterUncertainAction=%v want %v", tc.name, got, tc.want)
		}
	}
}

// TestContinueAfterUncertainActionSwitch 验证止血开关默认开启、显式置 0 时关闭。
func TestContinueAfterUncertainActionSwitch(t *testing.T) {
	// 未设置时必须开启，否则本次修复在部署后不会生效。
	t.Setenv(continueUncertainActionEnv, "")
	if !continueAfterUncertainActionEnabled() {
		t.Fatal("默认应开启放行")
	}
	t.Setenv(continueUncertainActionEnv, "1")
	if !continueAfterUncertainActionEnabled() {
		t.Fatal("非 0 值应保持开启")
	}
	t.Setenv(continueUncertainActionEnv, "0")
	if continueAfterUncertainActionEnabled() {
		t.Fatal("显式设为 0 时应关闭放行")
	}
}
