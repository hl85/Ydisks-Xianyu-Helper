package adapter

import (
	"context"
	"errors"
	"testing"

	"xianyu-go/internal/xianyu/mtop"
)

// TestSessionRecoveryHandlerFiltersNonExpiredErrors 验证普通平台错误不会触发账号恢复。
func TestSessionRecoveryHandlerFiltersNonExpiredErrors(t *testing.T) {
	// calls 记录恢复端口被调用次数。
	calls := 0
	// handler 是绑定测试恢复端口的会话恢复适配器。
	handler := NewSessionRecoveryHandler(nil, func(context.Context, string) bool {
		calls++
		return true
	})
	// recovered 表示普通错误的恢复结果。
	recovered := handler(context.Background(), "acc1", errors.New("ordinary failure"))
	if recovered || calls != 0 {
		t.Fatalf("普通错误不应触发恢复: recovered=%v calls=%d", recovered, calls)
	}
}

// TestSessionRecoveryHandlerDelegatesExpiredErrors 验证 Session 失效只触发一次恢复端口。
func TestSessionRecoveryHandlerDelegatesExpiredErrors(t *testing.T) {
	// calls 记录恢复端口被调用次数。
	calls := 0
	// handler 是绑定测试恢复端口的会话恢复适配器。
	handler := NewSessionRecoveryHandler(nil, func(_ context.Context, accountID string) bool {
		if accountID != "acc1" {
			t.Fatalf("恢复账号错误: %q", accountID)
		}
		calls++
		return true
	})
	// recovered 表示已识别 Session 失效后的恢复结果。
	recovered := handler(context.Background(), "acc1", errors.New("FAIL_SYS_SESSION_EXPIRED"))
	if !recovered || calls != 1 {
		t.Fatalf("Session 失效未正确委托: recovered=%v calls=%d", recovered, calls)
	}
}

// TestSessionRecoveryHandlerDelegatesMTopTokenErrors 验证仅 MTOP Token 失效也进入统一凭证恢复端口。
func TestSessionRecoveryHandlerDelegatesMTopTokenErrors(t *testing.T) {
	// calls 记录 Token 失效触发恢复端口的次数。
	calls := 0
	// handler 是绑定测试恢复端口的凭证恢复适配器。
	handler := NewSessionRecoveryHandler(nil, func(context.Context, string) bool {
		calls++
		return true
	})
	// tokenErr 是平台明确返回的仅 MTOP Token 失效错误。
	tokenErr := &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200}
	// recovered 表示 Token 失效后的凭证恢复结果。
	recovered := handler(context.Background(), "acc1", tokenErr)
	if !recovered || calls != 1 {
		t.Fatalf("MTOP Token 失效未正确委托: recovered=%v calls=%d", recovered, calls)
	}
}
