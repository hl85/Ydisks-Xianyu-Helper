package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
)

// transportReadyCoverageHandler 覆盖传输就绪通知的可选处理器接口。
type transportReadyCoverageHandler struct {
	// Handler 嵌入基础处理器接口，避免本测试重复实现无关回调。
	Handler
	// calls 记录传输就绪通知次数。
	calls int
}

// OnTransportReady 记录账号消息传输已就绪。
func (h *transportReadyCoverageHandler) OnTransportReady(context.Context, string) {
	h.calls++
}

// noRefreshCoverageHandler 覆盖密码登录续期明确失败的 token 错误分支。
type noRefreshCoverageHandler struct {
	// recordingHandler 提供账号运行所需的基础业务回调。
	*recordingHandler
}

// releaseReliabilitySDKOnlyHandler 只改变非签名 Cookie，验证 Token 失效恢复不能因此清零退避。
type releaseReliabilitySDKOnlyHandler struct {
	// recordingHandler 提供账号运行所需的其他测试回调。
	*recordingHandler
	// store 提供本地凭证写回能力。
	store *db.Store
	// refreshCalls 记录即时凭证刷新回调次数。
	refreshCalls int
}

// releaseReliabilityAlwaysRefreshHandler 在仓储读取失败时仍返回成功，验证引擎不会把未知状态当作恢复证据。
type releaseReliabilityAlwaysRefreshHandler struct {
	// recordingHandler 提供账号运行所需的其他测试回调。
	*recordingHandler
	// refreshCalls 记录即时凭证刷新回调次数。
	refreshCalls int
}

// OnPasswordLoginRefresh 返回成功但不写仓储，专门覆盖续期后无法读取凭证的保护分支。
func (h *releaseReliabilityAlwaysRefreshHandler) OnPasswordLoginRefresh(context.Context, string) bool {
	h.refreshCalls++
	return true
}

// OnPasswordLoginRefresh 保留 _m_h5_tk，仅更新 sdkSilent，模拟签名未轮换的假恢复。
func (h *releaseReliabilitySDKOnlyHandler) OnPasswordLoginRefresh(ctx context.Context, accountID string) bool {
	h.refreshCalls++
	// data、err 保存测试账号当前的非敏感凭证视图及读取错误。
	data, err := h.store.Cookies.GetCookieRuntimeData(ctx, accountID)
	if err != nil {
		return false
	}
	return h.store.Cookies.UpdateValueExisting(ctx, accountID, data.Value+"; sdkSilent=9999999999999") == nil
}

// TestReleaseReliabilityTokenRequiresSigningRotation 验证仅更新 sdkSilent 时仍进入可取消退避。
func TestReleaseReliabilityTokenRequiresSigningRotation(t *testing.T) {
	// account、handler、store、cleanup 提供本地 SQLite 凭证和连接协调器。
	account, handler, store, cleanup := newAccountForTest(t)
	defer cleanup()
	// refreshHandler 只改变普通 Cookie，用于验证签名令牌未轮换时的退避。
	refreshHandler := &releaseReliabilitySDKOnlyHandler{recordingHandler: handler, store: store}
	account.handler = refreshHandler
	// ctx、cancel 将正常退避压缩为可观察的取消结果。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// retry、err 保存 Token 失效处理的重连决定和退避退出原因。
	retry, err := (&connectionCoordinator{account: account}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200})
	if retry || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("签名未轮换时不应立即重连: retry=%v err=%v", retry, err)
	}
}

// TestReleaseReliabilityTokenRefreshIsBounded 验证连续 Token 失效不会反复即时刷新并形成热循环。
func TestReleaseReliabilityTokenRefreshIsBounded(t *testing.T) {
	// account、handler、store、cleanup 提供可读取凭证的隔离账号和刷新计数。
	account, handler, store, cleanup := newAccountForTest(t)
	defer cleanup()
	// refreshHandler 记录本轮是否重复调用即时凭证刷新。
	refreshHandler := &releaseReliabilitySDKOnlyHandler{recordingHandler: handler, store: store}
	account.handler = refreshHandler
	// tokenErr 是平台连续拒绝 MTOP 签名 Token 的错误。
	tokenErr := &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200}
	// runFailure 使用短取消窗口执行一次失败处理，避免测试等待真实退避时间。
	runFailure := func() {
		// ctx、cancel 将单次退避等待压缩为可观察的取消结果。
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = (&connectionCoordinator{account: account}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, tokenErr)
	}
	runFailure()
	runFailure()
	if refreshHandler.refreshCalls != 1 {
		t.Fatalf("连续 Token 失效期间即时刷新次数=%d want 1", refreshHandler.refreshCalls)
	}
}

