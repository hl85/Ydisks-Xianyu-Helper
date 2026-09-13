package db

import (
	"context"
	"testing"
	"time"
)

// TestHeartbeatStore_BeatThenLatest 验证首次写入走插入、再次写入走更新，
// 且 Latest 能读回写入的时间戳。
func TestHeartbeatStore_BeatThenLatest(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// store 是默认实例键的心跳仓储。
	store := NewHeartbeatStore(s.DB, s.Dialect, "")
	if store == nil {
		t.Fatal("NewHeartbeatStore 不应为 nil")
	}
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// first 是首次心跳时间基准。
	first := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	// beatErr 是首次写入错误，应成功插入单行。
	if beatErr := store.Beat(ctx, first); beatErr != nil {
		t.Fatalf("首次写入心跳: %v", beatErr)
	}
	// got1 是首次读回的心跳时间。
	got1, err1 := store.Latest(ctx)
	if err1 != nil {
		t.Fatalf("首次读取心跳: %v", err1)
	}
	if !got1.Equal(first) {
		t.Fatalf("首次心跳 = %v, want %v", got1, first)
	}
	// second 是第二次心跳时间，应覆盖首次而非新增行。
	second := first.Add(15 * time.Second)
	// beatErr2 是第二次写入错误，应走更新分支。
	if beatErr2 := store.Beat(ctx, second); beatErr2 != nil {
		t.Fatalf("第二次写入心跳: %v", beatErr2)
	}
	// got2 是更新后读回的心跳时间。
	got2, err2 := store.Latest(ctx)
	if err2 != nil {
		t.Fatalf("第二次读取心跳: %v", err2)
	}
	if !got2.Equal(second) {
		t.Fatalf("更新后心跳 = %v, want %v", got2, second)
	}
	// rows 统计确认仍是单行，更新分支没有产生重复实例键行。
	var rows int
	// countErr 是统计实例行数的错误。
	countErr := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_heartbeats WHERE instance_key = ?`, defaultHeartbeatInstanceKey).Scan(&rows)
	if countErr != nil {
		t.Fatalf("统计心跳行数: %v", countErr)
	}
	if rows != 1 {
		t.Fatalf("心跳实例行数 = %d, want 1（更新不应新增行）", rows)
	}
}

// TestHeartbeatStore_EmptyReturnsZero 验证数据库尚无心跳记录时 Latest 返回零值时间。
func TestHeartbeatStore_EmptyReturnsZero(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// store 是本次测试使用的默认实例键心跳仓储。
	store := NewHeartbeatStore(s.DB, s.Dialect, "")
	// got、err 保存空库读回的心跳时间及错误。
	got, err := store.Latest(context.Background())
	if err != nil {
		t.Fatalf("空库读取不应报错: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("空库应返回零值时间, got %v", got)
	}
}

// TestHeartbeatStore_NilStore 验证未初始化仓储返回错误而非 panic。
func TestHeartbeatStore_NilStore(t *testing.T) {
	// store 是未初始化的心跳仓储，等价于依赖缺失时的形态。
	var store *HeartbeatStore
	// beatErr 应是未初始化哨兵错误。
	if beatErr := store.Beat(context.Background(), time.Now()); beatErr == nil {
		t.Fatal("未初始化仓储 Beat 应返回错误")
	}
	// latestErr 应是未初始化哨兵错误。
	if _, latestErr := store.Latest(context.Background()); latestErr == nil {
		t.Fatal("未初始化仓储 Latest 应返回错误")
	}
}

// TestNewHeartbeatStore_NilDB 验证数据库为 nil 时构造返回 nil，调用方可据此跳过启动。
func TestNewHeartbeatStore_NilDB(t *testing.T) {
	// store 是 database 为 nil 时的构造结果，应为 nil。
	store := NewHeartbeatStore(nil, DialectSQLite, "")
	if store != nil {
		t.Fatalf("database 为 nil 时 NewHeartbeatStore 应返回 nil, got %+v", store)
	}
}

// TestNewHeartbeatStore_BlankKeyFallsBack 验证空白实例键回落默认键。
func TestNewHeartbeatStore_BlankKeyFallsBack(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// store 是用空实例键构造的心跳仓储，应回落默认键。
	store := NewHeartbeatStore(s.DB, s.Dialect, "   ")
	if store == nil {
		t.Fatal("database 非空时 NewHeartbeatStore 不应为 nil")
	}
	if store.InstanceKey != defaultHeartbeatInstanceKey {
		t.Fatalf("空白实例键应回落默认键 %q, got %q", defaultHeartbeatInstanceKey, store.InstanceKey)
	}
}
