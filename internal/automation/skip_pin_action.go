// skip_pin_action.go 实现「直接免拼」自动化动作的执行器。
package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"xianyu-go/internal/xianyu/mtop"
)

// skipPinOrder 对处于待刀成状态的拼团订单调用「直接免拼」接口。
// 平台免拼接口幂等：对已免拼订单重复调用无副作用，因此执行失败可安全进入通用恢复重试。
// 成功不产生买家可见消息，返回空结果数量；后续发货由既有付款发货链路接管。
func (e *automationActionExecutor) skipPinOrder(ctx context.Context, task Task, allowCredentialRecovery bool) error {
	// session 固定本次 MTOP 请求的最小凭证视图，外部调用期间不持有账号凭证锁。
	session, err := e.openShipmentConsignSession(ctx, task.AccountID)
	if err != nil {
		return err
	}
	// impl 是支持免拼调用的具体客户端；免拼能力较新，通过类型断言获取而不扩张 Client 接口。
	impl, ok := e.mtop().(*mtop.ClientImpl)
	if !ok || impl == nil {
		return fmt.Errorf("%w: 当前 MTOP 客户端不支持小刀免拼", errActionNotPerformed)
	}
	// ok、returns、updatedCookie、callErr 分别是免拼的业务成功标记、业务返回、扁平 Cookie 更新和调用错误。
	ok, returns, updatedCookie, callErr := impl.SkipPinFreeShippingContext(session.requestContext, session.cookieStr, task.OrderID, task.ItemID, task.BuyerID)
	// result 归并 MTOP 远端结果，Cookie 写回随后独立处理。
	result := shipmentConsignResult{succeeded: ok, returns: returns, updatedCookie: updatedCookie, callErr: callErr}
	// cookiePersistence 收集响应 Cookie 的条件写回结果，并在锁外同步在线运行时。
	cookiePersistence := e.persistShipmentConsignCookies(ctx, task.AccountID, session, result)
	// sessionErr 将请求层错误和远端业务失败统一为凭证状态判断输入。
	sessionErr := result.callErr
	if sessionErr == nil && !result.succeeded {
		sessionErr = errors.New(strings.Join(result.returns, "; "))
	}
	if mtop.IsCredentialRefreshableErr(sessionErr) {
		// recoverer 是当前生效的凭证恢复器快照，避免一次判断期间被替换两次。
		recoverer := e.recoverer()
		if allowCredentialRecovery && recoverer != nil && recoverer.RecoverExpiredCredential(ctx, task.AccountID) {
			e.logger.Info("小刀免拼凭证恢复成功，重新执行免拼", "account", task.AccountID, "order_id", task.OrderID)
			return e.skipPinOrder(ctx, task, false)
		}
		return fmt.Errorf("%w: 小刀免拼 %s 已失效且未能恢复: %v", errActionNotPerformed, task.OrderID, sessionErr)
	}
	if result.callErr != nil {
		// errorKind、hasErrorKind 保存 MTOP 错误分类；明确业务拒绝终止重试，平台系统错误保留为可恢复失败。
		errorKind, hasErrorKind := mtop.MTopErrorKindOf(result.callErr)
		if hasErrorKind && errorKind == mtop.MTopErrorBusiness {
			return noRetryAction(result.callErr)
		}
		return result.callErr
	}
	if !result.succeeded {
		return noRetryAction(errors.New(strings.Join(result.returns, "; ")))
	}
	if len(cookiePersistence.errors) > 0 {
		return errors.Join(cookiePersistence.errors...)
	}
	e.logger.Info("小刀自动免拼成功", "account", task.AccountID, "order_id", task.OrderID, "item_id", task.ItemID, "buyer_id", task.BuyerID)
	return nil
}

// 编译期符号引用：确保与执行器共享的错误分类与恢复机制保持一致。
var (
	_ = errors.New
	_ = fmt.Errorf
	_ = strings.Join
	_ = mtop.IsCredentialRefreshableErr
)
