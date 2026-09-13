package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"xianyu-go/internal/automation"
	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/ws"
)

// fakeAPIReplier 可控的 API 回复 mock：返回预设结果或错误。
type fakeAPIReplier struct {
	result *ReplyResult
	err    error
	called int
}

// Reply 封装回复业务协调。
func (f *fakeAPIReplier) Reply(_ context.Context, _ ChatMessage) (*ReplyResult, error) {
	f.called++
	return f.result, f.err
}

// fakeAIReplier 可控的 AI 回复 mock。
type fakeAIReplier struct {
	result *ReplyResult
	err    error
	called int
}

// Reply 封装回复业务协调。
func (f *fakeAIReplier) Reply(_ context.Context, _ ChatMessage) (*ReplyResult, error) {
	f.called++
	return f.result, f.err
}

// recordingSender 记录发送的文本/图片，用于断言回复投递。
type recordingSender struct {
	texts    []textSent
	images   []imageSent
	textErr  error
	imageErr error
	// textCalls 统计文本发送尝试次数，包含返回错误的传输调用。
	textCalls int
	// beforeTextError 在文本发送返回错误前执行，用于模拟发送过程中取消请求上下文。
	beforeTextError func()
}

// textSent 用于本次流程后续判断的文本Sent
type textSent struct {
	chatID, toUserID, text string
}

// imageSent 用于本次流程后续判断的图片Sent
type imageSent struct {
	chatID, toUserID, url string
	cardID                int64
}

// SendText 封装Send文本业务协调。
func (r *recordingSender) SendText(_ context.Context, chatID, toUserID, text string) error {
	r.textCalls++
	if r.beforeTextError != nil {
		r.beforeTextError()
	}
	if r.textErr != nil {
		return r.textErr
	}
	r.texts = append(r.texts, textSent{chatID, toUserID, text})
	return nil
}

// TestReplyOnceMarksDefiniteFailureWithIndependentContext 验证取消发送上下文时确定未发送状态仍可被领取重试。
func TestReplyOnceMarksDefiniteFailureWithIndependentContext(t *testing.T) {
	// store、cleanup 保存隔离数据库及关闭责任。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// setupErr 保存启用一次性默认回复的配置写入错误。
	if setupErr := store.DefaultReps.Upsert(context.Background(), "cid", db.DefaultReply{Enabled: true, ReplyOnce: true, ReplyContent: "欢迎"}); setupErr != nil {
		t.Fatal(setupErr)
	}
	// requestCtx、cancel 保存会在发送失败期间被取消的原始请求上下文。
	requestCtx, cancel := context.WithCancel(context.Background())
	// sender 模拟确定未发送错误，并在返回前取消原始请求上下文。
	sender := &recordingSender{textErr: automation.ErrMessageNotSent, beforeTextError: cancel}
	// service 使用真实状态仓储验证失败状态可恢复。
	service := NewReplyService("cid", store, sender, nil, nil, nil)
	// sendErr 保存确定未发送错误的回复结果。
	if sendErr := service.Handle(requestCtx, chatMsg("你好", "", "chat-definite-failure")); !errors.Is(sendErr, automation.ErrMessageNotSent) {
		t.Fatalf("确定未发送错误未透传: %v", sendErr)
	}
	// record、recordErr 保存第一次失败后的状态。
	record, recordErr := store.DefaultReps.Record(context.Background(), "cid", "chat-definite-failure")
	if recordErr != nil || record.Status != "failed" {
		t.Fatalf("取消上下文不应遗留 sending 状态 record=%+v err=%v", record, recordErr)
	}
	// sender 恢复成功发送，验证 failed 记录可被下一次请求重新领取。
	sender.textErr = nil
	// retryErr 保存重新领取失败状态后的回复结果。
	if retryErr := service.Handle(context.Background(), chatMsg("还在吗", "", "chat-definite-failure")); retryErr != nil || sender.textCalls != 2 {
		t.Fatalf("失败状态无法重新领取 retryErr=%v calls=%d", retryErr, sender.textCalls)
	}
}

