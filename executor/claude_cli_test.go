package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeExecutor_ConcurrencyLimit(t *testing.T) {
	exec := NewClaudeExecutor(2, 30*time.Second, "/tmp")

	// 確認 sem channel 大小
	if cap(exec.sem) != 2 {
		t.Errorf("semaphore capacity: got %d, want 2", cap(exec.sem))
	}
}

func TestClaudeExecutor_RunningCount(t *testing.T) {
	exec := NewClaudeExecutor(3, 30*time.Second, "/tmp")

	if exec.RunningCount() != 0 {
		t.Errorf("initial running: got %d, want 0", exec.RunningCount())
	}
	if exec.AvailableSlots() != 3 {
		t.Errorf("initial available: got %d, want 3", exec.AvailableSlots())
	}
}

func TestClaudeExecutor_ActiveCount(t *testing.T) {
	exec := NewClaudeExecutor(3, 30*time.Second, "/tmp")

	if exec.ActiveCount() != 0 {
		t.Errorf("initial active: got %d, want 0", exec.ActiveCount())
	}
}

func TestClaudeExecutor_CancelNonexistent(t *testing.T) {
	exec := NewClaudeExecutor(3, 30*time.Second, "/tmp")
	err := exec.Cancel("nonexistent")
	if err != nil {
		t.Errorf("cancel nonexistent should not error: %v", err)
	}
}

func TestClaudeExecutor_SemaphoreBlocking(t *testing.T) {
	exec := NewClaudeExecutor(1, 5*time.Second, "/tmp")

	var running int32
	var wg sync.WaitGroup

	// 佔用唯一的 semaphore slot
	exec.sem <- struct{}{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()

		atomic.AddInt32(&running, 1)
		_, err := exec.ExecuteTask(ctx, "blocked-task", "/tmp", "test")
		atomic.AddInt32(&running, -1)

		if err == nil {
			t.Error("expected error due to context timeout while waiting for semaphore")
		}
	}()

	// 等一下讓 goroutine 開始等待
	time.Sleep(50 * time.Millisecond)
	// 釋放 semaphore
	<-exec.sem

	wg.Wait()
}

func TestBuildClaudeMD(t *testing.T) {
	md := buildClaudeMD("幫我整理所有筆記")
	if md == "" {
		t.Error("should not be empty")
	}
	if !contains(md, "NoteCEO") {
		t.Error("should contain NoteCEO")
	}
	if !contains(md, "幫我整理所有筆記") {
		t.Error("should contain user instruction")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
