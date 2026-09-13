package db

import (
	"context"
	"testing"
	"time"
)

// TestLatestBusinessActivityAt_PicksLatestAcrossTables 验证看门狗查询跨三张业务表取全局最新业务时间。
func TestLatestBusinessActivityAt_PicksLatestAcrossTables(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// fixtures 是按顺序执行的夹具写入语句及参数，覆盖三张业务表与必要的账号、规则外键。
	fixtures := []struct {
		// query 是夹具写入 SQL。
		query string
		// args 是写入参数。
		args []any
	}{
		{"INSERT INTO users (username, email, password_hash) VALUES ('watchdog-admin', 'watchdog@example.com', 'pw')", nil},
		{"INSERT INTO cookies (id, value, user_id) SELECT 'acct-latest', 'cookie-value', id FROM users WHERE username='watchdog-admin'", nil},
		{"INSERT INTO ws_messages (cookie_id, direction, raw_text, parsed_json, message_kind, parse_status, error, created_at) VALUES (?,?,?,?,?,?,?,?)",
			[]any{"acct-latest", "in", "raw", "{}", "", "raw", "", "2026-09-13 08:00:00"}},
		{"INSERT INTO orders (order_id, cookie_id, created_at, updated_at) VALUES (?,?,?,?)",
			[]any{"order-1", "acct-latest", "2026-09-13 08:00:00", "2026-09-13 09:30:00"}},
		{"INSERT INTO automation_rules (user_id, cookie_id, name, trigger_type) SELECT id, 'acct-latest', 'r', 'order_paid' FROM users WHERE username='watchdog-admin'", nil},
		{"INSERT INTO automation_runs (rule_id, cookie_id, trigger_type, trigger_key, status, created_at, updated_at) VALUES (1, ?, 'order_paid', 'k', 'success', ?, ?)",
			[]any{"acct-latest", "2026-09-13 08:00:00", "2026-09-13 08:45:00"}},
	}
	for // i、fixture 表示当前遍历过程中的夹具下标与写入项
	i, fixture := range fixtures {
		// execErr 表示执行单条夹具写入的错误。
		_, execErr := s.DB.ExecContext(ctx, fixture.query, fixture.args...)
		if execErr != nil {
			t.Fatalf("写入夹具 #%d %q: %v", i, fixture.query, execErr)
		}
	}
	// got、err 保存查询结果及错误。
	got, err := s.Analytics.LatestBusinessActivityAt(ctx)
	if err != nil {
		t.Fatalf("查询最近业务活动: %v", err)
	}
	// want 是三张表中最晚的业务时间，应来自 orders.updated_at。
	want := time.Date(2026, 9, 13, 9, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("最近业务活动 = %v, want %v", got, want)
	}
}

// TestLatestBusinessActivityAt_EmptyDatabaseReturnsZero 验证空业务库返回零值时间，调用方不得据此判定静默。
func TestLatestBusinessActivityAt_EmptyDatabaseReturnsZero(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// got、err 保存空库查询结果及错误。
	got, err := s.Analytics.LatestBusinessActivityAt(context.Background())
	if err != nil {
		t.Fatalf("空库查询不应报错: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("空库应返回零值时间, got %v", got)
	}
}

// TestLatestBusinessActivityAt_ClosedStore 验证未初始化仓储返回错误而非 panic。
func TestLatestBusinessActivityAt_ClosedStore(t *testing.T) {
	// q 是未初始化的查询边界，等价于构造缺失时的形态。
	var q *AnalyticsQueries
	if // err 表示未初始化查询时的预期错误。
	_, err := q.LatestBusinessActivityAt(context.Background()); err == nil {
		t.Fatal("未初始化仓储应返回错误")
	}
}

// TestParseBusinessActivityTime_Formats 覆盖业务时间列可能出现的格式与非法输入。
func TestParseBusinessActivityTime_Formats(t *testing.T) {
	// cases 是表驱动用例：输入文本与期望时间（零值表示解析失败）。
	cases := []struct {
		// name 是用例名称。
		name string
		// raw 是输入时间文本。
		raw string
		// want 是期望解析结果。
		want time.Time
	}{
		{"SQLite CURRENT_TIMESTAMP", "2026-09-13 08:00:00", time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)},
		{"带小数秒", "2026-09-13 08:00:05.123", time.Date(2026, 9, 13, 8, 0, 5, 123000000, time.UTC)},
		{"RFC3339", "2026-09-13T08:00:00Z", time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)},
		{"非法文本", "not-a-time", time.Time{}},
		{"空白文本", "   ", time.Time{}},
	}
	for // tc 表示当前遍历过程中的用例
	_, tc := range cases {
		// got 保存实际解析结果。
		got := parseBusinessActivityTime(tc.raw)
		if !got.Equal(tc.want) {
			t.Fatalf("%s: parse(%q) = %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
}

// TestAccountIDsWithEnabledChannels 验证宿主账号查询只返回绑定已启用渠道且绑定启用的账号，并按 ID 稳定排序。
func TestAccountIDsWithEnabledChannels(t *testing.T) {
	// s、cleanup 用于本次流程后续判断的s、cleanup
	s, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 用于本次流程后续判断的ctx
	ctx := context.Background()
	// fixtures 建测试用户、三个账号、两个渠道与多条绑定：acct-a 绑定启用渠道，acct-b 绑定禁用渠道，acct-c 无绑定。
	fixtures := []string{
		"INSERT INTO users (username, email, password_hash) VALUES ('watchdog-admin', 'watchdog@example.com', 'pw')",
		"INSERT INTO cookies (id, value, user_id) SELECT 'acct-a', 'v', id FROM users WHERE username='watchdog-admin'",
		"INSERT INTO cookies (id, value, user_id) SELECT 'acct-b', 'v', id FROM users WHERE username='watchdog-admin'",
		"INSERT INTO cookies (id, value, user_id) SELECT 'acct-c', 'v', id FROM users WHERE username='watchdog-admin'",
		"INSERT INTO notification_channels (name, type, config, enabled, user_id) SELECT 'ch-on', 'webhook', '{}', 1, id FROM users WHERE username='watchdog-admin'",
		"INSERT INTO notification_channels (name, type, config, enabled, user_id) SELECT 'ch-off', 'webhook', '{}', 0, id FROM users WHERE username='watchdog-admin'",
		"INSERT INTO message_notifications (cookie_id, channel_id, enabled) VALUES ('acct-a', 1, 1)",
		"INSERT INTO message_notifications (cookie_id, channel_id, enabled) VALUES ('acct-b', 2, 1)",
	}
	for // fixture 表示当前遍历过程中的夹具写入语句。
	_, fixture := range fixtures {
		if // execErr 表示执行单条夹具写入的错误。
		_, execErr := s.DB.ExecContext(ctx, fixture); execErr != nil {
			t.Fatalf("写入夹具: %v", execErr)
		}
	}
	// hosts、err 保存查询结果及错误。
	hosts, err := s.Notifications.AccountIDsWithEnabledChannels(ctx)
	if err != nil {
		t.Fatalf("查询宿主账号: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "acct-a" {
		t.Fatalf("宿主账号 = %v, want [acct-a]", hosts)
	}
}
