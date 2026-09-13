// cooldown_persistence_test.go 冷却管理器接入持久化端口后的行为测试。
// 全部用注入的内存持久化实现，不依赖数据库，也不产生真实等待。

package renewal

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// cooldownKey 是内存持久化实现里记录的复合键。
type cooldownKey struct {
	// cookieID 是账号标识。
	cookieID string
	// kind 是冷却类别。
	kind string
}

// fakeCooldownPersistence 是冷却持久化端口的内存实现。
type fakeCooldownPersistence struct {
	mu sync.Mutex
	// records 保存全部冷却记录。
	records map[cooldownKey]time.Time
	// cleared 记录被清除过的账号。
	cleared []string
	// listErr 模拟读取失败。
	listErr error
}

// newFakeCooldownPersistence 构造一个空的内存持久化实现。
func newFakeCooldownPersistence() *fakeCooldownPersistence {
	return &fakeCooldownPersistence{records: make(map[cooldownKey]time.Time)}
}

// ListCooldowns 返回全部记录；listErr 非空时模拟读取失败。
func (f *fakeCooldownPersistence) ListCooldowns(ctx context.Context) ([]db.CredentialCooldown, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	// records 是返回结果。
	records := make([]db.CredentialCooldown, 0, len(f.records))
	// key、markedAt 是当前遍历到的记录键与标记时间。
	for key, markedAt := range f.records {
		records = append(records, db.CredentialCooldown{CookieID: key.cookieID, Kind: key.kind, MarkedAt: markedAt})
	}
	return records, nil
}

// MarkCooldown 记录一次冷却标记。
func (f *fakeCooldownPersistence) MarkCooldown(ctx context.Context, cookieID, kind string, markedAt time.Time) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[cooldownKey{cookieID: cookieID, kind: kind}] = markedAt
	return nil
}

// ClearCooldowns 删除指定账号的全部记录并登记清除动作。
func (f *fakeCooldownPersistence) ClearCooldowns(ctx context.Context, cookieID string) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	// key 是当前遍历到的记录键。
	for key := range f.records {
		if key.cookieID == cookieID {
			delete(f.records, key)
		}
	}
	f.cleared = append(f.cleared, cookieID)
	return nil
}

// hasRecord 返回指定冷却记录是否存在。
func (f *fakeCooldownPersistence) hasRecord(cookieID, kind string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	// ok 表示记录是否存在。
	_, ok := f.records[cooldownKey{cookieID: cookieID, kind: kind}]
	return ok
}

// TestAttachPersistence_RestoresCooldowns 验证重启后能从持久化层恢复仍在冷却期内的记录。
func TestAttachPersistence_RestoresCooldowns(t *testing.T) {
	// fake 里预先放一条 1 分钟前的会话过期记录，默认 300 秒冷却尚未走完。
	fake := newFakeCooldownPersistence()
	// err 是预置记录结果。
	if err := fake.MarkCooldown(context.Background(), "cid", CooldownKindSessionExpired, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("预置记录失败: %v", err)
	}
	// manager 是待恢复的冷却管理器。
	manager := NewCooldownManager()
	// err2 是恢复结果。
	if err2 := manager.AttachPersistence(context.Background(), fake, slog.Default()); err2 != nil {
		t.Fatalf("恢复冷却失败: %v", err2)
	}
	// cooled、remain 是恢复后的会话冷却状态。
	cooled, remain := manager.IsSessionCooled("cid")
	if !cooled {
		t.Fatal("重启后应继续处于会话冷却期")
	}
	if remain <= 0 || remain > sessionExpiredCooldown {
		t.Fatalf("剩余冷却应落在 (0, %v] 区间，实际 %v", sessionExpiredCooldown, remain)
	}
}

// TestAttachPersistence_IgnoresExpiredRecords 验证已过冷却期的历史记录不会再次生效。
func TestAttachPersistence_IgnoresExpiredRecords(t *testing.T) {
	// fake 里放一条 10 分钟前的记录，300 秒冷却早已走完。
	fake := newFakeCooldownPersistence()
	// err 是预置记录结果。
	if err := fake.MarkCooldown(context.Background(), "cid", CooldownKindSessionExpired, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("预置记录失败: %v", err)
	}
	// manager 是待恢复的冷却管理器。
	manager := NewCooldownManager()
	// err2 是恢复结果。
	if err2 := manager.AttachPersistence(context.Background(), fake, slog.Default()); err2 != nil {
		t.Fatalf("恢复冷却失败: %v", err2)
	}
	// cooled 是恢复后的会话冷却结论。
	if cooled, _ := manager.IsSessionCooled("cid"); cooled {
		t.Fatal("已过冷却期的记录不应再次生效")
	}
}

