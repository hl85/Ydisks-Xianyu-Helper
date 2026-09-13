// reply_review 实现 AI 回复人工确认模式（review_mode）：
// 开启后 AI 生成的客服回复不再直接发送给买家，而是通过通知器告知卖家人工确认，
// 为 AI 客服输出增加一道安全阀。关键词与默认回复不受影响。

package engine

import (
	"context"
	"fmt"
	"strings"
)

// ReplyReviewNotifier 是 AI 回复人工确认所需的最小通知接口，
// 与 *notify.Notifier 的 NotifyAccountEvent 签名对齐，使其可隐式满足本接口。
type ReplyReviewNotifier interface {
	// NotifyAccountEvent 上报一条账号维度的事件通知。
	NotifyAccountEvent(cookieID, eventType, level, title, body string)
}

// aiReplyReviewSetting 是 AI 回复人工确认模式的系统设置键。
const aiReplyReviewSetting = "ai_reply_review_mode"

// reviewNoticeSummaryLimit 是通知正文中买家消息与 AI 草稿各自的截断长度（字符数），
// 防止长对话把通知撑爆。
const reviewNoticeSummaryLimit = 200

// reviewModeEnabled 读取 AI 回复人工确认开关；
// 设置缺失、存储读取失败或取值无法识别时一律按关闭处理（安全默认为直接发送）。
func (r *ReplyService) reviewModeEnabled(ctx context.Context) bool {
	if r.store == nil || r.store.Settings == nil {
		return false
	}
	// value 是设置项的原始字符串值，可能是 1/true/on 等形式。
	value, err := r.store.Settings.Get(ctx, aiReplyReviewSetting)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// interceptAIReplyForReview 在人工确认模式开启时拦截 AI 生成的回复：
// 记录 Warn 日志并向卖家发送"待人工确认"通知，返回 true 表示已拦截、本次不应发送。
// 关键词与默认回复（Source 非 "AI"）直接放行。
func (r *ReplyService) interceptAIReplyForReview(ctx context.Context, res *ReplyResult, m ChatMessage) bool {
	if res == nil || res.Source != "AI" || !r.reviewModeEnabled(ctx) {
		return false
	}
	r.logger.Warn("AI 回复已拦截待人工确认，本次不自动发送", "chat_id", m.ChatID, "buyer_id", m.SenderUserID)
	if r.reviewNotifier != nil {
		r.reviewNotifier.NotifyAccountEvent(r.cookieID, "ai_reply_review", "info", "AI 回复待人工确认", reviewNoticeBody(m, res))
	}
	return true
}

// reviewNoticeBody 构造人工确认通知的正文：
// 包含买家消息与 AI 草稿摘要（各自截断防超长），
// 仅含会话内容，不包含 Cookie、Token 等敏感凭证值。
func reviewNoticeBody(m ChatMessage, res *ReplyResult) string {
	return fmt.Sprintf("账号 %s 收到买家消息：\n买家：%s\nAI 草稿：%s\n请登录后台确认后手动回复。",
		m.AccountID, truncateReviewSummary(m.Text), truncateReviewSummary(res.Text))
}

// truncateReviewSummary 把摘要文本按 reviewNoticeSummaryLimit 截断，
// 超出部分以省略号结尾；换行压成空格，避免破坏通知排版。
func truncateReviewSummary(text string) string {
	// flat 是把换行压平后的单行摘要，便于通知展示。
	flat := strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
	// runes 以字符为单位统计长度，避免中文按字节截断出现乱码。
	runes := []rune(strings.TrimSpace(flat))
	if len(runes) <= reviewNoticeSummaryLimit {
		return string(runes)
	}
	return string(runes[:reviewNoticeSummaryLimit]) + "…"
}
