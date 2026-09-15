package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/ws"
)

// TestIsTransientDialErrorClassifiesTransportFailures 验证拨号阶段错误被正确区分为
// 「可退避重连的瞬时网络故障」与「必须重新登录的凭证问题」。
//
// 回归背景：2026-09-15 07:11 一次 Docker 内置 DNS 瞬时故障（server misbehaving）导致
// WebSocket 握手失败，账号运行被直接终止且不再重试，停摆 1 小时 40 分钟。核心原因就是
// 拨号阶段的传输层错误没有被识别为可重试，因此这里把该真实样本固化为回归用例。
func TestIsTransientDialErrorClassifiesTransportFailures(t *testing.T) {
	// cases 保存每种拨号错误及其可重试判定。
	cases := []struct {
		name      string
		err       error
		transient bool
	}{
		{
			name:      "真实故障-docker内置DNS抖动",
			err:       errors.New(`failed to WebSocket dial: failed to send handshake request: Get "https://wss-goofish.dingtalk.com:443": dial tcp: lookup wss-goofish.dingtalk.com on 127.0.0.11:53: server misbehaving`),
			transient: true,
		},
		{name: "dns-解析不到主机", err: errors.New("dial tcp: lookup wss-goofish.dingtalk.com: no such host"), transient: true},
		{name: "dns-解析器临时故障", err: errors.New("lookup wss-goofish.dingtalk.com: Temporary failure in name resolution"), transient: true},
		{name: "网络不可达", err: errors.New("dial tcp 1.2.3.4:443: connect: network is unreachable"), transient: true},
		{name: "连接被拒", err: errors.New("dial tcp 127.0.0.1:443: connect: connection refused"), transient: true},
		{name: "连接被重置", err: errors.New("read tcp: connection reset by peer"), transient: true},
		{name: "拨号超时", err: errors.New("dial tcp 1.2.3.4:443: i/o timeout"), transient: true},
		{name: "tls握手超时", err: errors.New("net/http: TLS handshake timeout"), transient: true},
		{name: "握手报文中断", err: errors.New("failed to send handshake request: unexpected EOF"), transient: true},
		{name: "凭证被拒-非瞬时", err: &ws.RegError{Kind: ws.RegErrorInvalidToken, Reason: "invalid token"}, transient: false},
		{name: "认证失败-非瞬时", err: &ws.RegError{Kind: ws.RegErrorAuthentication, Reason: "not auth"}, transient: false},
		{name: "会话被移除-非瞬时", err: &ws.RegError{Kind: ws.RegErrorConnectLimit, Reason: "session remove"}, transient: false},
		{name: "普通业务错误-非瞬时", err: errors.New("dial failed"), transient: false},
		{name: "空错误-非瞬时", err: nil, transient: false},
	}
	// testCase 表示当前子测试使用的拨号错误样本。
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// got 是本次可重试判定结果。
			if got := isTransientDialError(testCase.err); got != testCase.transient {
				t.Fatalf("isTransientDialError(%v) = %v, 期望 %v", testCase.err, got, testCase.transient)
			}
		})
	}
}

// TestIsTransientDialErrorPrefersAuthenticationRejection 验证认证类错误优先于文本匹配：
// 即使服务端拒绝原因里恰好含网络字样，也必须走重新登录而不是无限重试。
func TestIsTransientDialErrorPrefersAuthenticationRejection(t *testing.T) {
	// err 是拒绝原因中带 timeout 字样的凭证错误，用于确认认证判定优先。
	err := &ws.RegError{Kind: ws.RegErrorInvalidToken, Reason: "token timeout"}
	if isTransientDialError(err) {
		t.Fatal("认证类错误不得被判定为可重试的瞬时网络故障")
	}
}

// TestRecordNetworkFailureDrivesBackoff 验证网络失败计数递增会推动退避时长增长。
func TestRecordNetworkFailureDrivesBackoff(t *testing.T) {
	// account 是仅用于验证退避阶梯的本地账号。
	account := New(Config{CookieID: "backoff", CookieStr: "unb=1"})
	// first 是首次失败后的退避时长。
	first := account.networkRetryDelay()
	account.recordNetworkFailure()
	// second 是递增一次后的退避时长。
	second := account.networkRetryDelay()
	if second <= first {
		t.Fatalf("网络失败计数递增后退避应增长: first=%v second=%v", first, second)
	}
	if account.networkFailures != 1 {
		t.Fatalf("networkFailures 期望 1，实际 %d", account.networkFailures)
	}
	// 连续递增后仍必须被上限约束，避免永久失去重连机会。
	for i := 0; i < 20; i++ {
		account.recordNetworkFailure()
	}
	// cappedDelay 是连续失败后的退避时长，必须仍被上限约束。
	if cappedDelay := account.networkRetryDelay(); cappedDelay > 75*time.Second {
		t.Fatalf("退避时长应被上限约束，实际 %v", cappedDelay)
	}
}

// TestTransientDialErrorMatchedBeforeTerminalHandling 验证瞬时网络故障会在终止处理之前被分流：
// 同一错误若走终止路径会返回非 nil 并结束账号运行，这正是本次停摆的成因。
func TestTransientDialErrorMatchedBeforeTerminalHandling(t *testing.T) {
	// err 是本次线上故障的真实形态。
	err := fmt.Errorf("failed to WebSocket dial: Get %q: dial tcp: lookup wss-goofish.dingtalk.com on 127.0.0.11:53: server misbehaving", "https://wss-goofish.dingtalk.com:443")
	if !isTransientDialError(err) {
		t.Fatal("真实 DNS 故障样本必须被识别为可重试")
	}
	// account 是仅用于对照的分流账号。
	account := New(Config{CookieID: "terminal", CookieStr: "unb=1"})
	// 对照：同一个错误若走终止路径会返回非 nil，调用方随即退出连接循环。
	if terminalErr := account.handleWSConnectFailure(context.Background(), err); terminalErr == nil {
		t.Fatal("对照用例：终止路径应当返回错误")
	}
}
