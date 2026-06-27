// internal/adminsession/session.go 管理短生命周期的内存管理会话，并在用户状态变化时让相关会话失效。

package adminsession

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type Session struct {
	// Token 只保存在内存和客户端，不写入 SQLite；进程重启会让所有会话失效。
	Token     string
	Username  string
	Role      string
	ExpiresAt time.Time
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]Session
	// now 可在测试中注入，便于验证过期清理逻辑。
	now func() time.Time
}

func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]Session),
		now:      time.Now,
	}
}

func NewManagerWithClock(now func() time.Time) *Manager {
	manager := NewManager()
	if now != nil {
		manager.now = now
	}
	return manager
}

func (m *Manager) Create(username, role string, ttl time.Duration) (Session, error) {
	// 32 字节随机数再做 URL 安全 base64，足够作为 bearer token 使用。
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return Session{}, err
	}
	session := Session{
		Token:     base64.RawURLEncoding.EncodeToString(tokenBytes),
		Username:  username,
		Role:      role,
		ExpiresAt: m.now().Add(ttl),
	}

	m.mu.Lock()
	m.sessions[session.Token] = session
	m.mu.Unlock()
	return session, nil
}

func (m *Manager) Get(token string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	session, ok := m.sessions[token]
	if !ok {
		return Session{}, false
	}
	if m.now().After(session.ExpiresAt) {
		// 读取时顺手清理过期会话，避免后台清理 goroutine。
		delete(m.sessions, token)
		return Session{}, false
	}
	return session, true
}

func (m *Manager) Delete(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func (m *Manager) RemoveUser(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for token, session := range m.sessions {
		if session.Username == username {
			// 用户密码、角色或禁用状态变化后，调用方用该方法使旧 token 立即失效。
			delete(m.sessions, token)
		}
	}
}

func (m *Manager) Clear() {
	m.mu.Lock()
	m.sessions = make(map[string]Session)
	m.mu.Unlock()
}