// SendImage 封装Send图片业务协调。
func (r *recordingSender) SendImage(_ context.Context, chatID, toUserID, url string, cardID int64, _, _ int) error {
	if r.imageErr != nil {
		return r.imageErr
	}
	r.images = append(r.images, imageSent{chatID, toUserID, url, cardID})
	return nil
}

// TestAIQuoteSavedOnlyAfterTextDelivery 验证 AI 报价只有在回复发送成功后才成为可执行报价。
func TestAIQuoteSavedOnlyAfterTextDelivery(t *testing.T) {
	// store、cleanup 是回复链测试仓储及清理函数。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是回复发送与报价领取共用的测试上下文。
	ctx := context.Background()
	// result 是模拟 AI 已给出 9.90 元有效报价的回复结果。
	result := &ReplyResult{Text: "可以，9.90 元成交", AutoPriceQuote: &AIPriceQuoteProposal{PriceCents: 990}}
	// failedSender 模拟文本没有成功交给买家。
	failedSender := &recordingSender{textErr: errors.New("send failed")}
	// failedService 是注入发送失败替身的 AI 回复链。
	failedService := NewReplyService("cid", store, failedSender, nil, &fakeAIReplier{result: result}, nil)
	// err 是模拟发送失败时必须向调用方返回的错误。
	if err := failedService.Handle(ctx, chatMsg("能便宜吗", "item-1", "chat-1")); err == nil {
		t.Fatal("发送失败应返回错误")
	}
	// failedQuote 是发送失败后尝试领取的报价，必须为空。
	failedQuote, err := store.AIReply.ClaimPendingQuote(ctx, "cid", "chat-1", "buyer1", "item-1", "order-failed", time.Now().Unix())
	if err != nil || failedQuote != nil {
		t.Fatalf("发送失败不应保存报价: quote=%+v err=%v", failedQuote, err)
	}
	// successService 是文本发送成功的 AI 回复链。
	successService := NewReplyService("cid", store, &recordingSender{}, nil, &fakeAIReplier{result: result}, nil)
	if err = successService.Handle(ctx, chatMsg("能便宜吗", "item-1", "chat-1")); err != nil {
		t.Fatal(err)
	}
	// successQuote 是发送成功后与订单事实匹配的可执行报价。
	successQuote, err := store.AIReply.ClaimPendingQuote(ctx, "cid", "chat-1", "buyer1", "item-1", "order-success", time.Now().Unix())
	if err != nil || successQuote == nil || successQuote.PriceCents != 990 {
		t.Fatalf("发送成功应保存报价: quote=%+v err=%v", successQuote, err)
	}
}