// TestReleaseReliabilityCredentialReadFailureMustBackoff 验证凭证仓储读取失败时不会走无数据库兼容成功分支。
func TestReleaseReliabilityCredentialReadFailureMustBackoff(t *testing.T) {
	// account、handler、store、cleanup 提供可控凭证读取失败的隔离账号。
	account, baseHandler, store, cleanup := newAccountForTest(t)
	defer cleanup()
	// handler 让续期回调返回成功，但数据库已不可读，结果必须仍然退避。
	handler := &releaseReliabilityAlwaysRefreshHandler{recordingHandler: baseHandler}
	account.handler = handler
	_ = store.DB.Close()
	// ctx、cancel 将正常退避压缩为可观察的取消结果。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// tokenErr 是平台明确返回的仅 MTOP Token 失效错误。
	tokenErr := &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200}
	// retry、err 保存读取失败后的重连决定和退避退出原因。
	retry, err := (&connectionCoordinator{account: account}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, tokenErr)
	if retry || !errors.Is(err, context.DeadlineExceeded) || handler.refreshCalls != 1 {
		t.Fatalf("凭证读取失败应退避且只尝试一次续期: retry=%v err=%v refresh=%d", retry, err, handler.refreshCalls)
	}
}

// OnPasswordLoginRefresh 返回续期失败，验证连接协调器的认证终止路径。
func (*noRefreshCoverageHandler) OnPasswordLoginRefresh(context.Context, string) bool {
	return false
}

// TestEngineCoversTokenFailureAndTransportNotification 验证 token 风控、Session 失效和传输就绪通知分支。
func TestEngineCoversTokenFailureAndTransportNotification(t *testing.T) {
	// ctx 是连接错误处理共用的可取消上下文。
	ctx := context.Background()
	// account、cleanup 保存风控错误测试使用的账号及数据库清理责任。
	account, _, _, cleanup := newAccountForTest(t)
	defer cleanup()
	// riskErr 是闲鱼明确要求安全验证的 token 错误。
	riskErr := &mtop.RiskVerificationError{Ret: []string{"FAIL_SYS_USER_VALIDATE"}}
	// riskRetry、riskResult 保存风控错误不得重试的处理结果。
	riskRetry, riskResult := (&connectionCoordinator{account: account}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, riskErr)
	if riskRetry || !errors.Is(riskResult, riskErr) {
		t.Fatalf("风控 token 结果 retry=%v err=%v", riskRetry, riskResult)
	}
	// refreshHandler 记录 Session 失效后应触发的密码登录续期。
	refreshHandler := &recordingHandler{}
	// refreshAccount 是续期成功路径使用的账号。
	refreshAccount := New(Config{CookieID: "cid", CookieStr: "unb=1; _m_h5_tk=tk;", Handler: refreshHandler})
	// sessionRetry、sessionResult 保存续期成功后的连接重试结果。
	sessionRetry, sessionResult := (&connectionCoordinator{account: refreshAccount}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, errors.New("FAIL_SYS_SESSION_EXPIRED"))
	if !sessionRetry || sessionResult != nil || refreshHandler.refresh != 1 {
		t.Fatalf("Session 续期成功结果 retry=%v err=%v refresh=%d", sessionRetry, sessionResult, refreshHandler.refresh)
	}
	// tokenHandler 记录仅 MTOP Token 失效时绕过疲劳窗口执行的登录态续期。
	tokenHandler := &recordingHandler{}
	// tokenAccount 是仅签名 Token 过期恢复路径使用的账号。
	tokenAccount := New(Config{CookieID: "cid", CookieStr: "unb=1; _m_h5_tk=tk;", Handler: tokenHandler})
	// tokenErr 是平台明确返回的仅 MTOP Token 失效错误。
	tokenErr := &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200}
	// tokenRetry、tokenResult 保存 Token 失效恢复后的连接重试结果。
	tokenRetry, tokenResult := (&connectionCoordinator{account: tokenAccount}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, tokenErr)
	if !tokenRetry || tokenResult != nil || tokenHandler.refresh != 1 {
		t.Fatalf("MTOP Token 续期成功结果 retry=%v err=%v refresh=%d", tokenRetry, tokenResult, tokenHandler.refresh)
	}
	// failedHandler 明确拒绝密码登录续期，验证 Session 终止路径。
	failedHandler := &noRefreshCoverageHandler{recordingHandler: &recordingHandler{}}
	// failedAccount 是续期失败路径使用的账号。
	failedAccount := New(Config{CookieID: "cid", CookieStr: "unb=1; _m_h5_tk=tk;", Handler: failedHandler})
	// failedRetry、failedResult 保存续期失败后的终止结果。
	failedRetry, failedResult := (&connectionCoordinator{account: failedAccount}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, errors.New("FAIL_SYS_SESSION_EXPIRED"))
	if failedRetry || !mtop.IsSessionExpiredErr(failedResult) {
		t.Fatalf("Session 续期失败结果 retry=%v err=%v", failedRetry, failedResult)
	}
	// transportHandler 记录可选的传输就绪回调。
	transportHandler := &transportReadyCoverageHandler{}
	// transportAccount 是传输通知测试使用的最小账号。
	transportAccount := New(Config{CookieID: "transport", Handler: transportHandler})
	transportAccount.notifyTransportReady(ctx)
	if transportHandler.calls != 1 {
		t.Fatalf("传输就绪回调次数=%d want 1", transportHandler.calls)
	}
	// noHandlerAccount 验证未实现可选接口时通知入口安全跳过。
	noHandlerAccount := New(Config{CookieID: "transport"})
	noHandlerAccount.notifyTransportReady(ctx)
}

