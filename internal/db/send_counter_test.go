// send_counter_test.go 验证发送日计数仓储在 SQLite 上的读写闭环。
// 覆盖：无记录读取、累加、跨账号隔离、跨日隔离与全局桶（空 cookie_id）。

package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSendCounterStore_SQLiteLifecycle 验证发送日计数的完整读写闭环。
func TestSendCounterStore_SQLiteLifecycle(t *testing.T) {
	// tmp 保存隔离的临时数据库目录，测试结束后由 testing 清理。
	tmp := t.TempDir()
	// dbPath 指向本次测试使用的 SQLite 文件。
	dbPath := filepath.Join(tmp, "send-counter.db")
	// ctx 提供仓储调用所需的取消边界；测试内无外部取消源。
	ctx := context.Background()
	// rawDB、dialect、err 打开临时数据库并聚合仓储。
	rawDB, dialect, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rawDB.Close()
	// store 是被测的发送日计数仓储。
	store := &SendCounterStore{DB: rawDB, Dialect: dialect}

	// count、readErr 验证无记录时读取返回 0 而非错误。
	count, readErr := store.GetSendCount(ctx, "account-a", "2026-09-13")
	if readErr != nil || count != 0 {
		t.Fatalf("无记录读取应为 0,nil，实际 %d,%v", count, readErr)
	}

	// total、addErr 验证首次累加会插入新桶并返回累加后的值。
	total, addErr := store.AddSendCount(ctx, "account-a", "2026-09-13", 1)
	if addErr != nil || total != 1 {
		t.Fatalf("首次累加应为 1,nil，实际 %d,%v", total, addErr)
	}
	// total、addErr 验证重复累加在同一桶上递增。
	if total, addErr = store.AddSendCount(ctx, "account-a", "2026-09-13", 2); addErr != nil || total != 3 {
		t.Fatalf("二次累加应为 3,nil，实际 %d,%v", total, addErr)
	}

	// count、readErr 验证读取与累计值一致。
	if count, readErr = store.GetSendCount(ctx, "account-a", "2026-09-13"); readErr != nil || count != 3 {
		t.Fatalf("读取应为 3,nil，实际 %d,%v", count, readErr)
	}

	// otherCount、otherErr 验证跨账号桶相互隔离。
	otherCount, otherErr := store.AddSendCount(ctx, "account-b", "2026-09-13", 5)
	if otherErr != nil || otherCount != 5 {
		t.Fatalf("跨账号累加应为 5,nil，实际 %d,%v", otherCount, otherErr)
	}
	if count, readErr = store.GetSendCount(ctx, "account-a", "2026-09-13"); readErr != nil || count != 3 {
		t.Fatalf("跨账号写入不应影响账号 a，实际 %d,%v", count, readErr)
	}

	// nextDayCount、nextDayErr 验证跨自然日桶相互隔离。
	nextDayCount, nextDayErr := store.AddSendCount(ctx, "account-a", "2026-09-14", 4)
	if nextDayErr != nil || nextDayCount != 4 {
		t.Fatalf("跨日累加应为 4,nil，实际 %d,%v", nextDayCount, nextDayErr)
	}
	if count, readErr = store.GetSendCount(ctx, "account-a", "2026-09-13"); readErr != nil || count != 3 {
		t.Fatalf("跨日写入不应影响前一日，实际 %d,%v", count, readErr)
	}

	// globalCount、globalErr 验证空 cookie_id 全局桶可独立读写。
	globalCount, globalErr := store.AddSendCount(ctx, "", "2026-09-13", 7)
	if globalErr != nil || globalCount != 7 {
		t.Fatalf("全局桶累加应为 7,nil，实际 %d,%v", globalCount, globalErr)
	}
	if count, readErr = store.GetSendCount(ctx, "", "2026-09-13"); readErr != nil || count != 7 {
		t.Fatalf("全局桶读取应为 7,nil，实际 %d,%v", count, readErr)
	}

	// negativeDeltaCount、negativeDeltaErr 验证负增量回扣（额度回滚）也能正确落库。
	negativeDeltaCount, negativeDeltaErr := store.AddSendCount(ctx, "", "2026-09-13", -1)
	if negativeDeltaErr != nil || negativeDeltaCount != 6 {
		t.Fatalf("负增量应为 6,nil，实际 %d,%v", negativeDeltaCount, negativeDeltaErr)
	}
}
