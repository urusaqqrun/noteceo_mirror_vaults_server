package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockExecutor 模擬任務執行器
type mockExecutor struct {
	executeDelay time.Duration
	executeErr   error
}

func (m *mockExecutor) Execute(task *Task) error {
	if m.executeDelay > 0 {
		time.Sleep(m.executeDelay)
	}
	if m.executeErr != nil {
		return m.executeErr
	}
	task.Result = "done"
	return nil
}
func (m *mockExecutor) Cancel(taskID string) error { return nil }

func setupTestServer() (*TaskHandler, *MemoryTaskStore, *http.ServeMux) {
	exec := &mockExecutor{}
	store := NewMemoryTaskStore()
	h := NewTaskHandler(exec, store)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, store, mux
}

func TestCreateTask_Success(t *testing.T) {
	_, _, mux := setupTestServer()
	body, _ := json.Marshal(TaskRequest{UserID: "user1", Instruction: "整理"})
	req := httptest.NewRequest("POST", "/api/vault/task", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d", rr.Code)
	}
	var task Task
	_ = json.NewDecoder(rr.Body).Decode(&task)
	if task.ID == "" || task.UserID != "user1" {
		t.Fatalf("unexpected task: %+v", task)
	}
}

func TestCreateTask_DuplicateActiveTask(t *testing.T) {
	_, store, mux := setupTestServer()
	_, _ = store.TryAcquireUserActiveTask(nil, "user1", "task-1")
	body, _ := json.Marshal(TaskRequest{UserID: "user1", Instruction: "new task"})
	req := httptest.NewRequest("POST", "/api/vault/task", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d", rr.Code)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	_, _, mux := setupTestServer()
	req := httptest.NewRequest("GET", "/api/vault/task/notfound", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d", rr.Code)
	}
}

func TestCancelTask_Success(t *testing.T) {
	_, store, mux := setupTestServer()
	task := &Task{ID: "task-1", UserID: "user1", Instruction: "x", Status: TaskStatusRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = store.CreateTask(nil, task)
	_, _ = store.TryAcquireUserActiveTask(nil, "user1", "task-1")

	req := httptest.NewRequest("DELETE", "/api/vault/task/task-1", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d", rr.Code)
	}
	got, _ := store.GetTask(nil, "task-1")
	if got.Status != TaskStatusCancelled {
		t.Fatalf("status: got %s", got.Status)
	}
}

func TestRunTask_TransitionCompleted(t *testing.T) {
	exec := &mockExecutor{}
	store := NewMemoryTaskStore()
	h := NewTaskHandler(exec, store)
	task := &Task{ID: "task-1", UserID: "user1", Instruction: "x", Status: TaskStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = store.CreateTask(nil, task)
	_, _ = store.TryAcquireUserActiveTask(nil, "user1", "task-1")

	h.runTask("task-1")
	got, _ := store.GetTask(nil, "task-1")
	if got.Status != TaskStatusCompleted {
		t.Fatalf("status: got %s", got.Status)
	}
}

func TestRunTask_TransitionFailed(t *testing.T) {
	exec := &mockExecutor{executeErr: errors.New("boom")}
	store := NewMemoryTaskStore()
	h := NewTaskHandler(exec, store)
	task := &Task{ID: "task-1", UserID: "user1", Instruction: "x", Status: TaskStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	_ = store.CreateTask(nil, task)
	_, _ = store.TryAcquireUserActiveTask(nil, "user1", "task-1")

	h.runTask("task-1")
	got, _ := store.GetTask(nil, "task-1")
	if got.Status != TaskStatusFailed {
		t.Fatalf("status: got %s", got.Status)
	}
}