// TestReplyOnceRetriesOnlyFailedParts 封装Test回复OnceRetriesOnly失败Parts业务协调。
func TestReplyOnceRetriesOnlyFailedParts(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO default_replies
		(cookie_id,enabled,reply_content,reply_image_url,reply_once)
		VALUES ('cid',1,'文字','http://img/retry.png',1)`)

	// textFailure 用于本次流程后续判断的文本Failure
	textFailure := errors.New("text failed")
	// firstSender 用于本次流程后续判断的firstSender
	firstSender := &recordingSender{textErr: textFailure}
	// service 用于本次流程后续判断的service
	service := NewReplyService("cid", s, firstSender, nil, nil, nil)
	if // err 用于本次流程后续判断的err
	err := service.Handle(ctx, chatMsg("在吗", "", "chat-retry")); !errors.Is(err, textFailure) {
		t.Fatalf("first error=%v want text failure", err)
	}
	if len(firstSender.images) != 1 || len(firstSender.texts) != 0 {
		t.Fatalf("first delivery images=%+v texts=%+v", firstSender.images, firstSender.texts)
	}
	// record、err 用于本次流程后续判断的record、err
	record, err := s.DefaultReps.Record(ctx, "cid", "chat-retry")
	if err != nil || record.Status != "failed" || !record.ImageSent || record.TextSent {
		t.Fatalf("failed record=%+v err=%v", record, err)
	}

	// secondSender 用于本次流程后续判断的secondSender
	secondSender := &recordingSender{}
	service = NewReplyService("cid", s, secondSender, nil, nil, nil)
	if // err 用于本次流程后续判断的err
	err := service.Handle(ctx, chatMsg("再问", "", "chat-retry")); err != nil {
		t.Fatal(err)
	}
	if len(secondSender.images) != 0 || len(secondSender.texts) != 1 {
		t.Fatalf("retry should send text only: images=%+v texts=%+v", secondSender.images, secondSender.texts)
	}
	record, err = s.DefaultReps.Record(ctx, "cid", "chat-retry")
	if err != nil || record.Status != "sent" || !record.ImageSent || !record.TextSent {
		t.Fatalf("sent record=%+v err=%v", record, err)
	}
}

// TestReplyOnceQuarantinesUncertainSend 验证平台可能已送达时不允许一次性默认回复自动重发。
func TestReplyOnceQuarantinesUncertainSend(t *testing.T) {
	// store、cleanup 保存隔离数据库及其关闭责任。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是默认回复配置和发送状态读写使用的无截止上下文。
	ctx := context.Background()
	// setupErr 保存启用一次性默认回复的配置写入错误。
	setupErr := store.DefaultReps.Upsert(ctx, "cid", db.DefaultReply{Enabled: true, ReplyOnce: true, ReplyContent: "欢迎"})
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	// sender 模拟平台连接在发送后断开，无法判断消息是否已经送达。
	sender := &recordingSender{textErr: &ws.SendError{Kind: ws.SendUncertain}}
	// service 使用真实投递记录存储验证不确定结果隔离。
	service := NewReplyService("cid", store, sender, nil, nil, nil)
	// firstErr 保存首次发送返回的不确定错误。
	firstErr := service.Handle(ctx, chatMsg("你好", "", "chat-uncertain"))
	if firstErr == nil {
		t.Fatal("不确定发送结果必须返回调用错误")
	}
	// secondErr 保存同一会话再次触发时的结果；它不应发出第二条消息。
	secondErr := service.Handle(ctx, chatMsg("还在吗", "", "chat-uncertain"))
	if secondErr != nil || sender.textCalls != 1 {
		t.Fatalf("不确定结果后不应重发 secondErr=%v calls=%d", secondErr, sender.textCalls)
	}
	// record、recordErr 保存最终隔离状态及查询错误。
	record, recordErr := store.DefaultReps.Record(ctx, "cid", "chat-uncertain")
	if recordErr != nil || record.Status != "uncertain" {
		t.Fatalf("不确定回复未隔离 record=%+v err=%v", record, recordErr)
	}
}

// TestReplyOnceQuarantinesWhenUncertainStateIsRejected 验证 uncertain 状态被数据库约束拒绝时，降级隔离仍阻止租约重发。
func TestReplyOnceQuarantinesWhenUncertainStateIsRejected(t *testing.T) {
	// store、cleanup 保存隔离数据库及其关闭责任。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是默认回复配置、发送状态和租约更新共用的无截止上下文。
	ctx := context.Background()
	// setupErr 保存启用一次性默认回复的配置写入错误。
	if setupErr := store.DefaultReps.Upsert(ctx, "cid", db.DefaultReply{Enabled: true, ReplyOnce: true, ReplyContent: "欢迎"}); setupErr != nil {
		t.Fatal(setupErr)
	}
	// triggerErr 模拟数据库拒绝直接进入 uncertain 状态的约束错误。
	if _, triggerErr := store.DB.ExecContext(ctx, `CREATE TRIGGER deny_uncertain_status BEFORE UPDATE OF status ON default_reply_records WHEN NEW.status='uncertain' BEGIN SELECT RAISE(FAIL,'fixture rejection'); END`); triggerErr != nil {
		t.Fatal(triggerErr)
	}
	// sender 让平台返回可能已送达的未知结果。
	sender := &recordingSender{textErr: &ws.SendError{Kind: ws.SendUncertain}}
	// service 使用真实投递记录存储验证降级隔离。
	service := NewReplyService("cid", store, sender, nil, nil, nil)
	// firstErr 保存首次发送返回的不确定错误。
	if firstErr := service.Handle(ctx, chatMsg("你好", "", "chat-uncertain-fallback")); firstErr == nil {
		t.Fatal("不确定发送结果必须返回调用错误")
	}
	// expireErr 保存租约到期模拟更新结果。
	if _, expireErr := store.DB.ExecContext(ctx, `UPDATE default_reply_records SET lease_expires_at=0 WHERE cookie_id=? AND chat_id=?`, "cid", "chat-uncertain-fallback"); expireErr != nil {
		t.Fatal(expireErr)
	}
	// secondErr 保存同一会话再次触发时的结果；降级隔离记录不应再次发送。
	secondErr := service.Handle(ctx, chatMsg("还在吗", "", "chat-uncertain-fallback"))
	if secondErr != nil || sender.textCalls != 1 {
		t.Fatalf("降级隔离后不应重发 secondErr=%v calls=%d", secondErr, sender.textCalls)
	}
	// record、recordErr 保存降级隔离后的记录状态及查询错误。
	record, recordErr := store.DefaultReps.Record(ctx, "cid", "chat-uncertain-fallback")
	if recordErr != nil || record.Status != "pending" {
		t.Fatalf("降级隔离记录状态异常 record=%+v err=%v", record, recordErr)
	}
}

// TestReplyOnceDoesNotReclaimWhenUncertainPersistenceFails 验证未知结果的两次状态写入都失败时仍不会自动重发。
func TestReplyOnceDoesNotReclaimWhenUncertainPersistenceFails(t *testing.T) {
	// store、cleanup 保存隔离数据库及其关闭责任。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是默认回复配置、发送状态和租约更新共用的无截止上下文。
	ctx := context.Background()
	// setupErr 保存启用一次性默认回复的配置写入错误。
	if setupErr := store.DefaultReps.Upsert(ctx, "cid", db.DefaultReply{Enabled: true, ReplyOnce: true, ReplyContent: "欢迎"}); setupErr != nil {
		t.Fatal(setupErr)
	}
	// triggerErr 模拟数据库在未知结果写入期间完全拒绝状态更新。
	if _, triggerErr := store.DB.ExecContext(ctx, `CREATE TRIGGER deny_reply_state_updates BEFORE UPDATE ON default_reply_records BEGIN SELECT RAISE(FAIL,'fixture write failure'); END`); triggerErr != nil {
		t.Fatal(triggerErr)
	}
	// sender 模拟平台可能已经送达但连接返回未知结果。
	sender := &recordingSender{textErr: &ws.SendError{Kind: ws.SendUncertain}}
	// service 使用真实投递记录存储验证 sending 状态的持久化保护。
	service := NewReplyService("cid", store, sender, nil, nil, nil)
	// firstErr 保存首次发送返回的不确定错误。
	if firstErr := service.Handle(ctx, chatMsg("你好", "", "chat-uncertain-db-down")); firstErr == nil {
		t.Fatal("不确定发送结果必须返回调用错误")
	}
	// dropErr 恢复租约更新能力，模拟数据库恢复后再次收到同一会话消息。
	if _, dropErr := store.DB.ExecContext(ctx, `DROP TRIGGER deny_reply_state_updates`); dropErr != nil {
		t.Fatal(dropErr)
	}
	// expireErr 模拟原领取租约已过期。
	if _, expireErr := store.DB.ExecContext(ctx, `UPDATE default_reply_records SET lease_expires_at=0 WHERE cookie_id=? AND chat_id=?`, "cid", "chat-uncertain-db-down"); expireErr != nil {
		t.Fatal(expireErr)
	}
	// secondErr 保存数据库恢复后的再次处理结果；sending 记录不得触发第二次外部发送。
	secondErr := service.Handle(ctx, chatMsg("还在吗", "", "chat-uncertain-db-down"))
	if secondErr != nil || sender.textCalls != 1 {
		t.Fatalf("数据库写入失败后不应重发 secondErr=%v calls=%d", secondErr, sender.textCalls)
	}
	// record、recordErr 保存仍需人工核对的发送状态及读取错误。
	record, recordErr := store.DefaultReps.Record(ctx, "cid", "chat-uncertain-db-down")
	if recordErr != nil || record.Status != "sending" {
		t.Fatalf("未知结果状态不应回到可重试 pending record=%+v err=%v", record, recordErr)
	}
}

// TestReply_APIPriorityAndError API 回复命中时优先级最高；API 报错时降级到关键词。
func TestReply_APIPriorityAndError(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','在吗','关键词回复','text')`)

	// API 返回结果 → 用 API。
	api := &fakeAPIReplier{result: &ReplyResult{Text: "API回复"}}
	// r 用于本次流程后续判断的r
	r := NewReplyService("cid", s, nil, api, nil, nil)
	// res 用于本次流程后续判断的响应
	res := r.resolve(ctx, chatMsg("在吗", "", "chat1"))
	if res == nil || res.Source != "API" || res.Text != "API回复" {
		t.Fatalf("API 命中应优先，got %+v", res)
	}

	// API 报错 → 降级到关键词。
	api2 := &fakeAPIReplier{err: errors.New("upstream down")}
	// r2 用于本次流程后续判断的r2
	r2 := NewReplyService("cid", s, nil, api2, nil, nil)
	// res2 用于本次流程后续判断的res2
	res2 := r2.resolve(ctx, chatMsg("在吗", "", "chat1"))
	if res2 == nil || res2.Source != "关键词" || res2.Text != "关键词回复" {
		t.Fatalf("API 报错应降级到关键词，got %+v", res2)
	}

	// API 返回 nil（无回复）→ 降级到关键词。
	api3 := &fakeAPIReplier{result: nil}
	// r3 用于本次流程后续判断的r3
	r3 := NewReplyService("cid", s, nil, api3, nil, nil)
	// res3 用于本次流程后续判断的res3
	res3 := r3.resolve(ctx, chatMsg("在吗", "", "chat1"))
	if res3 == nil || res3.Source != "关键词" {
		t.Fatalf("API nil 应降级到关键词，got %+v", res3)
	}
}

