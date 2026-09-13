// reply_review_test 覆盖 AI 回复人工确认模式（review_mode）：
// 开启时拦截发送并通知恰好一次、关闭时正常发送、非 AI 来源不受影响、
// 设置读取失败或取值无法识别时按关闭处理、通知摘要截断。

package engine

import (
	"context"
	"strings"
	"testing"
)

// fakeReviewNotifier 记录 AI 回复人工确认通知的调用与参数，用于断言通知行为。
type fakeReviewNotifier struct {
	calls     int    // 收到的通知总次数
	cookieID  string // 最近一次通知的账号 ID
	eventType string // 最近一次通知的事件类型
	level     string // 最近一次通知的级别
	title     string // 最近一次通知的标题
	body      string // 最近一次通知的正文
}

// NotifyAccountEvent 封装通知账号事件业务协调。
func (f *fakeReviewNotifier) NotifyAccountEvent(cookieID, eventType, level, title, body string) {
	f.calls++
	f.cookieID, f.eventType, f.level, f.title, f.body = cookieID, eventType, level, title, body
}

// TestReplyReview_InterceptsAIReply 验证 review 模式开启时 AI 回复不发送且通知恰好一次。
func TestReplyReview_InterceptsAIReply(t *testing.T) {
	// s、cleanup 是测试用的数据库存储及其清理函数。
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是测试用上下文。
	ctx := context.Background()
	// 写入开启人工确认模式的系统设置。
	// err 是设置写入失败原因。
	if err := s.Settings.Set(ctx, "ai_reply_review_mode", "1"); err != nil {
		t.Fatalf("写入设置: %v", err)
	}
	// notifier 记录人工确认通知调用。
	notifier := &fakeReviewNotifier{}
	// sender 记录回复投递。
	sender := &recordingSender{}
	// ai 返回固定的 AI 回复草稿。
	ai := &fakeAIReplier{result: &ReplyResult{Text: "AI 草稿内容", Source: "AI"}}
	// r 是被测回复服务。
	r := NewReplyService("cid", s, sender, nil, ai, nil, notifier)
	// err 是回复处理失败原因。
	if err := r.Handle(ctx, chatMsg("能便宜点吗", "item1", "chat1")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 0 || len(sender.images) != 0 {
		t.Fatalf("review 开启时不应发送任何回复，got texts=%d images=%d", len(sender.texts), len(sender.images))
	}
	if notifier.calls != 1 {
		t.Fatalf("应恰好通知 1 次，got %d", notifier.calls)
	}
	if notifier.cookieID != "cid" || notifier.eventType != "ai_reply_review" || notifier.level != "info" {
		t.Errorf("通知参数不符: cookieID=%q eventType=%q level=%q", notifier.cookieID, notifier.eventType, notifier.level)
	}
	if notifier.title != "AI 回复待人工确认" {
		t.Errorf("通知标题不符: %q", notifier.title)
	}
	if !strings.Contains(notifier.body, "能便宜点吗") || !strings.Contains(notifier.body, "AI 草稿内容") {
		t.Errorf("通知正文应含买家消息与 AI 草稿摘要: %q", notifier.body)
	}
}

// TestReplyReview_DisabledSendsNormally 验证 review 模式未开启时 AI 回复正常发送且不通知。
func TestReplyReview_DisabledSendsNormally(t *testing.T) {
	// s、cleanup 是测试用的数据库存储及其清理函数。
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是测试用上下文。
	ctx := context.Background()
	// notifier 记录人工确认通知调用。
	notifier := &fakeReviewNotifier{}
	// sender 记录回复投递。
	sender := &recordingSender{}
	// ai 返回固定的 AI 回复草稿。
	ai := &fakeAIReplier{result: &ReplyResult{Text: "AI 草稿内容", Source: "AI"}}
	// r 是被测回复服务，未写入设置即默认关闭。
	r := NewReplyService("cid", s, sender, nil, ai, nil, notifier)
	// err 是回复处理失败原因。
	if err := r.Handle(ctx, chatMsg("在吗", "item1", "chat1")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 1 || sender.texts[0].text != "AI 草稿内容" {
		t.Fatalf("review 关闭时应正常发送，got %+v", sender.texts)
	}
	if notifier.calls != 0 {
		t.Errorf("review 关闭时不应通知，got %d", notifier.calls)
	}
}

// TestReplyReview_KeywordSourceUnaffected 验证 review 模式开启时关键词回复照常发送且不通知。
func TestReplyReview_KeywordSourceUnaffected(t *testing.T) {
	// s、cleanup 是测试用的数据库存储及其清理函数。
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是测试用上下文。
	ctx := context.Background()
	// err 是设置写入失败原因。
	if err := s.Settings.Set(ctx, "ai_reply_review_mode", "1"); err != nil {
		t.Fatalf("写入设置: %v", err)
	}
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','在吗','在的老板','text')`)
	// notifier 记录人工确认通知调用。
	notifier := &fakeReviewNotifier{}
	// sender 记录回复投递。
	sender := &recordingSender{}
	// r 是被测回复服务，AI 与 API 回复均未注入。
	r := NewReplyService("cid", s, sender, nil, nil, nil, notifier)
	// err 是回复处理失败原因。
	if err := r.Handle(ctx, chatMsg("老板在吗", "item1", "chat1")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 1 || sender.texts[0].text != "在的老板" {
		t.Fatalf("关键词回复不应被 review 拦截，got %+v", sender.texts)
	}
	if notifier.calls != 0 {
		t.Errorf("非 AI 来源不应通知，got %d", notifier.calls)
	}
}

// TestReplyReview_SettingReadFailureTreatedAsOff 验证设置读取失败时按关闭处理、正常发送。
func TestReplyReview_SettingReadFailureTreatedAsOff(t *testing.T) {
	// s、cleanup 是测试用的数据库存储及其清理函数。
	s, cleanup := newReplyStore(t)
	// ctx 是测试用上下文。
	ctx := context.Background()
	// err 是设置写入失败原因。
	if err := s.Settings.Set(ctx, "ai_reply_review_mode", "1"); err != nil {
		t.Fatalf("写入设置: %v", err)
	}
	// 先写入开启设置，再关闭底层连接模拟存储读取失败。
	cleanup()
	// notifier 记录人工确认通知调用。
	notifier := &fakeReviewNotifier{}
	// sender 记录回复投递。
	sender := &recordingSender{}
	// ai 返回固定的 AI 回复草稿。
	ai := &fakeAIReplier{result: &ReplyResult{Text: "AI 草稿内容", Source: "AI"}}
	// r 是被测回复服务。
	r := NewReplyService("cid", s, sender, nil, ai, nil, notifier)
	// err 是回复处理失败原因。
	if err := r.Handle(ctx, chatMsg("在吗", "item1", "chat1")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 1 {
		t.Fatalf("设置读取失败应按关闭处理并正常发送，got %+v", sender.texts)
	}
	if notifier.calls != 0 {
		t.Errorf("设置读取失败不应通知，got %d", notifier.calls)
	}
}

// TestReplyReview_UnknownSettingValueTreatedAsOff 验证设置取值无法识别时按关闭处理。
func TestReplyReview_UnknownSettingValueTreatedAsOff(t *testing.T) {
	// s、cleanup 是测试用的数据库存储及其清理函数。
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是测试用上下文。
	ctx := context.Background()
	// err 是设置写入失败原因。
	if err := s.Settings.Set(ctx, "ai_reply_review_mode", "sometimes"); err != nil {
		t.Fatalf("写入设置: %v", err)
	}
	// notifier 记录人工确认通知调用。
	notifier := &fakeReviewNotifier{}
	// sender 记录回复投递。
	sender := &recordingSender{}
	// ai 返回固定的 AI 回复草稿。
	ai := &fakeAIReplier{result: &ReplyResult{Text: "AI 草稿内容", Source: "AI"}}
	// r 是被测回复服务。
	r := NewReplyService("cid", s, sender, nil, ai, nil, notifier)
	// err 是回复处理失败原因。
	if err := r.Handle(ctx, chatMsg("在吗", "item1", "chat1")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 1 {
		t.Fatalf("无法识别的取值应按关闭处理并正常发送，got %+v", sender.texts)
	}
	if notifier.calls != 0 {
		t.Errorf("无法识别的取值不应通知，got %d", notifier.calls)
	}
}

// TestTruncateReviewSummary 验证通知摘要按字符数截断并压平换行。
func TestTruncateReviewSummary(t *testing.T) {
	// short 是未超长的普通文本，应原样返回。
	short := "你好，在的"
	// got 是短文本的摘要结果。
	got := truncateReviewSummary(short)
	if got != short {
		t.Errorf("短文本不应变化，got %q", got)
	}
	// long 是超长文本，应被截断并以省略号结尾。
	long := strings.Repeat("价", reviewNoticeSummaryLimit+10)
	// got 重新赋值为超长文本的摘要。
	got = truncateReviewSummary(long)
	if len([]rune(got)) != reviewNoticeSummaryLimit+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("超长文本应截断为 %d 字符加省略号，got %d 字符", reviewNoticeSummaryLimit, len([]rune(got)))
	}
	// multiline 是含换行的文本，换行应压成空格。
	multiline := "第一行\n第二行"
	// flatGot 是多行文本的摘要结果。
	flatGot := truncateReviewSummary(multiline)
	if strings.Contains(flatGot, "\n") {
		t.Errorf("摘要不应含换行，got %q", flatGot)
	}
}
