package executor

import "sync"

// VaultLock 控制 AI 任務期間暫停該用戶的 Vault 同步
type VaultLock struct {
	mu    sync.RWMutex
	locks map[string]string // userId → taskID
}

func NewVaultLock() *VaultLock {
	return &VaultLock{locks: make(map[string]string)}
}

// Lock 鎖定用戶的 Vault（回傳 false 表示已被其他任務鎖定）
func (l *VaultLock) Lock(userId, taskID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if existing, ok := l.locks[userId]; ok && existing != taskID {
		return false
	}
	l.locks[userId] = taskID
	return true
}

// Unlock 解鎖用戶的 Vault
func (l *VaultLock) Unlock(userId, taskID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.locks[userId] == taskID {
		delete(l.locks, userId)
	}
}

// IsLocked 檢查用戶的 Vault 是否被鎖定
func (l *VaultLock) IsLocked(userId string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.locks[userId]
	return ok
}

// GetLockingTask 取得鎖定的 taskID（空字串表示未鎖定）
func (l *VaultLock) GetLockingTask(userId string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.locks[userId]
}