// TestReply_AIPriorityOverDefault AI 回复优先于默认回复；AI 报错降级到默认。
func TestReply_AIPriorityOverDefault(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO default_replies (cookie_id,enabled,reply_content,reply_once) VALUES ('cid',1,'默认回复',0)`)

	// ai 用于本次流程后续判断的人工智能
	ai := &fakeAIReplier{result: &ReplyResult{Text: "AI回复"}}
	// r 用于本次流程后续判断的r
	r := NewReplyService("cid", s, nil, nil, ai, nil)
	// res 用于本次流程后续判断的响应
	res := r.resolve(ctx, chatMsg("复杂问题", "", "chat1"))
	if res == nil || res.Source != "AI" || res.Text != "AI回复" {
		t.Fatalf("AI 命中应优先于默认，got %+v", res)
	}

	// AI 报错 → 降级到默认。
	ai2 := &fakeAIReplier{err: errors.New("model timeout")}
	// r2 用于本次流程后续判断的r2
	r2 := NewReplyService("cid", s, nil, nil, ai2, nil)
	// res2 用于本次流程后续判断的res2
	res2 := r2.resolve(ctx, chatMsg("复杂问题", "", "chat1"))
	if res2 == nil || res2.Source != "默认" || res2.Text != "默认回复" {
		t.Fatalf("AI 报错应降级到默认，got %+v", res2)
	}
}

// TestReply_ImageKeyword 图片类型关键词返回 ImageURL。
func TestReply_ImageKeyword(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,image_url,type) VALUES ('cid','看图','','http://img/x.png','image')`)

	// r 用于本次流程后续判断的r
	r := NewReplyService("cid", s, nil, nil, nil, nil)
	// res 用于本次流程后续判断的响应
	res := r.resolve(ctx, chatMsg("发看图", "item1", "chat1"))
	if res == nil || res.Source != "关键词" || res.ImageURL != "http://img/x.png" {
		t.Fatalf("图片关键词应返回 ImageURL，got %+v", res)
	}
}

