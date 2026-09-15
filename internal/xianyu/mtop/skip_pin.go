// Package mtop: 拼团小刀免拼发货域 — mtop.idle.groupon.activity.seller.freeshipping 调用与重试。
// 闲鱼「小刀」是拼团玩法，需凑够人数才成团；未成团的订单卡在待刀成状态无法发货。
// 平台在卖家订单列表的 rightVO.btnList 中下发 tradeAction=SKIP_PIN、name=直接免拼 的按钮，
// 其 clickEvent 携带本文件实现的端点与入参，等价于卖家在客户端点击「直接免拼」。
package mtop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xianyu-go/internal/xianyu/protocol"
)

// SkipPinAPI 是卖家对拼团小刀订单执行「直接免拼」的 MTOP 端点。
const SkipPinAPI = "https://h5api.m.goofish.com/h5/mtop.idle.groupon.activity.seller.freeshipping/1.0/"

// skipPinReferer 是免拼请求参与签名使用的来源页，与卖家工作台域名保持一致。
const skipPinReferer = "https://www.goofish.com/"

// skipPinPayload 是免拼接口要求的 JSON 请求体，字段顺序与平台按钮下发的 params 保持一致，
// 避免订单号直接拼接进 JSON 而破坏签名原文。
type skipPinPayload struct {
	// ItemID 是拼团商品标识。
	ItemID string `json:"itemId"`
	// BizOrderID 是拼团订单号；该接口使用 bizOrderId 而非 orderId。
	BizOrderID string `json:"bizOrderId"`
	// BuyerID 是发起小刀的买家标识。
	BuyerID string `json:"buyerId"`
}

// SkipPinFreeShippingContext 调用 mtop.idle.groupon.activity.seller.freeshipping 把待刀成的
// 拼团订单直接置为免拼可发货状态。
//
// 幂等性：平台对已免拼的订单重复调用不会改变结果，调用方可以把重复调用视为成功，
// 因此本方法不对「已免拼」做特殊分支，只把平台业务返回原样回传。
//
// 签名 token 过期时使用响应下发的新 Cookie 重签并重试，与改价、发货保持同一恢复策略。
func (c *ClientImpl) SkipPinFreeShippingContext(ctx context.Context, cookiesStr, orderID, itemID, buyerID string) (ok bool, ret []string, updatedCookies string, err error) {
	// currentCookies 是本轮请求实际使用的扁平 Cookie，优先取上下文中的会话状态。
	currentCookies := cookiesStr
	if // session 是上下文携带的响应 Cookie 会话，存在时以其合并状态为准。
	session := cookieSessionFromContext(ctx); session != nil {
		currentCookies, _, _ = session.State()
	}
	// lastRet 保存最后一次业务返回，供重试耗尽后回传调用方。
	var lastRet []string
	for // attempt 是含首次在内的 token 重签尝试序号。
	attempt := 0; attempt < 4; attempt++ {
		// previousCookies 记录本次请求前的 Cookie，用于判断响应是否下发了新 token。
		previousCookies := currentCookies
		// ok、ret、updated、requestErr 分别是单次调用的业务成功标志、业务返回、Cookie 更新和传输错误。
		ok, ret, updated, requestErr := c.skipPinFreeShippingOnce(ctx, currentCookies, orderID, itemID, buyerID)
		if requestErr != nil {
			if !IsMTopTokenExpiredErr(requestErr) {
				return false, ret, currentCookies, requestErr
			}
			lastRet = mtopErrorRet(requestErr)
		} else {
			lastRet = ret
			if updated != "" {
				currentCookies = updated
			}
			if ok {
				return true, ret, currentCookies, nil
			}
			requestErr = c.mtopResponseFailure("订单免拼接口", http.StatusOK, ret, "平台业务未确认成功")
			if !IsMTopTokenExpiredErr(requestErr) {
				return false, ret, currentCookies, requestErr
			}
		}
		if updated != "" {
			currentCookies = updated
		}
		if attempt == 3 {
			break
		}

		// MTop 通常会在 token 过期响应中通过 Set-Cookie 下发新签名 token；
		// 未下发时主动刷新一次 token 再重试。
		if !mtopTokenCookieChanged(previousCookies, currentCookies) {
			// refreshed、refreshErr 分别是 token 主动刷新结果和刷新失败原因。
			refreshed, refreshErr := c.RefreshTokenContext(ctx, currentCookies)
			if refreshErr != nil {
				return false, ret, currentCookies, fmt.Errorf("订单免拼 token 过期且刷新失败: %w", refreshErr)
			}
			if refreshed.UpdatedCookies != "" {
				currentCookies = refreshed.UpdatedCookies
			}
		}
		if // err 是重试间隔等待期间的上下文取消错误。
		err := sleepCtx(ctx, MTopRetryGap); err != nil {
			return false, ret, currentCookies, err
		}
	}
	return false, lastRet, currentCookies, fmt.Errorf("订单免拼接口 Token 重试失败: %w", c.mtopResponseFailure("订单免拼接口", http.StatusOK, lastRet, "重试次数已耗尽"))
}

