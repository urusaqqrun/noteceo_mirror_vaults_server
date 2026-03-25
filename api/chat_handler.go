package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/urusaqqrun/vault-mirror-service/database"
)

// ChatStore defines the database operations required by ChatHandler.
// PgStore satisfies this interface.
type ChatStore interface {
	GetMessagesAfter(ctx context.Context, threadID, mode, cursorID string, limit int) ([]database.ChatMessage, bool, error)
	GetMessagesBefore(ctx context.Context, threadID, mode, cursorID string, limit int) ([]database.ChatMessage, bool, error)
	GetThreadsByMemberID(ctx context.Context, memberID, mode string) ([]database.ThreadInfo, error)
	AddThreadMapping(ctx context.Context, memberID, threadID, title, mode string) error
	DeleteUserThreads(ctx context.Context, memberID string) (int, int, error)
}

// ChatHandler serves the chat-related REST API endpoints.
type ChatHandler struct {
	store ChatStore
}

// NewChatHandler creates a ChatHandler backed by the given store.
func NewChatHandler(store ChatStore) *ChatHandler {
	return &ChatHandler{store: store}
}

// RegisterRoutes registers all chat-related routes on the provided mux.
func (h *ChatHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /get_messages_after_checkpoint", h.GetMessagesAfterCheckpoint)
	mux.HandleFunc("POST /get_messages_before_checkpoint", h.GetMessagesBeforeCheckpoint)
	mux.HandleFunc("POST /get_threads", h.GetThreads)
	mux.HandleFunc("POST /add_thread", h.AddThread)
	mux.HandleFunc("POST /get_session_status", h.GetSessionStatus)
	mux.HandleFunc("DELETE /api/internal/user/{member_id}/threads", h.DeleteUserThreads)
}

// GetMessagesAfterCheckpoint returns messages after a checkpoint (cursor).
// When checkpointID is empty, the latest N messages are returned.
func (h *ChatHandler) GetMessagesAfterCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID     string `json:"threadID"`
		Mode         string `json:"mode"`
		CheckpointID string `json:"checkpointID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	rows, hasPrevious, err := h.store.GetMessagesAfter(r.Context(), req.ThreadID, req.Mode, req.CheckpointID, 50)
	if err != nil {
		log.Printf("[ChatHandler] GetMessagesAfter error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	messages := formatMessages(rows)
	resp := map[string]interface{}{"messages": messages}
	if req.CheckpointID == "" {
		hp := 0
		if hasPrevious {
			hp = 1
		}
		resp["has_previous"] = hp
	}
	chatWriteJSON(w, http.StatusOK, resp)
}

// GetMessagesBeforeCheckpoint returns messages before a checkpoint (cursor).
func (h *ChatHandler) GetMessagesBeforeCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ThreadID     string `json:"threadID"`
		Mode         string `json:"mode"`
		CheckpointID string `json:"checkpointID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	rows, hasPrevious, err := h.store.GetMessagesBefore(r.Context(), req.ThreadID, req.Mode, req.CheckpointID, 50)
	if err != nil {
		log.Printf("[ChatHandler] GetMessagesBefore error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	hp := 0
	if hasPrevious {
		hp = 1
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{
		"messages":     formatMessages(rows),
		"has_previous": hp,
	})
}

// GetThreads returns the list of threads for a member.
func (h *ChatHandler) GetThreads(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemberID string `json:"memberID"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	threads, err := h.store.GetThreadsByMemberID(r.Context(), req.MemberID, req.Mode)
	if err != nil {
		log.Printf("[ChatHandler] GetThreads error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{"threads": threads})
}

// AddThread creates or updates a thread mapping.
func (h *ChatHandler) AddThread(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemberID    string `json:"memberID"`
		ThreadID    string `json:"threadID"`
		ThreadTitle string `json:"threadTitle"`
		Mode        string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.store.AddThreadMapping(r.Context(), req.MemberID, req.ThreadID, req.ThreadTitle, req.Mode)
	if err != nil {
		log.Printf("[ChatHandler] AddThread error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// GetSessionStatus returns the session status for a member/thread.
// Phase 2 will implement WebSocket session management; for now, always returns "idle".
func (h *ChatHandler) GetSessionStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemberID string `json:"memberID"`
		ThreadID string `json:"threadID"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	chatWriteJSON(w, http.StatusOK, map[string]string{"status": "idle"})
}

// DeleteUserThreads deletes all threads and chat messages for a user.
func (h *ChatHandler) DeleteUserThreads(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("member_id")
	threadsDeleted, msgsDeleted, err := h.store.DeleteUserThreads(r.Context(), memberID)
	if err != nil {
		log.Printf("[ChatHandler] DeleteUserThreads error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{
		"success":            true,
		"threadsDeleted":     threadsDeleted,
		"checkpointsDeleted": msgsDeleted,
	})
}

// ---------------------------------------------------------------------------
// Helpers (chat_handler-local to avoid collisions with task_handler helpers)
// ---------------------------------------------------------------------------

func chatWriteJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func chatWriteError(w http.ResponseWriter, status int, msg string) {
	chatWriteJSON(w, status, map[string]string{"error": msg})
}

// formatMessages converts []ChatMessage into the frontend-expected format.
func formatMessages(rows []database.ChatMessage) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		msg := map[string]interface{}{
			"role":    row.Role,
			"content": row.Content,
		}
		if row.Role == "assistant" {
			msg["checkpoint_id"] = row.ID
			if row.Thinking != "" {
				msg["thinking"] = row.Thinking
			}
			if len(row.ToolCalls) > 0 {
				msg["tool_calls"] = row.ToolCalls
			}
		}
		if row.Role == "tool" && row.ToolCallID != "" {
			msg["tool_call_id"] = row.ToolCallID
		}
		if len(row.AttachedItems) > 0 {
			msg["attachedItems"] = row.AttachedItems
		}
		result = append(result, msg)
	}
	return result
}
