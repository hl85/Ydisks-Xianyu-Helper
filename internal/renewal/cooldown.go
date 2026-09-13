// cooldown.go 登录恢复相关冷却状态。
// Scheduler 和运行时 Adapter 共用同一实例，避免同一账号被重复触发密码登录。
// 可选接入持久化端口：进程重启后仍尊重最近的登录失败冷却，避免「重启→立即申请登录凭证→触发风控」。
// 接入失败或未接入时行为与纯内存版本完全一致（降级而非失败）。

package renewal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"xianyu-go/internal/db"
)

// 冷却记录在持久化层的分类键。
const (
	// CooldownKindSessionExpired 表示会话过期冷却。
	CooldownKindSessionExpired = "session_expired"
	// CooldownKindPasswordLogin 表示密码登录冷却。
	CooldownKindPasswordLogin = "password_login"
	// CooldownKindPasswordError 表示密码错误冷却。
	CooldownKindPasswordError = "password_error"
)

// cooldownPersistTimeout 是单次冷却持久化写入的超时预算；写入失败只降级不阻塞调用链。
const cooldownPersistTimeout = 3 * time.Second

// CooldownPersistence 是冷却状态的持久化端口；实现方负责跨进程保存与读取。
type CooldownPersistence interface {
	// ListCooldowns 返回全部冷却记录，供进程启动时恢复。
	ListCooldowns(ctx context.Context) ([]db.CredentialCooldown, error)
	// MarkCooldown 以 (cookieID, kind) 为主键记录最近一次冷却标记时间。
	MarkCooldown(ctx context.Context, cookieID, kind string, markedAt time.Time) error
	// ClearCooldowns 删除指定账号的全部冷却记录。
	ClearCooldowns(ctx context.Context, cookieID string) error
}

// CooldownManager 保存登录恢复相关冷却状态。
// Scheduler 和运行时 Adapter 共用同一实例，避免同一账号被重复触发密码登录。
// CooldownManager 用于本次流程后续判断的CooldownManager
type CooldownManager struct {
	mu sync.Mutex

	sessionExpiredByCookie map[string]time.Time
	passwordLoginByCookie  map[string]time.Time
	passwordErrorByCookie  map[string]time.Time

	// persistence 是可选的冷却持久化端口；为空时行为与纯内存版本完全一致。
	persistence CooldownPersistence
	// logger 记录持久化失败原因；为空时静默降级。
	logger *slog.Logger
}

// GlobalCooldown 是服务进程内的统一续期冷却状态。
var GlobalCooldown = NewCooldownManager()

// NewCooldownManager 封装NewCooldownManager业务协调。
func NewCooldownManager() *CooldownManager {
	return &CooldownManager{
		sessionExpiredByCookie: make(map[string]time.Time),
		passwordLoginByCookie:  make(map[string]time.Time),
		passwordErrorByCookie:  make(map[string]time.Time),
	}
}