// TestTokenExpiredRefreshWithoutCredentialChangeUsesBackoff 验证续期回调未改变凭证时不会立即重连风暴。
func TestTokenExpiredRefreshWithoutCredentialChangeUsesBackoff(t *testing.T) {
	// account、handler、cleanup 保存带数据库凭证的隔离账号及清理责任。
	account, handler, _, cleanup := newAccountForTest(t)
	defer cleanup()
	// ctx 将正常 Token 退避压缩为可控的取消窗口，避免测试真实等待一分钟。
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// tokenErr 是平台明确返回的仅 MTOP Token 失效错误。
	tokenErr := &mtop.MTopResponseError{API: "token", Kind: mtop.MTopErrorTokenExpired, HTTPStatus: 200}
	// retry、result 保存未变化凭证时的处理结果。
	retry, result := (&connectionCoordinator{account: account}).handleTokenAcquisitionFailure(ctx, &fakeWSConn{}, tokenErr)
	if retry || !errors.Is(result, context.DeadlineExceeded) {
		t.Fatalf("未变化凭证应进入退避并响应取消，retry=%v result=%v", retry, result)
	}
	if handler.refresh != 1 {
		t.Fatalf("应调用一次续期回调，got %d", handler.refresh)
	}
}

// TestEngineCoversDispatcherDefaultsAndCredentialSnapshotGuard 验证消息分发器默认依赖和无数据库凭证快照边界。
func TestEngineCoversDispatcherDefaultsAndCredentialSnapshotGuard(t *testing.T) {
	// dispatcher 保存空配置构造出的消息分发器。
	dispatcher := newMessageDispatcher(messageDispatcherConfig{})
	// loggerMissing、cookie、handler 保存默认依赖的非锁状态摘要，避免复制含互斥锁的分发器。
	loggerMissing := dispatcher.logger == nil
	// cookie 保存默认凭证读取结果，应为空字符串。
	cookie := dispatcher.currentCookie()
	// handler 保存默认消息处理器读取结果，应为空接口。
	handler := dispatcher.currentHandler()
	if loggerMissing || cookie != "" || handler != nil {
		t.Fatalf("空配置默认依赖未正确初始化: loggerMissing=%v cookie=%q handlerNil=%v", loggerMissing, cookie, handler == nil)
	}
	// taskContext、finish、accepted 验证兼容生命周期回退端口可完成一次任务。
	taskContext, finish, accepted := dispatcher.beginTask()
	if taskContext == nil || !accepted {
		t.Fatal("默认生命周期回退端口应接受任务")
	}
	finish()
	// account 是无数据库账号，用于验证凭证快照校验不依赖持久化层。
	account := New(Config{CookieID: "cid", CookieStr: "cookie"})
	if !account.cookieSnapshotMatchesDB(context.Background(), "expected") {
		t.Fatal("无数据库账号应视为凭证快照可用")
	}
	// senderID 是标准展示扩展中的发送者账号。
	senderID := extractChatSenderUserIDFromMaps(nil, map[string]any{"senderUserId": "  user-1 "})
	if senderID != "user-1" {
		t.Fatalf("标准发送者=%q", senderID)
	}
	// fallbackID 是紧凑消息信封中的兼容发送者账号。
	fallbackID := extractChatSenderUserIDFromMaps(map[string]any{"1": map[string]any{"1": "compact-user"}}, nil)
	if fallbackID != "compact-user" {
		t.Fatalf("兼容发送者=%q", fallbackID)
	}
	if extractChatSenderUserIDFromMaps(nil, nil) != "" {
		t.Fatal("缺少发送者字段应返回空字符串")
	}
	// canceledContext 验证 token 失败在关闭上下文下立即收束，不等待退避。
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	// canceledAccount 是关闭上下文错误路径使用的账号。
	canceledAccount := New(Config{CookieID: "cid", CookieStr: "unb=1; _m_h5_tk=tk;"})
	// canceledRetry、canceledErr 保存关闭上下文下的处理结果。
	canceledRetry, canceledErr := (&connectionCoordinator{account: canceledAccount}).handleTokenAcquisitionFailure(canceledContext, &fakeWSConn{}, errors.New("network down"))
	if canceledRetry || !errors.Is(canceledErr, context.Canceled) {
		t.Fatalf("关闭上下文结果 retry=%v err=%v", canceledRetry, canceledErr)
	}
	// timeoutContext 验证普通网络失败会在退避等待被取消时返回超时。
	timeoutContext, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer timeoutCancel()
	// timeoutAccount 是普通网络错误路径使用的账号。
	timeoutAccount := New(Config{CookieID: "cid", CookieStr: "unb=1; _m_h5_tk=tk;"})
	// timeoutRetry、timeoutErr 保存退避等待被上下文打断的结果。
	timeoutRetry, timeoutErr := (&connectionCoordinator{account: timeoutAccount}).handleTokenAcquisitionFailure(timeoutContext, &fakeWSConn{}, errors.New("network down"))
	if timeoutRetry || !errors.Is(timeoutErr, context.DeadlineExceeded) {
		t.Fatalf("退避超时结果 retry=%v err=%v", timeoutRetry, timeoutErr)
	}
}

