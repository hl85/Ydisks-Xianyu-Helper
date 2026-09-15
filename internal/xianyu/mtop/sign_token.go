// sign_token.go 负责 MTOP 签名令牌 _m_h5_tk 的存在性判定与错误分类。
//
// 签名令牌与登录凭证是两类不同的东西：它由 Token 接口随时补发，也会随任意 MTOP 响应轮换，
// 而登录凭证失效才需要重新登录。把两者混为一谈会导致「缺令牌」被误升级为密码登录级恢复，
// 或者被当成终态失败而让定时任务持续空转。本文件集中承载这一区分，供各调用方复用。
package mtop

import (
	"errors"

	"xianyu-go/internal/xianyu/protocol"
)

// ErrMissingSignToken 表示请求所需的 Cookie 中缺少 MTOP 签名令牌 _m_h5_tk。
//
// 该状态与登录凭证是否有效无关：签名令牌由 Token 接口随时补发，也会随任意 MTOP 响应轮换。
// 因此它属于「可即时修复的请求前置条件」，调用方必须先尝试刷新令牌并按原请求重试，
// 而不是把它当成终态失败——否则依赖数据库凭证做 MTOP 调用的定时任务（如自动评价）会在
// 令牌被其他流程覆盖后持续失败，只能等某个无关的 MTOP 请求恰好把令牌写回才能自愈。
var ErrMissingSignToken = errors.New("cookie 缺少 _m_h5_tk")

// IsMissingSignTokenErr 判断错误是否为缺少 MTOP 签名令牌，供调用方选择「刷新令牌后重试」。
func IsMissingSignTokenErr(err error) bool {
	return errors.Is(err, ErrMissingSignToken)
}

// SignTokenPresent 判断扁平 Cookie 中是否携带 MTOP 签名令牌 _m_h5_tk。
//
// 只返回布尔值、不暴露任何令牌内容，供各条 Cookie 写回路径做安全摘要日志。
// 用途是定位「哪条流程把签名令牌抹掉了」：令牌会在任意 MTOP 响应中轮换，
// 但若某条写回路径产出不含令牌的 Cookie 并覆盖数据库，依赖数据库凭证的定时任务就会失败。
func SignTokenPresent(cookies string) bool {
	return protocol.SignToken(cookies) != ""
}