// TestReply_HandleSendsImageThenText Handle 先发图片后发文本；Skip 不发送。
func TestReply_HandleSendsImageThenText(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO default_replies (cookie_id,enabled,reply_content,reply_image_url,reply_once) VALUES ('cid',1,'文字','http://img/y.png',0)`)

	// sender 用于本次流程后续判断的sender
	sender := &recordingSender{}
	// r 用于本次流程后续判断的r
	r := NewReplyService("cid", s, sender, nil, nil, nil)
	if // err 用于本次流程后续判断的err
	err := r.Handle(ctx, chatMsg("在吗", "", "chat9")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(sender.images) != 1 || sender.images[0].url != "http://img/y.png" {
		t.Fatalf("应先发图片，got %+v", sender.images)
	}
	if len(sender.texts) != 1 || sender.texts[0].text != "文字" {
		t.Fatalf("再发文本，got %+v", sender.texts)
	}

	// Skip（空默认回复）不应发送任何内容。
	s.DB.ExecContext(ctx, `UPDATE default_replies SET reply_content='', reply_image_url='' WHERE cookie_id='cid'`)
	// sender2 用于本次流程后续判断的sender2
	sender2 := &recordingSender{}
	// r2 用于本次流程后续判断的r2
	r2 := NewReplyService("cid", s, sender2, nil, nil, nil)
	if // err 用于本次流程后续判断的err
	err := r2.Handle(ctx, chatMsg("在吗", "", "chat9")); err != nil {
		t.Fatalf("Handle skip: %v", err)
	}
	if len(sender2.texts) != 0 || len(sender2.images) != 0 {
		t.Fatalf("Skip 不应发送，got texts=%+v images=%+v", sender2.texts, sender2.images)
	}
}

// TestParseMessageIDFromJSON bizTag/extJson 中提取 messageId。
func TestParseMessageIDFromJSON(t *testing.T) {
	// cases 用于本次流程后续判断的cases
	cases := map[string]string{
		`{"messageId":"abc123"}`: "abc123",
		`{"sourceId":"x"}`:       "",
		`not json`:               "",
		`{}`:                     "",
	}
	// in、want 表示当前遍历过程中的in、want
	for in, want := range cases {
		if // got 用于本次流程后续判断的got
		got := parseMessageIDFromJSON(in); got != want {
			t.Errorf("parseMessageIDFromJSON(%q)=%q want %q", in, got, want)
		}
	}
}

// TestExtractMessageID 优先实时消息的 PNM ID，其次兼容 bizTag/extJson，无则空。
func TestExtractMessageID(t *testing.T) {
	if // got 用于本次流程后续判断的got
	got := extractMessageID(map[string]any{
		"1": map[string]any{
			"3": "4263141580162.PNM",
			"10": map[string]any{
				"bizTag":  `{"messageId":"biz-id"}`,
				"extJson": `{"messageId":"ext-id"}`,
			},
		},
	}); got != "4263141580162.PNM" {
		t.Errorf("PNM 优先: got %q", got)
	}
	if // got 用于本次流程后续判断的got
	got := extractMessageID(map[string]any{
		"1": map[string]any{
			"10": map[string]any{
				"extJson": `{"messageId":"ext-id"}`,
			},
		},
	}); got != "ext-id" {
		t.Errorf("extJson 兜底: got %q", got)
	}
	if // got 用于本次流程后续判断的got
	got := extractMessageID(map[string]any{"1": map[string]any{}}); got != "" {
		t.Errorf("无 ID: got %q", got)
	}
}

// TestMessageContentType extJson 优先，其次 m6.3.4，再其次 m6.3.5 内嵌 JSON。
func TestMessageContentType(t *testing.T) {
	// extJson 命中。
	if got := messageContentType(
		map[string]any{},
		map[string]any{"extJson": `{"contentType":"14"}`},
	); got != "14" {
		t.Errorf("extJson: got %q", got)
	}
	// m6.3.4 数字字段（toString 把 float64 转 "26"）。
	if got := messageContentType(
		map[string]any{"6": map[string]any{"3": map[string]any{"4": float64(26)}}},
		map[string]any{},
	); got != "26" {
		t.Errorf("m6.3.4: got %q", got)
	}
	// m6.3.5 内嵌 JSON。
	if got := messageContentType(
		map[string]any{"6": map[string]any{"3": map[string]any{"5": `{"contentType":"14"}`}}},
		map[string]any{},
	); got != "14" {
		t.Errorf("m6.3.5: got %q", got)
	}
	// 都没有 → 空。
	if got := messageContentType(map[string]any{}, map[string]any{}); got != "" {
		t.Errorf("空: got %q", got)
	}
}

// TestIsNonUserChatNotice contentType 14/26 为系统提示，应过滤。
func TestIsNonUserChatNotice(t *testing.T) {
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"extJson": `{"contentType":"14"}`}, "[提示]") {
		t.Error("contentType=14 应判为系统提示")
	}
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"extJson": `{"contentType":"26"}`}, "[卡片]") {
		t.Error("contentType=26 应判为系统提示")
	}
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"extJson": `{"contentType":"25"}`}, "快给ta一个评价吧～") {
		t.Error("contentType=25 评价提醒应判为系统提示")
	}
	if !isNonUserChatNotice(map[string]any{}, map[string]any{}, "快给ta一个评价吧～") {
		t.Error("评价提醒文案即使缺少扩展字段也不应进入聊天回复")
	}
	if isNonUserChatNotice(map[string]any{}, map[string]any{}, "[买家说你好]") {
		t.Error("普通消息不应判为系统提示")
	}
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"sessionType": "24"}, "售后问卷") {
		t.Error("非真人会话不应进入买家聊天列表")
	}
}

// TestIsNonUserChatNoticeFiltersOfficialSenderAndPlaceholder 封装TestIsNon用户聊天NoticeFiltersOfficialSenderAndPlaceholder业务协调。
func TestIsNonUserChatNoticeFiltersOfficialSenderAndPlaceholder(t *testing.T) {
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"senderUserId": "1400", "reminderContent": "邀您填写售后问卷"}, "邀您填写售后问卷") {
		t.Error("闲小蜜消息应判为官方系统消息")
	}
	if !isNonUserChatNotice(map[string]any{}, map[string]any{"senderUserId": "peer-1"}, "发来一条新消息") {
		t.Error("官方通知占位文本不应进入聊天回复")
	}
}

// TestToStringAndTrimFloatInt 数字/字符串安全转换。
func TestToStringAndTrimFloatInt(t *testing.T) {
	if // got 用于本次流程后续判断的got
	got := toString(float64(26)); got != "26" {
		t.Errorf("toString(float64 26)=%q", got)
	}
	if // got 用于本次流程后续判断的got
	got := toString("hello"); got != "hello" {
		t.Errorf("toString(string)=%q", got)
	}
	if // got 用于本次流程后续判断的got
	got := toString(nil); got != "" {
		t.Errorf("toString(nil)=%q", got)
	}
	if // got 用于本次流程后续判断的got
	got := trimFloatInt(12.00); got != "12" {
		t.Errorf("trimFloatInt(12.00)=%q", got)
	}
	if // got 用于本次流程后续判断的got
	got := trimFloatInt(12.50); got != "12.5" {
		t.Errorf("trimFloatInt(12.50)=%q", got)
	}
}

// TestContains 大小写不敏感包含。
func TestContains(t *testing.T) {
	if !contains("Hello World", "world") {
		t.Error("应大小写不敏感命中")
	}
	if contains("Hello", "xyz") {
		t.Error("不应误命中")
	}
}

// 编译期保证 db 包被引用（测试构造 store 时使用）。
var _ = db.DialectSQLite
