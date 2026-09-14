// ai_intent.go 买家消息的意图注册表与规则前置分类。
// 设计来源：同类项目把「意图注册表 + 规则前置（不确定就别猜）」作为客服路由的沉淀，
// 此处按本仓库约定用 Go 重写，并叠加一条负向意图规则：投诉/售后类消息永远不交给自动应答。
//
// 当前消费方：AI 回复接管策略 aiShouldHandleIntent。
// 其余意图（order / inquiry / consult）已建模但不改变既有路由，
// 目的是把「哪些消息给 AI」从一条正则升级为显式、可测试的策略，同时为后续按意图分流 prompt 留好接缝。

package engine

import (
	"log/slog"
	"regexp"
	"strings"
)

// 意图常量集中定义，取值会写入 AI 对话历史的 intent 字段。
// 取值语义向后兼容说明：旧逻辑只写 "chat"/"bargain"/"reply" 三值；本注册表将 "chat" 更名为
// "chitchat"（零命中兜底，语义一致），其余沿用；新增 complaint/order/inquiry/consult/ambiguous
// 仅供意图分布观测与后续分流使用，前端/日志展示时不得把未知取值当成错误。
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
	// IntentChitchat 是闲聊兜底桶：注册表不为其配置规则，未命中任何规则的默认归入此类（即旧值 "chat" 的更名）。
	IntentChitchat = "chitchat"
	// IntentAmbiguous 是多规则同时命中时的归并标签：注册表无法确定单一意图，便于后续人工或分流策略复核。
	IntentAmbiguous = "ambiguous"
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

// builtinComplaintPatterns 内置投诉负向词表，刻意保守，只收录平台敏感的明确纠纷表达。
// 命中即拦截，即使消息同时夹带其它意图。运维可通过设置 ai_complaint_keywords 在不发版的情况下扩充，
// 扩充词表与内置词表取并集，永远不会替换或清空内置保护（fail-safe：宁可多拦不可少拦）。
var builtinComplaintPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)退款|退货|投诉|差评|举报|骗子|骗人|假货|被骗|维权`),
}

// aiIntentRules 默认意图注册表，使用内置投诉词表，按优先级从高到低排列。
// 词表刻意保守，只收录表达明确的说法，避免把正常客服对话误判成纠纷。
var aiIntentRules = newIntentRules(builtinComplaintPatterns)

// newIntentRules 用给定的投诉词表构建意图注册表（按优先级从高到低）。
// 投诉规则始终排首位并带 negative 标记，命中即短路；其余正向意图按既定优先级排列。
func newIntentRules(complaintPatterns []*regexp.Regexp) []aiIntentRule {
	return []aiIntentRule{
		{
			intent:   IntentComplaint,
			negative: true,
			// 负向词表覆盖平台敏感的纠纷表达；命中即拦截，即使消息同时夹带砍价表达。
			patterns: complaintPatterns,
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
}

// buildComplaintPatterns 合并内置词表与扩展词表，返回内置在前、扩展在后的并集切片。
// 新建切片避免追加时污染内置词表底层数组，确保扩展词表永远只是内置的补充而非替换。
func buildComplaintPatterns(extra []*regexp.Regexp) []*regexp.Regexp {
	// merged 是内置词表与扩展词表合并后的并集切片。
	merged := make([]*regexp.Regexp, 0, len(builtinComplaintPatterns)+len(extra))
	merged = append(merged, builtinComplaintPatterns...)
	merged = append(merged, extra...)
	return merged
}

// classifyIntent 用内置投诉词表对买家消息执行规则前置分类，返回按注册表优先级排序的命中意图。
// 负向意图一旦命中会排在首位并短路后续规则；返回空切片表示没有任何规则命中，
// 调用方应按「不确定就不猜」处理，而不是自行猜测一个意图。
func classifyIntent(text string) []string {
	return classifyIntentWithKeywords(text, nil)
}

// classifyIntentWithKeywords 在分类时把 extra 作为投诉负向词表的扩展项（与内置取并集）。
// extra 应为已通过校验的正则，非法正则不应传入（由 ParseComplaintKeywords 负责过滤）。
// 该函数保持纯函数特性，便于在不依赖外部设置存储的情况下直接单测扩展词表语义。
func classifyIntentWithKeywords(text string, extra []*regexp.Regexp) []string {
	// rules 是本次分类实际使用的意图注册表；有扩展词表时合并后重建，否则复用内置。
	rules := aiIntentRules
	if len(extra) > 0 {
		rules = newIntentRules(buildComplaintPatterns(extra))
	}
	return matchIntentRules(text, rules)
}

// matchIntentRules 对买家消息按给定注册表执行规则前置分类，返回按优先级排序的命中意图。
func matchIntentRules(text string, rules []aiIntentRule) []string {
	// lowered 是用于匹配的小写化文本；正则本身已带大小写不敏感标记，这里再统一一次以兼容后续纯字面规则。
	lowered := strings.ToLower(text)
	// hits 是按优先级排列的命中意图。
	hits := make([]string, 0, 2)
	// rule 是当前遍历到的意图规则。
	for _, rule := range rules {
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

// ParseComplaintKeywords 把设置值（逗号或分号分隔，兼容全角）解析为已校验的正则列表，供投诉词表扩展使用。
// 非法正则被忽略并记告警；空值或仅空白返回 nil（调用方回落内置词表，宁可多拦不可少拦）。
func ParseComplaintKeywords(raw string, logger *slog.Logger) []*regexp.Regexp {
	// value 是去首尾空白后的原始设置值。
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	// parts 是按逗号或分号（含全角）切分的原始词表项。
	parts := strings.FieldsFunc(value, func(r rune) bool {
		// r 是当前待判断的分隔符字符。
		return r == ',' || r == ';' || r == '，' || r == '；'
	})
	// out 是解析通过且成功编译的扩展正则。
	out := make([]*regexp.Regexp, 0, len(parts))
	// part 表示当前遍历过程中的原始词表项。
	for _, part := range parts {
		// keyword 是当前遍历到的词表项（去空白后）。
		keyword := strings.TrimSpace(part)
		if keyword == "" {
			continue
		}
		// re 是用户提供的扩展正则；编译失败说明非法，必须忽略并告警，不能中断拦截。
		re, err := regexp.Compile(keyword)
		if err != nil {
			if logger != nil {
				logger.Warn("忽略非法投诉扩展正则，回落内置词表", "keyword", keyword, "err", err)
			}
			continue
		}
		out = append(out, re)
	}
	// 全部为空或非法时返回 nil，统一回落内置词表（宁可多拦不可少拦）。
	if len(out) == 0 {
		return nil
	}
	return out
}

// 意图落库标签的取值语义（写入 AI 对话历史 intent 字段，读取侧/前端需同步说明新增取值）：
//   - 单一规则命中：直接写该意图常量（bargain/order/inquiry/consult/complaint）。
//   - 多条规则同时命中（如「便宜点怎么发货」既砍价又问物流）：写 ambiguous，
//     表示注册表无法确定单一意图，便于后续人工或分流策略复核。
//   - 零命中：写 chitchat，表示当前注册表未配置对应规则（即旧值 "chat" 的更名，语义一致）。
//
// primaryIntentLabel 把命中集合收敛成写入历史用的单一意图标签。
func primaryIntentLabel(hits []string) string {
	if len(hits) == 0 {
		return IntentChitchat
	}
	if len(hits) == 1 {
		return hits[0]
	}
	return IntentAmbiguous
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
