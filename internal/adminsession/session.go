package adminsession

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type Session struct {
	Token     string
	Username  string
	Role      string
	ExpiresAt time.Time
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]Session
	now      func() time.Time
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
			delete(m.sessions, token)
		}
	}
}

func (m *Manager) Clear() {
	m.mu.Lock()
	m.sessions = make(map[string]Session)
	m.mu.Unlock()
}
