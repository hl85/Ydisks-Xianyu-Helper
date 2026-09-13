package adapter

import (
	"context"
	"fmt"
	"log/slog"

	"xianyu-go/internal/db"
	"xianyu-go/internal/notify"
)

// businessSilenceAlerter 把进程级业务静默告警路由进既有账号通知链路。
//
// 通知渠道按账号（cookie_id）绑定，而业务静默是进程级事件、没有归属账号；
// 直接以空账号调用 NotifyAccountEvent 会查不到任何渠道、告警被静默丢弃。
// 因此本实现从「绑定了已启用渠道的账号」中选择 ID 最小者作为投递宿主，
// 以 warn 级别发送一条 business_silence 事件；多账号共享渠道时只会收到一条，不产生 N 倍重复。
type businessSilenceAlerter struct {
	// notifier 是进程启动时装配完成的通知出口；为 nil 时跳过发送。
	notifier *notify.Notifier
	// store 提供宿主账号查询；为 nil 时无法路由，跳过发送。
	store *db.Store
	// logger 记录宿主账号选择失败；不包含渠道配置等敏感内容。
	logger *slog.Logger
}

// NotifyBusinessSilence 发送一条业务静默告警；宿主账号不可用时只记日志，
// 不向看门狗返回错误，保证告警路由故障不影响业务扫描循环。
func (a *businessSilenceAlerter) NotifyBusinessSilence(ctx context.Context, title, body string) {
	if a == nil || a.notifier == nil || a.store == nil || a.store.Notifications == nil {
		return
	}
	// hosts、err 保存绑定了已启用渠道的账号列表及查询错误。
	hosts, err := a.store.Notifications.AccountIDsWithEnabledChannels(ctx)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("业务静默告警查询投递宿主账号失败", "err", err)
		}
		return
	}
	if len(hosts) == 0 {
		if a.logger != nil {
			a.logger.Warn("业务静默告警未发送：没有任何账号绑定已启用的通知渠道")
		}
		return
	}
	// host 是选定的投递宿主账号；列表按 ID 排序，取首个保证多次告警路由稳定。
	host := hosts[0]
	a.notifier.NotifyAccountEvent(host, notify.EventBusinessSilence, "warn", title, fmt.Sprintf("%s（投递宿主账号：%s）", body, host))
}
