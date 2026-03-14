package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

const maxRequestBodyBytes = 1 << 20 // 1 MB

// TaskStatus 任務狀態
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// TaskRequest 建立任務的請求
type TaskRequest struct {
	UserID      string `json:"userId"`
	Instruction string `json:"instruction"`
	Scope       string `json:"scope,omitempty"`
}

// Task 任務實體
type Task struct {
	ID          string     `json:"id"`
	UserID      string     `json:"userId"`
	Instruction string     `json:"instruction"`
	Scope       string     `json:"scope,omitempty"`
	Status      TaskStatus `json:"status"`
	Result      string     `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// TaskExecutor 任務執行器介面
type TaskExecutor interface {
	Execute(task *Task) error
	Cancel(taskID string) error
}

// TaskHandler REST API handler
type TaskHandler struct {
	mu       sync.Mutex
	executor TaskExecutor
	store    TaskStore
	ctx      context.Context
}

func NewTaskHandler(executor TaskExecutor, store TaskStore) *TaskHandler {
	return &TaskHandler{executor: executor, store: store, ctx: context.Background()}
}

// SetContext 設定可取消的 context（用於 graceful shutdown）
func (h *TaskHandler) SetContext(ctx context.Context) {
	h.ctx = ctx
}

func NewTaskHandlerWithMemory(executor TaskExecutor) *TaskHandler {
	return NewTaskHandler(executor, NewMemoryTaskStore())
}

func (h *TaskHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/vault/task", h.CreateTask)
	mux.HandleFunc("GET /api/vault/task/{id}", h.GetTask)
	mux.HandleFunc("DELETE /api/vault/task/{id}", h.CancelTask)
}

func (h *TaskHandler) CreateTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId is required")
		return
	}
	if req.Instruction == "" {
		writeError(w, http.StatusBadRequest, "instruction is required")
		return
	}

	// 驗證請求者身份：X-User-ID 存在時必須與 body 一致
	if hdr := r.Header.Get("X-User-ID"); hdr != "" && hdr != req.UserID {
		writeError(w, http.StatusForbidden, "userId does not match authenticated user")
		return
	}

	taskID, err := h.store.NextTaskID(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to allocate task id")
		return
	}
	acquired, err := h.store.TryAcquireUserActiveTask(r.Context(), req.UserID, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock user task")
		return
	}
	if !acquired {
		writeError(w, http.StatusConflict, "user already has an active task")
		return
	}

	now := time.Now()
	task := &Task{ID: taskID, UserID: req.UserID, Instruction: req.Instruction, Scope: req.Scope, Status: TaskStatusPending, CreatedAt: now, UpdatedAt: now}
	if err := h.store.CreateTask(r.Context(), task); err != nil {
		if relErr := h.store.ReleaseUserActiveTask(context.Background(), req.UserID, taskID); relErr != nil {
			log.Printf("[TaskHandler] ReleaseUserActiveTask error on CreateTask rollback: %v", relErr)
		}
		writeError(w, http.StatusInternalServerError, "failed to create task")
		return
	}
	go h.runTask(taskID, req.UserID)
	writeJSON(w, http.StatusCreated, task)
}

func (h *TaskHandler) GetTask(w http.ResponseWriter, r *http.Request) {
	task, err := h.store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read task")
		return
	}
	if task == nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if hdr := r.Header.Get("X-User-ID"); hdr != "" && hdr != task.UserID {
		writeError(w, http.StatusForbidden, "not the task owner")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *TaskHandler) CancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	h.mu.Lock()
	task, err := h.store.GetTask(r.Context(), id)
	if err != nil {
		h.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "failed to read task")
		return
	}
	if task == nil {
		h.mu.Unlock()
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	// 驗證請求者是否為任務擁有者
	reqUserID := r.Header.Get("X-User-ID")
	if reqUserID != "" && reqUserID != task.UserID {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "not the task owner")
		return
	}

	if task.Status != TaskStatusPending && task.Status != TaskStatusRunning {
		h.mu.Unlock()
		writeError(w, http.StatusBadRequest, "task is not active")
		return
	}
	task.Status = TaskStatusCancelled
	task.UpdatedAt = time.Now()
	if err := h.store.UpdateTask(r.Context(), task); err != nil {
		log.Printf("[TaskHandler] UpdateTask error in CancelTask: %v", err)
	}
	if err := h.store.ReleaseUserActiveTask(context.Background(), task.UserID, task.ID); err != nil {
		log.Printf("[TaskHandler] ReleaseUserActiveTask error in CancelTask: %v", err)
	}
	h.mu.Unlock()

	if err := h.executor.Cancel(id); err != nil {
		log.Printf("[TaskHandler] executor.Cancel error: %v", err)
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *TaskHandler) runTask(taskID, userID string) {
	ctx := h.ctx

	// 階段 1：在鎖內將狀態設為 running
	h.mu.Lock()
	task, err := h.store.GetTask(ctx, taskID)
	if err != nil || task == nil {
		if err != nil {
			log.Printf("[TaskHandler] GetTask error in runTask start: %v", err)
		}
		if relErr := h.store.ReleaseUserActiveTask(context.Background(), userID, taskID); relErr != nil {
			log.Printf("[TaskHandler] ReleaseUserActiveTask error (getTask failed): %v", relErr)
		}
		h.mu.Unlock()
		return
	}
	if task.Status == TaskStatusCancelled {
		if relErr := h.store.ReleaseUserActiveTask(context.Background(), userID, taskID); relErr != nil {
			log.Printf("[TaskHandler] ReleaseUserActiveTask error (cancelled early): %v", relErr)
		}
		h.mu.Unlock()
		return
	}
	task.Status = TaskStatusRunning
	task.UpdatedAt = time.Now()
	if err := h.store.UpdateTask(ctx, task); err != nil {
		log.Printf("[TaskHandler] UpdateTask(running) error: %v", err)
	}
	h.mu.Unlock()

	// 階段 2：不持鎖執行任務，允許 CancelTask 同時操作
	execErr := h.executor.Execute(task)

	// 階段 3：在鎖內檢查是否已被取消，設定最終狀態
	h.mu.Lock()
	defer h.mu.Unlock()

	latest, latestErr := h.store.GetTask(ctx, taskID)
	if latestErr != nil {
		log.Printf("[TaskHandler] GetTask error in runTask finish: %v", latestErr)
	}
	if latest == nil || latest.Status == TaskStatusCancelled {
		if err := h.store.ReleaseUserActiveTask(ctx, task.UserID, task.ID); err != nil {
			log.Printf("[TaskHandler] ReleaseUserActiveTask error (cancelled): %v", err)
		}
		return
	}

	if execErr != nil {
		task.Status = TaskStatusFailed
		task.Error = execErr.Error()
	} else {
		task.Status = TaskStatusCompleted
	}
	task.UpdatedAt = time.Now()
	if err := h.store.UpdateTask(ctx, task); err != nil {
		log.Printf("[TaskHandler] UpdateTask(final) error: %v", err)
	}
	if err := h.store.ReleaseUserActiveTask(ctx, task.UserID, task.ID); err != nil {
		log.Printf("[TaskHandler] ReleaseUserActiveTask error (final): %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