// TestEngineCoversWSRecorderCallbackAndNestedMessageID 验证报文回调的有界队列语义和消息 ID 递归兼容解析。
func TestEngineCoversWSRecorderCallbackAndNestedMessageID(t *testing.T) {
	// recorder 保存容量为 1 的记录器，用于同时覆盖入队和队列已满丢弃。
	recorder := &wsRecorder{cookieID: "account", logger: slog.Default(), queue: make(chan db.WSMessage, 1)}
	// callback 保存记录器提供的非阻塞报文回调。
	callback := recorder.callback()
	if callback == nil {
		t.Fatal("已装配队列应返回报文回调")
	}
	callback("in", "raw", "{}", "ok", "")
	callback("out", "raw-2", "{}", "ok", "")
	// message 保存队列中首条报文，验证业务字段没有在回调边界丢失。
	message := <-recorder.queue
	if message.CookieID != "account" || message.Direction != "in" || message.RawText != "raw" {
		t.Fatalf("报文回调结果=%+v", message)
	}
	// nestedID 保存从 JSON 字符串嵌套信封中解析出的消息 ID。
	nestedID := findMessageID([]any{map[string]any{"nested": `{"message_id":"nested-id"}`}})
	if nestedID != "nested-id" {
		t.Fatalf("JSON 字符串嵌套 ID=%q", nestedID)
	}
	// directID 保存从对象字段中解析出的直接消息 ID。
	directID := findMessageID(map[string]any{"messageId": " direct-id "})
	if directID != "direct-id" {
		t.Fatalf("直接 ID=%q", directID)
	}
	if findMessageID(`not-json`) != "" || findMessageID(true) != "" {
		t.Fatal("非法或不支持的消息 ID 输入应返回空字符串")
	}
}