// TestMarkWritesThroughToPersistence 验证三类冷却标记都会写穿到持久化层。
func TestMarkWritesThroughToPersistence(t *testing.T) {
	// fake 是内存持久化实现。
	fake := newFakeCooldownPersistence()
	// manager 是接入持久化的冷却管理器。
	manager := NewCooldownManager()
	// err 是接入结果。
	if err := manager.AttachPersistence(context.Background(), fake, slog.Default()); err != nil {
		t.Fatalf("接入持久化失败: %v", err)
	}
	manager.MarkSessionExpired("cid-a")
	manager.MarkPasswordLogin("cid-b")
	manager.MarkPasswordError("cid-c")
	if !fake.hasRecord("cid-a", CooldownKindSessionExpired) {
		t.Fatal("会话过期标记应写穿到持久化层")
	}
	if !fake.hasRecord("cid-b", CooldownKindPasswordLogin) {
		t.Fatal("密码登录标记应写穿到持久化层")
	}
	if !fake.hasRecord("cid-c", CooldownKindPasswordError) {
		t.Fatal("密码错误标记应写穿到持久化层")
	}
}

// TestResetClearsPersistence 验证冷却重置会同步清除持久化记录。
func TestResetClearsPersistence(t *testing.T) {
	// fake 是内存持久化实现。
	fake := newFakeCooldownPersistence()
	// manager 是接入持久化的冷却管理器。
	manager := NewCooldownManager()
	// err 是接入结果。
	if err := manager.AttachPersistence(context.Background(), fake, slog.Default()); err != nil {
		t.Fatalf("接入持久化失败: %v", err)
	}
	manager.MarkSessionExpired("cid")
	if !fake.hasRecord("cid", CooldownKindSessionExpired) {
		t.Fatal("前置条件失败：标记应已写入持久化层")
	}
	manager.Reset("cid")
	if fake.hasRecord("cid", CooldownKindSessionExpired) {
		t.Fatal("重置后持久化记录应被清除")
	}
	if len(fake.cleared) != 1 || fake.cleared[0] != "cid" {
		t.Fatalf("应登记一次清除动作，实际 %v", fake.cleared)
	}
}

// TestAttachPersistence_FailsOpen 验证恢复失败时管理器仍按纯内存模式可用。
func TestAttachPersistence_FailsOpen(t *testing.T) {
	// fake 的读取失败由 listErr 注入。
	fake := newFakeCooldownPersistence()
	// listFailure 是注入的读取失败。
	listFailure := errors.New("模拟读取失败")
	fake.mu.Lock()
	fake.listErr = listFailure
	fake.mu.Unlock()
	// manager 是待接入的冷却管理器。
	manager := NewCooldownManager()
	// err 是接入结果，应返回错误但不影响后续可用性。
	err := manager.AttachPersistence(context.Background(), fake, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "读取冷却持久化记录失败") {
		t.Fatalf("恢复失败应返回错误，实际 %v", err)
	}
	// 标记仍应写内存（持久化端口已接上，写穿不报错）。
	manager.MarkSessionExpired("cid")
	// cooled 是内存冷却结论。
	if cooled, _ := manager.IsSessionCooled("cid"); !cooled {
		t.Fatal("恢复失败后内存冷却仍应可用")
	}
}

// TestAttachPersistence_SkipsIncompleteRecords 验证空键或空时间戳的残缺记录不会被恢复。
func TestAttachPersistence_SkipsIncompleteRecords(t *testing.T) {
	// fake 预置一条空账号的记录。
	fake := newFakeCooldownPersistence()
	// err 是预置记录结果。
	if err := fake.MarkCooldown(context.Background(), "", CooldownKindSessionExpired, time.Now()); err != nil {
		t.Fatalf("预置记录失败: %v", err)
	}
	// manager 是待恢复的冷却管理器。
	manager := NewCooldownManager()
	// err2 是恢复结果。
	if err2 := manager.AttachPersistence(context.Background(), fake, slog.Default()); err2 != nil {
		t.Fatalf("恢复冷却失败: %v", err2)
	}
	// 不应因残缺记录而 panic，且空键不产生任何冷却状态。
	// cooled 是空键的冷却结论。
	if cooled, _ := manager.IsSessionCooled(""); cooled {
		t.Fatal("空账号不应产生冷却状态")
	}
}