// skipPinFreeShippingOnce 执行一次免拼请求；业务成功要求 ret 为 SUCCESS。
func (c *ClientImpl) skipPinFreeShippingOnce(ctx context.Context, cookiesStr, orderID, itemID, buyerID string) (ok bool, ret []string, updatedCookies string, err error) {
	// hc 是带统一日志的 HTTP 客户端。
	hc := c.httpClient()
	// endpoint 是实际请求端点，测试可通过 SkipPinURL 覆盖。
	endpoint := c.SkipPinURL
	if endpoint == "" {
		endpoint = SkipPinAPI
	}
	// signingCookies、requestCookies 分别是参与签名的 Cookie 和随请求发送的 Cookie。
	signingCookies, requestCookies := mtopRequestCookies(ctx, cookiesStr, skipPinReferer, endpoint)
	// token 是从 Cookie 提取的 MTOP 签名 token。
	token := protocol.SignToken(signingCookies)
	if token == "" {
		return false, nil, cookiesStr, fmt.Errorf("cookie 缺少 _m_h5_tk，无法调用免拼接口")
	}
	// t 是签名使用的毫秒时间戳。
	t := strconv.FormatInt(time.Now().UnixMilli(), 10)
	// payload 是免拼请求的结构化参数，序列化结果同时作为签名原文和 HTTP 请求体。
	payload := skipPinPayload{ItemID: itemID, BizOrderID: orderID, BuyerID: buyerID}
	// dataBytes、marshalErr 分别是序列化后的请求原文和理论上仅可能由未来字段变更引入的错误。
	dataBytes, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return false, nil, cookiesStr, fmt.Errorf("序列化订单免拼请求: %w", marshalErr)
	}
	// dataVal 是参与签名并提交的平台参数文本。
	dataVal := string(dataBytes)
	// sign 是本次请求的 MTOP 签名。
	sign := protocol.GenerateSign(t, token, dataVal)

	// query 是免拼请求的 URL 查询参数。
	query := buildSkipPinQuery(t, sign)
	// body 是 URL 编码后的请求体。
	body := "data=" + url.QueryEscape(dataVal)
	// req、err 分别是构造的 HTTP 请求和构造错误。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint+"?"+query,
		strings.NewReader(body))
	if err != nil {
		return false, nil, cookiesStr, err
	}
	setCommonHeaders(req, requestCookies)
	// resp、err 分别是 HTTP 响应和传输错误。
	resp, err := hc.Do(req)
	if err != nil {
		return false, nil, cookiesStr, fmt.Errorf("订单免拼请求失败: %w", err)
	}
	defer resp.Body.Close()
	// updated 是合并响应 Set-Cookie 后的最新扁平 Cookie。
	updated := absorbMTopResponseCookies(ctx, cookiesStr, resp)
	// raw、err 分别是响应正文和读取错误。
	raw, err := readMTopBody(resp)
	if err != nil {
		return false, nil, updated, c.mtopResponseFailureWithCause("订单免拼接口", resp.StatusCode, nil, "读取响应失败", err)
	}
	// res 是免拼响应的最小解析结构；data.data 是平台的业务成功标志，
	// 实测成功响应为 {"data":{"data":true},"ret":["SUCCESS::调用成功"]}。
	var res struct {
		Ret  []string `json:"ret"`
		Data struct {
			Data bool `json:"data"`
		} `json:"data"`
	}
	if // err 是响应 JSON 解析错误。
	err := json.Unmarshal(raw, &res); err != nil {
		return false, nil, updated, c.mtopResponseFailureWithCause("订单免拼接口", resp.StatusCode, nil, "JSON 解析失败", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return false, res.Ret, updated, c.mtopResponseFailure("订单免拼接口", resp.StatusCode, res.Ret, "HTTP 状态异常")
	}
	return hasMTopSuccess(res.Ret) && res.Data.Data, res.Ret, updated, nil
}

// buildSkipPinQuery 构造订单免拼请求的固定查询参数。
func buildSkipPinQuery(t, sign string) string {
	// parts 是按顺序拼接的查询键值对。
	parts := [][2]string{
		{"jsv", "2.7.2"},
		{"appKey", protocol.SignAppKey},
		{"t", t},
		{"sign", sign},
		{"v", "1.0"},
		{"type", "originaljson"},
		{"accountSite", "xianyu"},
		{"dataType", "json"},
		{"timeout", "20000"},
		{"api", "mtop.idle.groupon.activity.seller.freeshipping"},
		{"sessionOption", "AutoLoginOnly"},
	}
	// b 是查询字符串构造器。
	var b strings.Builder
	// i、p 分别是当前键值对的下标和内容。
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p[0])
		b.WriteByte('=')
		b.WriteString(p[1])
	}
	return b.String()
}
