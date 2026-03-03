package api

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TaskStore 任務狀態儲存抽象（可用 Redis / Memory）
type TaskStore interface {
	NextTaskID(ctx context.Context) (string, error)
	CreateTask(ctx context.Context, task *Task) error
	UpdateTask(ctx context.Context, task *Task) error
	GetTask(ctx context.Context, taskID string) (*Task, error)
	TryAcquireUserActiveTask(ctx context.Context, userID, taskID string) (bool, error)
	ReleaseUserActiveTask(ctx context.Context, userID, taskID string) error
}

// MemoryTaskStore 本地 fallback 實作
type MemoryTaskStore struct {
	mu      sync.RWMutex
	counter int64
	tasks   map[string]*Task
	active  map[string]string
}

func NewMemoryTaskStore() *MemoryTaskStore {
	return &MemoryTaskStore{tasks: map[string]*Task{}, active: map[string]string{}}
}

func (m *MemoryTaskStore) NextTaskID(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counter++
	return fmt.Sprintf("task-%d", m.counter), nil
}

func (m *MemoryTaskStore) CreateTask(ctx context.Context, task *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *task
	m.tasks[task.ID] = &cp
	return nil
}

func (m *MemoryTaskStore) UpdateTask(ctx context.Context, task *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *task
	m.tasks[task.ID] = &cp
	return nil
}

func (m *MemoryTaskStore) GetTask(ctx context.Context, taskID string) (*Task, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[taskID]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (m *MemoryTaskStore) TryAcquireUserActiveTask(ctx context.Context, userID, taskID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[userID]; ok && cur != "" {
		return false, nil
	}
	m.active[userID] = taskID
	return true, nil
}

func (m *MemoryTaskStore) ReleaseUserActiveTask(ctx context.Context, userID, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[userID]; ok && cur == taskID {
		delete(m.active, userID)
	}
	return nil
}

func terminalTask(status TaskStatus) bool {
	return status == TaskStatusCompleted || status == TaskStatusFailed || status == TaskStatusCancelled
}

func defaultTaskTTL() time.Duration { return 7 * 24 * time.Hour }
