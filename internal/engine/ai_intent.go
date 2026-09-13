// ai_intent.go 买家消息的意图注册表与规则前置分类。
// 设计来源：同类项目把「意图注册表 + 规则前置（不确定就别猜）」作为客服路由的沉淀，
// 此处按本仓库约定用 Go 重写，并叠加一条负向意图规则：投诉/售后类消息永远不交给自动应答。
//
// 当前消费方：AI 回复接管策略 aiShouldHandleIntent。
// 其余意图（order / inquiry / consult）已建模但不改变既有路由，
// 目的是把「哪些消息给 AI」从一条正则升级为显式、可测试的策略，同时为后续按意图分流 prompt 留好接缝。

package engine

import (
	"regexp"
	"strings"
)

// 意图常量集中定义，取值会写入 AI 对话历史的 intent 字段，新增取值需同步前端展示。
const (
	// IntentComplaint 是负向意图：投诉、退款、纠纷类消息，命中即禁止 AI 接管。
	IntentComplaint = "complaint"
	// IntentBargain 是砍价意图，是当前唯一允许 AI 接管的意图。
	IntentBargain = "bargain"
	// IntentOrder 是订单与发货咨询意图。
	IntentOrder = "order"
	// IntentInquiry 是询价与物流政策意图。
	IntentInquiry = "inquiry"
	// IntentConsult 是商品内容咨询意图。
	IntentConsult = "consult"
	// IntentChitchat 是闲聊兜底桶：注册表不为其配置规则，未命中任何规则的默认归入此类。
	IntentChitchat = "chitchat"
)

// aiIntentRule 是一条意图规则。
type aiIntentRule struct {
	// intent 是本规则命中的意图。
	intent string
	// negative 表示负向意图：命中后优先于其它意图生效，用于拦截而不是路由。
	negative bool
	// patterns 是本意图的判定正则集合，任一命中即视为该意图命中。
	patterns []*regexp.Regexp
}

// aiIntentRules 是意图注册表，按优先级从高到低排列。
// 词表刻意保守，只收录表达明确的说法，避免把正常客服对话误判成纠纷。
var aiIntentRules = []aiIntentRule{
	{
		intent:   IntentComplaint,
		negative: true,
		// 负向词表覆盖平台敏感的纠纷表达；命中即拦截，即使消息同时夹带砍价表达。
		patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)退款|退货|投诉|差评|举报|骗子|骗人|假货|被骗|维权`),
		},
	},
	{
		intent: IntentBargain,
		// 复用既有的砍价判定正则，保证 AI 接管范围与历史行为一致。
		patterns: []*regexp.Regexp{bargainMessageRe},
	},
	{
		intent: IntentOrder,
		patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)发货|还没发|提取码|网盘|下载链接|怎么下载`),
		},
	},
	{
		intent: IntentInquiry,
		patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)多少钱|什么价格|怎么卖|包邮`),
		},
	},
	{
		intent: IntentConsult,
		patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)内容|适合|有效|真实|怎么用`),
		},
	},
}

// classifyIntent 对买家消息执行规则前置分类，返回按注册表优先级排序的命中意图。
// 负向意图一旦命中会排在首位并短路后续规则；返回空切片表示没有任何规则命中，
// 调用方应按「不确定就不猜」处理，而不是自行猜测一个意图。
func classifyIntent(text string) []string {
	// lowered 是用于匹配的小写化文本；正则本身已带大小写不敏感标记，这里再统一一次以兼容后续纯字面规则。
	lowered := strings.ToLower(text)
	// hits 是按优先级排列的命中意图。
	hits := make([]string, 0, 2)
	// rule 是当前遍历到的意图规则。
	for _, rule := range aiIntentRules {
		// matched 表示本规则是否有任一正则命中。
		matched := false
		// pattern 是当前遍历到的判定正则。
		for _, pattern := range rule.patterns {
			if pattern.MatchString(lowered) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if rule.negative {
			// 负向意图具有最高优先级：命中即短路，后面的规则不再参与判定。
			return []string{rule.intent}
		}
		hits = append(hits, rule.intent)
	}
	return hits
}

// aiShouldHandleIntent 根据意图命中集合判定是否把消息交给 AI 自动应答。
// 策略：命中砍价（无论是否与其它正向意图同时出现）即接管，保持既有覆盖面；
// 命中负向意图或没有任何命中时不接管，交给默认回复与人工处理。
func aiShouldHandleIntent(hits []string) bool {
	// negative 表示命中集合里是否出现了负向意图；它具有一票否决权。
	negative := false
	// bargain 表示命中集合里是否出现了砍价意图。
	bargain := false
	// intent 是当前遍历到的命中意图。
	for _, intent := range hits {
		switch intent {
		case IntentComplaint:
			negative = true
		case IntentBargain:
			bargain = true
		}
	}
	return bargain && !negative
}
