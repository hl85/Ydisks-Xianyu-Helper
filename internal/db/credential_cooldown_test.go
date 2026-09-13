// credential_cooldown_test.go 登录恢复冷却持久化仓储的 sqlite 闭环测试。

package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestCredentialCooldownRoundTrip 验证冷却记录的写入、覆盖、读取与清除在 sqlite 上闭环。
func TestCredentialCooldownRoundTrip(t *testing.T) {
	// ctx 是用例取消边界。
	ctx := context.Background()
	// database 是临时 sqlite 数据库；Open 会自动应用全部迁移。
	database, _, err := Open(ctx, filepath.Join(t.TempDir(), "cooldown.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer database.Close()
	// store 是绑定 sqlite 方言的仓储集合。
	store := NewStore(database, DialectSQLite)
	// 建管理员与账号记录，满足冷却表对 cookies 表的外键约束。
	store.Users.Create(ctx, "admin", "a@e.com", "pw")
	// admin 是刚创建的用户。
	admin, userErr := store.Users.GetByUsername(ctx, "admin")
	if userErr != nil {
		t.Fatalf("读取管理员失败: %v", userErr)
	}
	// cookieErr 是账号写入结果。
	if cookieErr := store.Cookies.Save(ctx, "cid", "unb=1;", admin.ID); cookieErr != nil {
		t.Fatalf("写入账号失败: %v", cookieErr)
	}
	// markedAt 是测试用的标记时刻，截断到秒以对齐存储精度。
	markedAt := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	// writeErr 是首次写入结果。
	if writeErr := store.Renewal.MarkCooldown(ctx, "cid", "session_expired", markedAt); writeErr != nil {
		t.Fatalf("写入冷却记录失败: %v", writeErr)
	}
	// records 是首次读取结果。
	records, listErr := store.Renewal.ListCooldowns(ctx)
	if listErr != nil {
		t.Fatalf("读取冷却记录失败: %v", listErr)
	}
	if len(records) != 1 {
		t.Fatalf("应有 1 条冷却记录，实际 %d", len(records))
	}
	if records[0].CookieID != "cid" || records[0].Kind != "session_expired" {
		t.Fatalf("记录键不符: %+v", records[0])
	}
	if !records[0].MarkedAt.Equal(markedAt) {
		t.Fatalf("标记时间应为 %v，实际 %v", markedAt, records[0].MarkedAt)
	}
	// 覆盖同键写入后仍应只有一条，且时间被更新。
	// newer 是覆盖用的更新时刻。
	newer := markedAt.Add(time.Minute)
	// overwriteErr 是覆盖写入结果。
	if overwriteErr := store.Renewal.MarkCooldown(ctx, "cid", "session_expired", newer); overwriteErr != nil {
		t.Fatalf("覆盖冷却记录失败: %v", overwriteErr)
	}
	// records2 是覆盖后的读取结果。
	records2, listErr2 := store.Renewal.ListCooldowns(ctx)
	if listErr2 != nil {
		t.Fatalf("覆盖后读取失败: %v", listErr2)
	}
	if len(records2) != 1 {
		t.Fatalf("同键覆盖后应仍为 1 条，实际 %d", len(records2))
	}
	if !records2[0].MarkedAt.Equal(newer) {
		t.Fatalf("覆盖后时间应为 %v，实际 %v", newer, records2[0].MarkedAt)
	}
	// 同账号不同类别应共存。
	// otherErr 是第二类别的写入结果。
	if otherErr := store.Renewal.MarkCooldown(ctx, "cid", "password_error", newer); otherErr != nil {
		t.Fatalf("写入第二类冷却失败: %v", otherErr)
	}
	// records3 是两类共存后的读取结果。
	records3, listErr3 := store.Renewal.ListCooldowns(ctx)
	if listErr3 != nil {
		t.Fatalf("读取两类冷却失败: %v", listErr3)
	}
	if len(records3) != 2 {
		t.Fatalf("两类冷却应共存，实际 %d 条", len(records3))
	}
	// 清除后应无记录。
	// clearErr 是清除结果。
	if clearErr := store.Renewal.ClearCooldowns(ctx, "cid"); clearErr != nil {
		t.Fatalf("清除冷却记录失败: %v", clearErr)
	}
	// records4 是清除后的读取结果。
	records4, listErr4 := store.Renewal.ListCooldowns(ctx)
	if listErr4 != nil {
		t.Fatalf("清除后读取失败: %v", listErr4)
	}
	if len(records4) != 0 {
		t.Fatalf("清除后应无记录，实际 %d", len(records4))
	}
}
