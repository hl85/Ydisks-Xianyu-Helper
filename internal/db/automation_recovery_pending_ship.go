// automation_recovery_pending_ship.go 待发货兜底扫描的持久化查询。
// 职责：为「平台付款消息丢失 / 运行未完成」的订单提供稳定游标的补触发扫描入口。
// 从 automation_recovery.go 按业务职责拆出，仅移动代码、不改变任何逻辑。

package db

import (
	"context"
	"database/sql"
)

// PendingShipOrdersWithoutPaidRunAfter 用不可变的订单 ID 作为稳定游标分页扫描「已付款待发货、但尚无任何
// order_paid 运行」的订单，供调度器在付款系统消息丢失时补触发自动发货。
//
// 兜底边界：
//   - 只取 order_status='pending_ship'，即平台侧已确认付款、只差我方发货；
//   - 只取**完全没有任何** order_paid 运行的订单；已有运行的订单交给失败的运行恢复扫描处理，
//     避免与恢复链重复触发、也避免形成第二个执行入口；
//   - 要求账号下存在启用的 order_paid 规则且商品匹配，否则没有可执行的动作；
//   - 必须有 chat_id，否则无法向买家发起会话发送。
func (a *AutomationRules) PendingShipOrdersWithoutPaidRunAfter(ctx context.Context, afterOrderID string, limit int) ([]Order, error) {
	if limit <= 0 {
		limit = 200
	}
	// cursorSQL 用于本次流程后续判断的游标SQL
	cursorSQL := ""
	// args 用于本次流程后续判断的args
	args := []any{}
	if afterOrderID != "" {
		cursorSQL = " AND o.order_id>?"
		args = append(args, afterOrderID)
	}
	args = append(args, limit)
	// rows、err 用于本次流程后续判断的rows、err
	rows, err := a.DB.QueryContext(ctx, `
SELECT order_id,item_id,buyer_id,spec_name,spec_value,quantity,amount,order_status,cookie_id,is_bargain,
       receiver_name,receiver_phone,receiver_address,receiver_city,version,chat_id,system_shipped,
       COALESCE(paid_at,''),COALESCE(shipped_at,''),COALESCE(completed_at,''),
       COALESCE(buyer_reviewed_at,''),COALESCE(last_review_request_at,''),review_request_count,
       created_at,updated_at
  FROM orders o
WHERE o.order_status='pending_ship'
   AND o.deleted_at IS NULL
   AND COALESCE(o.chat_id,'')<>''
   AND NOT EXISTS (SELECT 1 FROM automation_runs r
                    WHERE r.order_id=o.order_id AND r.trigger_type='order_paid')
   AND EXISTS (SELECT 1 FROM automation_rules r2
                WHERE r2.cookie_id=o.cookie_id AND r2.trigger_type='order_paid'
                  AND r2.deleted_at IS NULL AND r2.enabled=1
                  AND (r2.item_id=o.item_id OR r2.item_id=''))`+cursorSQL+`
 ORDER BY o.order_id ASC
 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// out 用于本次流程后续判断的out
	out := []Order{}
	for rows.Next() {
		// ord 用于本次流程后续判断的ord
		var ord Order
		// isBargain、version、sysShipped 用于本次流程后续判断的isBargain、version、sysShipped
		var isBargain, version, sysShipped int
		// itemID、buyerID、specName、specValue、qty、amount、status、cookieID、receiverName、receiverPhone、receiverAddr、receiverCity、chatID 保存商品ID、buyerID、specName、specValue、qty、amount、status、cookieID、receiverName、receiverPhone、receiverAddr、receiverCity、chatID，供当前处理流程使用
		var itemID, buyerID, specName, specValue, qty, amount, status, cookieID,
			receiverName, receiverPhone, receiverAddr, receiverCity, chatID sql.NullString
		if // err 用于本次流程后续判断的err
		err := rows.Scan(&ord.OrderID, &itemID, &buyerID, &specName, &specValue, &qty, &amount,
			&status, &cookieID, &isBargain, &receiverName, &receiverPhone, &receiverAddr,
			&receiverCity, &version, &chatID, &sysShipped, &ord.PaidAt,
			&ord.ShippedAt, &ord.CompletedAt, &ord.BuyerReviewedAt, &ord.LastReviewRequestAt,
			&ord.ReviewRequestCount, &ord.CreatedAt, &ord.UpdatedAt); err != nil {
			return nil, err
		}
		ord.ItemID = itemID.String
		ord.BuyerID = buyerID.String
		ord.SpecName = specName.String
		ord.SpecValue = specValue.String
		ord.Quantity = qty.String
		ord.Amount = amount.String
		ord.OrderStatus = status.String
		ord.CookieID = cookieID.String
		ord.ReceiverName = receiverName.String
		ord.ReceiverPhone = receiverPhone.String
		ord.ReceiverAddr = receiverAddr.String
		ord.ReceiverCity = receiverCity.String
		ord.ChatID = chatID.String
		ord.IsBargain = isBargain
		ord.Version = version
		ord.SystemShipped = sysShipped != 0
		out = append(out, ord)
	}
	return out, rows.Err()
}

// PendingShipResume 描述一条尚可断点续跑的待发货订单运行。
type PendingShipResume struct {
	// Order 是平台侧已确认付款、仍需我方发货的订单事实。
	Order Order
	// RunID 是尚未完成全部动作的自动化运行主键。
	RunID int64
	// RuleID 是该运行所属规则，续跑时必须用它恢复规则快照。
	RuleID int64
	// Attempt 是运行当前代次；重开运行必须带上它做 CAS。
	Attempt int
	// ActionCursor 是尚未执行的下一个动作下标。
	ActionCursor int
	// Status 是运行当前状态（needs_review 或 failed）。
	Status string
}

// PendingShipResumableRunsAfter 用订单 ID 作为稳定游标分页扫描「已付款待发货、且存在剩余动作
// 全部为幂等状态动作的未完成 order_paid 运行」的订单，供调度器从检查点继续执行。
//
// 与 PendingShipOrdersWithoutPaidRunAfter 的分工：
//   - 前者覆盖「完全没有运行记录」的订单（付款事件在准备阶段就被卡死）；
//   - 本方法覆盖「有运行但没做完」的订单（例如消息动作结果不确定后运行被隔离）。
//
// **安全边界**：只续跑「游标已越过全部发卡/模板动作」的运行。只要还有会再次联系买家的动作
// 未完成，续跑就可能重复发货，因此一律不在此处自动恢复，交由人工核对。
func (a *AutomationRules) PendingShipResumableRunsAfter(ctx context.Context, afterOrderID string, maxAttempts, limit int) ([]PendingShipResume, error) {
	if limit <= 0 {
		limit = 200
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	// cursorSQL 保存稳定游标分页片段。
	cursorSQL := ""
	// args 保存查询参数，顺序与 SQL 中的占位符一致。
	args := []any{maxAttempts}
	if afterOrderID != "" {
		cursorSQL = " AND o.order_id>?"
		args = append(args, afterOrderID)
	}
	args = append(args, limit)
	// rows、err 保存待续跑运行查询结果及数据库错误。
	rows, err := a.DB.QueryContext(ctx, `
SELECT o.order_id,o.item_id,o.buyer_id,o.spec_name,o.spec_value,o.quantity,o.amount,o.order_status,o.cookie_id,o.is_bargain,
       o.receiver_name,o.receiver_phone,o.receiver_address,o.receiver_city,o.version,o.chat_id,o.system_shipped,
       COALESCE(o.paid_at,''),COALESCE(o.shipped_at,''),COALESCE(o.completed_at,''),
       COALESCE(o.buyer_reviewed_at,''),COALESCE(o.last_review_request_at,''),o.review_request_count,
       o.created_at,o.updated_at,
       r.id,r.rule_id,r.attempt_count,r.action_cursor,r.status
  FROM orders o
  JOIN automation_runs r ON r.order_id=o.order_id AND r.trigger_type='order_paid'
WHERE o.order_status='pending_ship'
   AND o.deleted_at IS NULL
   AND COALESCE(o.chat_id,'')<>''
   AND r.status IN ('needs_review','failed')
   AND r.attempt_count<?
   AND r.action_cursor<(SELECT COUNT(*) FROM automation_rule_actions a
                        WHERE a.rule_id=r.rule_id AND a.enabled=1)
   AND r.action_cursor>=(SELECT COUNT(*) FROM automation_rule_actions a
                          WHERE a.rule_id=r.rule_id AND a.enabled=1
                            AND a.action_type IN ('send_card','send_template'))
   AND EXISTS (SELECT 1 FROM automation_rules r2
                WHERE r2.cookie_id=o.cookie_id AND r2.trigger_type='order_paid'
                  AND r2.deleted_at IS NULL AND r2.enabled=1
                  AND (r2.item_id=o.item_id OR r2.item_id=''))`+cursorSQL+`
 ORDER BY o.order_id ASC
 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// out 保存本次扫描到的可续跑运行。
	out := []PendingShipResume{}
	for rows.Next() {
		// candidate 保存当前行的订单事实与运行检查点。
		var candidate PendingShipResume
		// isBargain、version、sysShipped 保存订单行的整型事实字段。
		var isBargain, version, sysShipped int
		// 以下 NullString 保存可能为空的订单文本字段。
		var itemID, buyerID, specName, specValue, qty, amount, status, cookieID,
			receiverName, receiverPhone, receiverAddr, receiverCity, chatID sql.NullString
		if err := rows.Scan(&candidate.Order.OrderID, &itemID, &buyerID, &specName, &specValue, &qty, &amount,
			&status, &cookieID, &isBargain, &receiverName, &receiverPhone, &receiverAddr,
			&receiverCity, &version, &chatID, &sysShipped, &candidate.Order.PaidAt,
			&candidate.Order.ShippedAt, &candidate.Order.CompletedAt, &candidate.Order.BuyerReviewedAt,
			&candidate.Order.LastReviewRequestAt, &candidate.Order.ReviewRequestCount,
			&candidate.Order.CreatedAt, &candidate.Order.UpdatedAt,
			&candidate.RunID, &candidate.RuleID, &candidate.Attempt, &candidate.ActionCursor, &candidate.Status); err != nil {
			return nil, err
		}
		candidate.Order.ItemID = itemID.String
		candidate.Order.BuyerID = buyerID.String
		candidate.Order.SpecName = specName.String
		candidate.Order.SpecValue = specValue.String
		candidate.Order.Quantity = qty.String
		candidate.Order.Amount = amount.String
		candidate.Order.OrderStatus = status.String
		candidate.Order.CookieID = cookieID.String
		candidate.Order.ReceiverName = receiverName.String
		candidate.Order.ReceiverPhone = receiverPhone.String
		candidate.Order.ReceiverAddr = receiverAddr.String
		candidate.Order.ReceiverCity = receiverCity.String
		candidate.Order.ChatID = chatID.String
		candidate.Order.IsBargain = isBargain
		candidate.Order.Version = version
		candidate.Order.SystemShipped = sysShipped != 0
		out = append(out, candidate)
	}
	return out, rows.Err()
}
