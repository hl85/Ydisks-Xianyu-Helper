package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// TestRunHeartbeatCheck_ReadsLatest 验证只读检查能从已迁移数据库读回刚写入的心跳。
func TestRunHeartbeatCheck_ReadsLatest(t *testing.T) {
	// dbPath 是隔离的临时 SQLite 文件路径。
	dbPath := filepath.Join(t.TempDir(), "heartbeat-check.db")
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// seedDB 是预置心跳用的已迁移数据库连接。
	seedDB, dialect, openErr := db.Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("打开种子数据库: %v", openErr)
	}
	// seedStore 用于写入一条测试心跳。
	seedStore := db.NewHeartbeatStore(seedDB, dialect, "default")
	// want 是预期读回的心跳时刻（秒级对齐）。
	want := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	// beatErr 是种子写入错误。
	if beatErr := seedStore.Beat(ctx, want); beatErr != nil {
		t.Fatalf("写入种子心跳: %v", beatErr)
	}
	// closeErr 是关闭种子连接错误；必须在只读检查重开前释放文件锁。
	if closeErr := seedDB.Close(); closeErr != nil {
		t.Fatalf("关闭种子连接: %v", closeErr)
	}
	// last、configured、err 是只读检查的结果。
	last, configured, err := runHeartbeatCheck(dbPath, "default")
	if err != nil {
		t.Fatalf("只读检查不应报错: %v", err)
	}
	if !configured {
		t.Fatal("已写入心跳时检查应判定为 configured")
	}
	if !last.Equal(want) {
		t.Fatalf("只读检查读回心跳 = %v, want %v", last, want)
	}
}

// TestRunHeartbeatCheck_Unconfigured 验证数据库尚无心跳记录时检查返回未配置。
func TestRunHeartbeatCheck_Unconfigured(t *testing.T) {
	// dbPath 是隔离的临时 SQLite 文件路径。
	dbPath := filepath.Join(t.TempDir(), "heartbeat-empty.db")
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// preDB 仅用于触发迁移建表，随后关闭释放文件。
	preDB, _, openErr := db.Open(ctx, dbPath)
	if openErr != nil {
		t.Fatalf("打开数据库以建表: %v", openErr)
	}
	// closeErr 是关闭预打开连接错误。
	if closeErr := preDB.Close(); closeErr != nil {
		t.Fatalf("关闭预打开连接: %v", closeErr)
	}
	// last、configured、err 是只读检查的结果；此时尚无心跳记录。
	last, configured, err := runHeartbeatCheck(dbPath, "default")
	if err != nil {
		t.Fatalf("空库检查不应报错: %v", err)
	}
	if configured {
		t.Fatal("尚无心跳记录时检查应判定为未配置")
	}
	if !last.IsZero() {
		t.Fatalf("未配置时读回应是零值时间, got %v", last)
	}
}
