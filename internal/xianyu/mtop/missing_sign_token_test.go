package mtop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsMissingSignTokenErrRecognizesSentinel 验证缺少签名令牌的错误可被稳定识别，
// 且不会被误判为需要账号级凭证恢复的失效。
func TestIsMissingSignTokenErrRecognizesSentinel(t *testing.T) {
	// wrapped 复现调用方实际拿到的错误形态：哨兵被业务上下文包装。
	wrapped := fmt.Errorf("%w，无法调用 %s", ErrMissingSignToken, "mtop.taobao.idle.merchant.rate.list")
	if !IsMissingSignTokenErr(wrapped) {
		t.Fatal("包装后的缺少签名令牌错误必须可被识别")
	}
	if IsMissingSignTokenErr(errors.New("其他错误")) {
		t.Fatal("普通错误不得被识别为缺少签名令牌")
	}
	// 缺少签名令牌只靠刷新令牌接口即可修复，不得进入密码登录级别的凭证恢复。
	if IsCredentialRefreshableErr(wrapped) {
		t.Fatal("缺少签名令牌不应触发账号级凭证恢复")
	}
}

// TestFetchPendingRateOrdersRefreshesMissingSignToken 验证缺失签名令牌时不再直接失败，
// 而是先向令牌接口补齐签名 Cookie 再重试业务请求。
//
// 回归背景：2026-09-15 自动评价因数据库凭证被其他流程覆盖而缺 _m_h5_tk，连续失败 20 分钟，
// 直到一笔无关的发货请求恰好把令牌写回才自愈。根因是缺令牌被当作终态失败直接返回，
// 从未进入已有的令牌刷新分支。
func TestFetchPendingRateOrdersRefreshesMissingSignToken(t *testing.T) {
	// tokenCalls 记录令牌接口被调用的次数，用于证明刷新分支确实被进入。
	tokenCalls := 0
	// rateCalls 记录业务接口被调用的次数，用于证明补齐后确实重试了原请求。
	rateCalls := 0

	// tokenServer 模拟令牌接口：下发新的签名 Cookie。
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Set-Cookie", "_m_h5_tk=refreshed_token_1; Path=/")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":["SUCCESS::调用成功"],"data":{"accessToken":"fresh-token","accessTokenExpiredTime":1789500000}}`))
	}))
	defer tokenServer.Close()

	// rateServer 模拟自动评价列表接口：只要带上签名令牌就返回成功。
	rateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rateCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ret":["SUCCESS::调用成功"],"data":{"module":{"items":[]}}}`))
	}))
	defer rateServer.Close()

	// client 绑定两个本地端点，避免任何真实外网请求。
	client := &ClientImpl{
		HTTPClient:  rateServer.Client(),
		TokenURL:    tokenServer.URL,
		RateListURL: rateServer.URL,
	}
	// cookies 故意不含 _m_h5_tk，复现数据库凭证被覆盖后的真实状态。
	cookies := "unb=11873716; cookie2=abc"

	// _, _ 表示业务结果不做断言：本用例只验证「是否走了补齐路径」。
	_, _ = client.FetchPendingRateOrders(context.Background(), cookies, 1, 50)

	if tokenCalls == 0 {
		t.Fatal("缺失签名令牌时必须先请求令牌接口补齐，而不是直接返回失败")
	}
	if rateCalls == 0 {
		t.Fatal("补齐签名令牌后必须重试原业务请求")
	}
}
