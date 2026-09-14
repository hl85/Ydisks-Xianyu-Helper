package db

import (
	"context"
	"testing"
)

// TestUpsertOrderRecordsShippedAtOnStatusTransition 验证订单状态首次推进到已发货/已完成时仓储会补记发货时间，
// 且重复同步与早已处于该状态的历史订单都不会回填或刷新该时间。
// 该时间被「超时未评价求评价」的计划任务用作等待基准，一旦为空就会回退到随时被刷新的 updated_at。
func TestUpsertOrderRecordsShippedAtOnStatusTransition(t *testing.T) {
	// s、cleanup 是本次测试独占的数据库及其清理函数。
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 控制本次测试数据库调用的生命周期。
	ctx := context.Background()
	// cid 是测试订单归属的账号标识。
	_, cid := seedAccount(t, s)

	t.Run("首次推进到已发货会补记发货时间", func(t *testing.T) {
		// orderID 是本场景的订单标识。
		orderID := "shipped-transition"
		if // err 是写入待发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "pending_ship"}); err != nil {
			t.Fatal(err)
		}
		// before 是状态推进前的订单快照，err 是读取失败原因。
		before, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if before.ShippedAt != "" {
			t.Fatalf("状态推进前不应有发货时间: %q", before.ShippedAt)
		}

		if // err 是写入已发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "shipped"}); err != nil {
			t.Fatal(err)
		}
		// after 是状态推进后的订单快照，err 是读取失败原因。
		after, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if after.OrderStatus != "shipped" {
			t.Fatalf("订单状态=%q，期望 shipped", after.OrderStatus)
		}
		if after.ShippedAt == "" {
			t.Fatal("状态推进到已发货后应补记发货时间")
		}
		// firstShippedAt 保存首次记录的发货时间，供后续场景验证不被刷新。
		firstShippedAt := after.ShippedAt

		// 重复同步同一状态是订单刷新最常见的路径，不得刷新首次发货时间。
		if // err 是重复同步已发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "shipped", Amount: "9.90"}); err != nil {
			t.Fatal(err)
		}
		// repeated 是重复同步后的订单快照，err 是读取失败原因。
		repeated, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if repeated.ShippedAt != firstShippedAt {
			t.Fatalf("重复同步刷新了发货时间: %q → %q", firstShippedAt, repeated.ShippedAt)
		}

		// 继续推进到已完成同样不得覆盖首次发货时间。
		if // err 是写入已完成状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "completed"}); err != nil {
			t.Fatal(err)
		}
		// completedOrder 是推进到已完成后的订单快照，err 是读取失败原因。
		completedOrder, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if completedOrder.OrderStatus != "completed" {
			t.Fatalf("订单状态=%q，期望 completed", completedOrder.OrderStatus)
		}
		if completedOrder.ShippedAt != firstShippedAt {
			t.Fatalf("推进到已完成覆盖了发货时间: %q → %q", firstShippedAt, completedOrder.ShippedAt)
		}
	})

	t.Run("未发货的状态推进不写发货时间", func(t *testing.T) {
		// orderID 是本场景的订单标识。
		orderID := "pending-only"
		if // err 是写入已付款状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "paid"}); err != nil {
			t.Fatal(err)
		}
		if // err 是写入待发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "pending_ship"}); err != nil {
			t.Fatal(err)
		}
		// pending 是仍处于待发货阶段的订单快照，err 是读取失败原因。
		pending, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if pending.ShippedAt != "" {
			t.Fatalf("未发货订单不应有发货时间: %q", pending.ShippedAt)
		}
	})

	t.Run("历史订单缺少发货时间时不会被回填", func(t *testing.T) {
		// orderID 是本场景的订单标识。
		orderID := "legacy-shipped"
		if // err 是写入已发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "shipped"}); err != nil {
			t.Fatal(err)
		}
		// 构造历史数据形态：状态已是已发货，但历史库中没有发货时间。
		if _, err := s.DB.ExecContext(ctx, `UPDATE orders SET shipped_at='' WHERE order_id=?`, orderID); err != nil {
			t.Fatal(err)
		}
		// 重复同步同一状态属于历史订单会持续经历的路径，不得把它补记成“刚刚发货”。
		if // err 是重复同步已发货状态失败的原因。
		err := s.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: cid, OrderStatus: "shipped"}); err != nil {
			t.Fatal(err)
		}
		// legacy 是重复同步后的历史订单快照，err 是读取失败原因；
		// 它必须保持无发货时间，避免计划任务回溯老订单打扰买家。
		legacy, err := s.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if legacy.ShippedAt != "" {
			t.Fatalf("历史订单被回填了发货时间: %q", legacy.ShippedAt)
		}
	})
}
