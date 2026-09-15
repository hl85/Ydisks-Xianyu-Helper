// skip_pin.go 持久化「拼团小刀自动免拼」的商品名单。
//
// 名单是卖家运营配置（哪些商品需要买家下刀单后系统直接免拼发货），与登录凭证无关，
// 因此不做静态加密。订单同步发现带 SKIP_PIN 按钮的订单时按本名单决定是否触发免拼。
package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SkipPinItem 是一条自动免拼商品配置。
type SkipPinItem struct {
	// CookieID 是账号主键（cookies.id）。
	CookieID string
	// ItemID 是闲鱼商品 ID。
	ItemID string
	// Enabled 表示该商品当前是否启用自动免拼。
	Enabled bool
	// CreatedAt 是首次登记时间（Unix 秒）。
	CreatedAt int64
	// UpdatedAt 是最近一次变更时间（Unix 秒）。
	UpdatedAt int64
}

// SkipPinItems 提供按账号维度管理自动免拼商品名单的持久化能力。
type SkipPinItems struct {
	DB      *sql.DB
	Dialect Dialect
}

// List 返回某账号的全部名单条目（含停用），按登记时间倒序，供前端展示。
func (s *SkipPinItems) List(ctx context.Context, cookieID string) ([]SkipPinItem, error) {
	// rows、err 是名单查询结果与错误。
	rows, err := s.DB.QueryContext(ctx,
		`SELECT cookie_id,item_id,enabled,created_at,updated_at FROM skip_pin_items WHERE cookie_id=? ORDER BY created_at DESC, item_id`,
		cookieID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// items 聚集全部条目。
	items := make([]SkipPinItem, 0, 8)
	for rows.Next() {
		// item、enabled 是单条配置与开关原始值。
		var item SkipPinItem
		// enabled 是开关的存储原始值。
		var enabled int
		// err 是扫描错误。
		if err := rows.Scan(&item.CookieID, &item.ItemID, &enabled, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.Enabled = enabled != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

// ListEnabledItems 返回某账号已启用自动免拼的全部商品 ID，供订单同步触发器做成员判断。
func (s *SkipPinItems) ListEnabledItems(ctx context.Context, cookieID string) (map[string]struct{}, error) {
	// rows、err 是启用条目查询结果与错误。
	rows, err := s.DB.QueryContext(ctx,
		`SELECT item_id FROM skip_pin_items WHERE cookie_id=? AND enabled=1`, cookieID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// ids 是启用商品 ID 集合。
	ids := make(map[string]struct{}, 8)
	for rows.Next() {
		// id 是单个商品 ID。
		var id string
		// err 是扫描错误。
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// Upsert 新增或更新一条名单；已存在时仅更新开关与更新时间。
func (s *SkipPinItems) Upsert(ctx context.Context, cookieID, itemID string, enabled bool) error {
	// now 是本次写入的统一时间戳。
	now := time.Now().Unix()
	// enabledVal 是开关的存储表示。
	enabledVal := 0
	if enabled {
		enabledVal = 1
	}
	// existing 探测条目是否已存在，避免依赖各方言不同的 upsert 语法。
	var existing int
	// err 是存在性探测错误。
	if err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM skip_pin_items WHERE cookie_id=? AND item_id=?`, cookieID, itemID).Scan(&existing); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// _, err 是首次登记结果与错误。
		_, err := s.DB.ExecContext(ctx,
			`INSERT INTO skip_pin_items (cookie_id,item_id,enabled,created_at,updated_at) VALUES (?,?,?,?,?)`,
			cookieID, itemID, enabledVal, now, now)
		return err
	}
	// _, err 是更新开关结果与错误。
	_, err := s.DB.ExecContext(ctx,
		`UPDATE skip_pin_items SET enabled=?, updated_at=? WHERE cookie_id=? AND item_id=?`,
		enabledVal, now, cookieID, itemID)
	return err
}

// Delete 移除一条名单；条目不存在视为已删除（幂等）。
func (s *SkipPinItems) Delete(ctx context.Context, cookieID, itemID string) error {
	// _, err 是删除结果与错误。
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM skip_pin_items WHERE cookie_id=? AND item_id=?`, cookieID, itemID)
	return err
}
