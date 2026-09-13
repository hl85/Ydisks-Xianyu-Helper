// reply_safety.go 出站客服文本的安全闸门。
// 在回复真正发给买家之前统一扫描「引导站外联系」与「超出售后承诺」两类风险内容，
// 命中后不裸奔也不静默：改用中性兜底话术替代原文，保证买家仍能得到回应。
// 设计思路来自同类项目把安全过滤做在发送前的沉淀，此处按本仓库约定用 Go 纯函数重写。

package engine

import (
	"os"
	"strings"
)

// 出站安全闸门的常量集中定义，便于审阅与调参。
const (
	// replySafetyDisableEnv 是出站安全闸门的急停开关：显式设为 "0" 时关闭扫描。
	// 保留开关是为了在词表误伤出乎预期时能立刻止血，而不必回滚二进制。
	replySafetyDisableEnv = "XIANYU_REPLY_SAFETY"

	// replySafetyMaxRunes 是单条出站客服文本允许的最大字符数，按 rune 计数以免把多字节字符截成半个。
	replySafetyMaxRunes = 500

	// replySafetyFallbackText 是命中安全规则后替代原文发送的中性话术。
	// 它保证买家仍收到一次有意义的回应，同时不含任何站外引导或超越平台的承诺。
	replySafetyFallbackText = "您好，相关商品的详细信息请以商品页面为准，我会尽快为您确认，感谢理解。"
)

// replySafetyRule 是一条出站安全规则：命中其中任一关键词即判定为触碰了该规则保护的边界。
type replySafetyRule struct {
	// Reason 是命中后写入日志的违规原因，供人工判断模型输出为何被拦截。
	Reason string
	// Keywords 是本规则的关键词集合，按「小写化后的子串包含」匹配。
	Keywords []string
}

// replySafetyRules 是内置的出站安全规则集，分为「引导站外联系」与「超出售后承诺」两类。
// 词表默认值偏保守，只收窄到明确的违规表达，避免误伤正常的产品说明；可按真实语料继续调参。
var replySafetyRules = []replySafetyRule{
	{
		Reason: "引导站外联系方式",
		// 只收录明确的站外导流表达，不收「微信」「QQ」这类单独出现时可能属于正常说明的词。
		Keywords: []string{
			"加微信", "微信号", "微信联系", "微信转账", "加我微",
			"qq号", "加qq", "扣扣号",
			"站外交易", "线下交易", "私下交易", "不走平台", "脱离平台",
		},
	},
	{
		Reason: "超出售后承诺",
		// 收录平台敏感、容易引发纠纷或投诉的绝对化承诺。
		Keywords: []string{
			"保证学会", "包就业", "退款秒到", "秒退款", "保证百分百", "百分之百有效",
		},
	},
}

// replySafetyVerdict 是一次出站文本安全扫描的结论。
type replySafetyVerdict struct {
	// Blocked 表示文本是否命中了任一安全规则。
	Blocked bool
	// Reason 是命中的规则原因；未命中时为空字符串。
	Reason string
	// Matched 是命中的关键词，只用于日志，绝不进入对买家展示的内容。
	Matched string
	// Text 是可以直接发送的最终文本：未命中时保持原文，命中时替换为中性兜底话术，超长时截断。
	Text string
	// Truncated 表示文本是否因超过长度上限而被截断。
	Truncated bool
}

// replySafetyEnabled 返回当前进程是否启用出站安全闸门；显式把环境变量设为 "0" 时关闭。
func replySafetyEnabled() bool {
	return strings.TrimSpace(os.Getenv(replySafetyDisableEnv)) != "0"
}

// scanReplySafety 对一条待发送的客服文本执行安全扫描，并给出可直接发送的最终文本。
// 处理顺序是先判违规、再截断：违规文本整体换成兜底话术，因此无需再关心它有多长。
func scanReplySafety(text string) replySafetyVerdict {
	// verdict 保存本次扫描的结论，默认按「未命中、原文可发」初始化。
	verdict := replySafetyVerdict{Text: text}
	if replySafetyEnabled() {
		// lowered 是用于关键词匹配的小写化文本，兼容大小写混写的英文关键词。
		lowered := strings.ToLower(text)
		// rule 是当前遍历到的安全规则。
		for _, rule := range replySafetyRules {
			// keyword 是当前规则下待匹配的关键词。
			for _, keyword := range rule.Keywords {
				if !strings.Contains(lowered, keyword) {
					continue
				}
				verdict.Blocked = true
				verdict.Reason = rule.Reason
				verdict.Matched = keyword
				verdict.Text = replySafetyFallbackText
				return verdict
			}
		}
	}
	// 闸门被急停时仍保留长度保护，避免把异常长的文本发给买家。
	verdict.Text, verdict.Truncated = truncateReplyText(text, replySafetyMaxRunes)
	return verdict
}

// truncateReplyText 把文本按 rune 截断到上限以内，返回截断后的文本以及是否真的发生了截断。
func truncateReplyText(text string, maxRunes int) (string, bool) {
	// runes 是文本的字符视图，按字符而非字节计数才能避免截坏多字节字符。
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text, false
	}
	return string(runes[:maxRunes]), true
}

// applyReplySafety 在发送前对回复结果执行安全扫描，并把结论回写到结果上。
// 命中违规时替换为兜底话术，同时清空已经不再成立的 AI 自动报价承诺，避免按买家从未见过的价格改价。
func (r *ReplyService) applyReplySafety(res *ReplyResult) replySafetyVerdict {
	// 文本为空（例如纯图片回复）时没有任何可扫描内容，直接跳过。
	if res == nil || res.Text == "" {
		return replySafetyVerdict{}
	}
	// verdict 是本次扫描的结论，仅在文本真的被改动时才需要回写与告警。
	verdict := scanReplySafety(res.Text)
	if !verdict.Blocked && !verdict.Truncated {
		return verdict
	}
	if res.AutoPriceQuote != nil {
		res.AutoPriceQuote = nil
	}
	if verdict.Blocked {
		r.logger.Warn("出站客服文本命中安全闸门，已替换为兜底话术",
			"source", res.Source, "reason", verdict.Reason, "matched", verdict.Matched)
	} else {
		r.logger.Warn("出站客服文本超长已截断", "source", res.Source, "max_runes", replySafetyMaxRunes)
	}
	res.Text = verdict.Text
	return verdict
}