// AttachPersistence 为冷却管理器接上持久化端口，并立即恢复历史冷却记录。
// 只允许在进程启动阶段、任何账号开始运行之前调用一次；并发调用不安全。
// ctx 是启动阶段的取消边界；恢复失败返回错误，但内存冷却保持可用，行为退化为与旧版本一致。
func (m *CooldownManager) AttachPersistence(ctx context.Context, persistence CooldownPersistence, logger *slog.Logger) error {
	if m == nil || persistence == nil {
		return nil
	}
	m.mu.Lock()
	m.persistence = persistence
	m.logger = logger
	m.mu.Unlock()
	// records 是持久化层里的全部冷却记录。
	records, err := persistence.ListCooldowns(ctx)
	if err != nil {
		return fmt.Errorf("读取冷却持久化记录失败: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// record 是当前遍历到的冷却记录。
	for _, record := range records {
		// 跳过空键与空时间戳的残缺记录，避免覆盖正常的内存状态。
		if strings.TrimSpace(record.CookieID) == "" || record.MarkedAt.IsZero() {
			continue
		}
		switch record.Kind {
		case CooldownKindSessionExpired:
			m.sessionExpiredByCookie[record.CookieID] = record.MarkedAt
		case CooldownKindPasswordLogin:
			m.passwordLoginByCookie[record.CookieID] = record.MarkedAt
		case CooldownKindPasswordError:
			m.passwordErrorByCookie[record.CookieID] = record.MarkedAt
		}
	}
	return nil
}

// persistSnapshot 获取当前持久化端口与日志器的快照；加锁读取避免与 AttachPersistence 竞争。
func (m *CooldownManager) persistSnapshot() (CooldownPersistence, *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistence, m.logger
}

// persistCooldown 把一次冷却标记写入持久化层；失败只记录告警，不影响内存冷却语义。
func (m *CooldownManager) persistCooldown(kind, cookieID string, at time.Time) {
	// persistence、logger 是当前快照；未接入持久化时直接返回。
	persistence, logger := m.persistSnapshot()
	if persistence == nil {
		return
	}
	// ctx 给持久化写入独立的短超时，避免调用方 ctx 已取消导致冷却记录丢失。
	ctx, cancel := context.WithTimeout(context.Background(), cooldownPersistTimeout)
	defer cancel()
	// err 是持久化写入失败原因；失败时下次重启会少一条冷却记录，属可接受的降级。
	if err := persistence.MarkCooldown(ctx, cookieID, kind, at); err != nil && logger != nil {
		logger.Warn("写入冷却持久化失败", "account", cookieID, "kind", kind, "err", err)
	}
}

// clearPersisted 删除指定账号在持久化层的全部冷却记录；失败只记录告警。
func (m *CooldownManager) clearPersisted(cookieID string) {
	// persistence、logger 是当前快照；未接入持久化时直接返回。
	persistence, logger := m.persistSnapshot()
	if persistence == nil {
		return
	}
	// ctx 是删除操作的独立短超时边界。
	ctx, cancel := context.WithTimeout(context.Background(), cooldownPersistTimeout)
	defer cancel()
	// err 是持久化删除失败原因；失败时该账号在下次重启后可能仍处于冷却，属可接受的降级。
	if err := persistence.ClearCooldowns(ctx, cookieID); err != nil && logger != nil {
		logger.Warn("清除冷却持久化失败", "account", cookieID, "err", err)
	}
}

// MarkSessionExpired 封装Mark会话Expired业务协调。
func (m *CooldownManager) MarkSessionExpired(cookieID string) {
	if m == nil {
		return
	}
	// now 是标记时刻；先写内存再落库，落库失败不影响内存语义。
	now := time.Now()
	m.mu.Lock()
	m.sessionExpiredByCookie[cookieID] = now
	m.mu.Unlock()
	m.persistCooldown(CooldownKindSessionExpired, cookieID, now)
}

// IsSessionCooled 封装Is会话Cooled业务协调。
func (m *CooldownManager) IsSessionCooled(cookieID string) (bool, time.Duration) {
	if m == nil {
		return false, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// last 用于本次流程后续判断的last
	last := m.sessionExpiredByCookie[cookieID]
	if last.IsZero() {
		return false, 0
	}
	// remain 用于本次流程后续判断的remain
	remain := sessionExpiredCooldown - time.Since(last)
	return remain > 0, remain
}

// TryPasswordLogin 封装Try密码登录业务协调。
func (m *CooldownManager) TryPasswordLogin(cookieID string) (bool, time.Duration) {
	// ok、remain 用于本次流程后续判断的ok、remain
	ok, remain, _ := m.PasswordLoginAllowed(cookieID, passwordLoginCooldown)
	if !ok {
		return false, remain
	}
	m.MarkPasswordLogin(cookieID)
	return true, 0
}

// PasswordLoginAllowed 只检查冷却而不写入时间。参考运行时只在真实密码
// 登录拿到 Cookie 后记录登录时间，接口/浏览器轻量续期和失败尝试都不能启动
// 登录冷却。reason 为 login_cooldown 或 password_error_cooldown。
// PasswordLoginAllowed 封装密码登录Allowed业务协调。
func (m *CooldownManager) PasswordLoginAllowed(cookieID string, loginCooldown time.Duration) (bool, time.Duration, string) {
	if m == nil {
		return true, 0, ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// now 用于本次流程后续判断的now
	now := time.Now()
	// last 用于本次流程后续判断的last
	last := m.passwordLoginByCookie[cookieID]
	if !last.IsZero() {
		// remain 用于本次流程后续判断的remain
		remain := loginCooldown - now.Sub(last)
		if remain > 0 {
			return false, remain, "login_cooldown"
		}
	}
	// lastErr 用于本次流程后续判断的lastErr
	lastErr := m.passwordErrorByCookie[cookieID]
	if !lastErr.IsZero() {
		// remain 用于本次流程后续判断的remain
		remain := passwordErrorCooldown - now.Sub(lastErr)
		if remain > 0 {
			return false, remain, "password_error_cooldown"
		}
	}
	return true, 0, ""
}

// MarkPasswordLogin 封装Mark密码登录业务协调。
func (m *CooldownManager) MarkPasswordLogin(cookieID string) {
	if m == nil {
		return
	}
	// now 是标记时刻；先写内存再落库，落库失败不影响内存语义。
	now := time.Now()
	m.mu.Lock()
	m.passwordLoginByCookie[cookieID] = now
	m.mu.Unlock()
	m.persistCooldown(CooldownKindPasswordLogin, cookieID, now)
}

// MarkPasswordError 封装Mark密码错误业务协调。
func (m *CooldownManager) MarkPasswordError(cookieID string) {
	if m == nil {
		return
	}
	// now 是标记时刻；先写内存再落库，落库失败不影响内存语义。
	now := time.Now()
	m.mu.Lock()
	m.passwordErrorByCookie[cookieID] = now
	m.mu.Unlock()
	m.persistCooldown(CooldownKindPasswordError, cookieID, now)
}

// Reset 封装Reset业务协调。
func (m *CooldownManager) Reset(cookieID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.sessionExpiredByCookie, cookieID)
	delete(m.passwordLoginByCookie, cookieID)
	delete(m.passwordErrorByCookie, cookieID)
	m.mu.Unlock()
	m.clearPersisted(cookieID)
}
