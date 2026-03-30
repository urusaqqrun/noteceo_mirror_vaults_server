package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/urusaqqrun/vault-mirror-service/database"
	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// ChatStore defines the database operations required by ChatHandler and WsHandler.
// PgStore satisfies this interface.
type ChatStore interface {
	GetMessagesAfter(ctx context.Context, sessionID, mode, cursorID string, limit int) ([]database.ChatMessage, bool, error)
	GetMessagesBefore(ctx context.Context, sessionID, mode, cursorID string, limit int) ([]database.ChatMessage, bool, error)
	GetSessionsByMemberID(ctx context.Context, memberID, mode string) ([]database.SessionInfo, error)
	AddSessionMapping(ctx context.Context, memberID, sessionID, title, mode string) error
	DeleteUserSessions(ctx context.Context, memberID string) (int, int, error)
	InsertChatMessage(ctx context.Context, msg *database.ChatMessage) error
}

// ChatHandler serves the chat-related REST API endpoints.
type ChatHandler struct {
	store     ChatStore
	wsHandler *WsHandler
	vaultFS   mirror.VaultFS
}

// NewChatHandler creates a ChatHandler backed by the given store.
func NewChatHandler(store ChatStore, vaultFS mirror.VaultFS) *ChatHandler {
	return &ChatHandler{store: store, vaultFS: vaultFS}
}

// SetWsHandler sets the WebSocket handler for session status lookups.
func (h *ChatHandler) SetWsHandler(ws *WsHandler) {
	h.wsHandler = ws
}

// RegisterRoutes registers all chat-related routes on the provided mux.
func (h *ChatHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /get_messages_after_checkpoint", h.GetMessagesAfterCheckpoint)
	mux.HandleFunc("POST /get_messages_before_checkpoint", h.GetMessagesBeforeCheckpoint)
	mux.HandleFunc("POST /get_sessions", h.GetSessions)
	mux.HandleFunc("POST /add_session", h.AddSession)
	mux.HandleFunc("POST /get_session_status", h.GetSessionStatus)
	mux.HandleFunc("DELETE /api/internal/user/{member_id}/data", h.DeleteUserData)
}

// memberIDFromHeader 從 Nginx 注入的 X-User-ID header 取得已驗證的 memberID。
func memberIDFromHeader(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := r.Header.Get("X-User-ID")
	if uid == "" {
		chatWriteError(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return uid, true
}

// GetMessagesAfterCheckpoint returns messages after a checkpoint (cursor).
// When checkpointID is empty, the latest N messages are returned.
func (h *ChatHandler) GetMessagesAfterCheckpoint(w http.ResponseWriter, r *http.Request) {
	if _, ok := memberIDFromHeader(w, r); !ok {
		return
	}
	var req struct {
		SessionID    string `json:"sessionID"`
		Mode         string `json:"mode"`
		CheckpointID string `json:"checkpointID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	rows, hasPrevious, err := h.store.GetMessagesAfter(r.Context(), req.SessionID, req.Mode, req.CheckpointID, 50)
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
	if _, ok := memberIDFromHeader(w, r); !ok {
		return
	}
	var req struct {
		SessionID    string `json:"sessionID"`
		Mode         string `json:"mode"`
		CheckpointID string `json:"checkpointID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	rows, hasPrevious, err := h.store.GetMessagesBefore(r.Context(), req.SessionID, req.Mode, req.CheckpointID, 50)
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

// GetSessions returns the list of sessions for a member.
func (h *ChatHandler) GetSessions(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	sessions, err := h.store.GetSessionsByMemberID(r.Context(), memberID, req.Mode)
	if err != nil {
		log.Printf("[ChatHandler] GetSessions error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{"sessions": sessions})
}

// AddSession creates or updates a session mapping.
func (h *ChatHandler) AddSession(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID    string `json:"sessionID"`
		SessionTitle string `json:"sessionTitle"`
		Mode         string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	err := h.store.AddSessionMapping(r.Context(), memberID, req.SessionID, req.SessionTitle, req.Mode)
	if err != nil {
		log.Printf("[ChatHandler] AddSession error: %v", err)
		chatWriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// GetSessionStatus returns the session status for a member/session.
// When a WsHandler is configured, it queries the live WebSocket session;
// otherwise it returns "idle".
func (h *ChatHandler) GetSessionStatus(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	var req struct {
		SessionID string `json:"sessionID"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	status := "idle"
	if h.wsHandler != nil {
		status = h.wsHandler.GetSessionStatus(memberID, req.SessionID)
	}
	chatWriteJSON(w, http.StatusOK, map[string]string{"status": status})
}

// DeleteUserData deletes all user data: sessions, chat messages, and vault files.
// Called when a user deletes their account from MemberCenter.
func (h *ChatHandler) DeleteUserData(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("member_id")
	log.Printf("[ChatHandler] DeleteUserData: cleaning up member %s", memberID)

	// 1. Delete sessions + chat messages from DB
	sessionsDeleted, msgsDeleted, err := h.store.DeleteUserSessions(r.Context(), memberID)
	if err != nil {
		log.Printf("[ChatHandler] DeleteUserSessions error: %v", err)
	}

	// 2. Delete vault directory on EFS
	vaultDeleted := false
	if h.vaultFS != nil {
		if err := h.vaultFS.RemoveAll(memberID); err != nil {
			log.Printf("[ChatHandler] DeleteVault error for %s: %v", memberID, err)
		} else {
			vaultDeleted = true
			log.Printf("[ChatHandler] Vault deleted for %s", memberID)
		}
	}

	chatWriteJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"sessionsDeleted": sessionsDeleted,
		"messagesDeleted": msgsDeleted,
		"vaultDeleted":    vaultDeleted,
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
// 只回傳 user-visible 的欄位，不回傳 thinking、tool_calls 等內部資料。
// tool 訊息中包含 noteID 的轉換為 note_embed 格式，其餘 tool 訊息不回傳。
func formatMessages(rows []database.ChatMessage) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		if row.Role == "tool" {
			var toolData map[string]interface{}
			if json.Unmarshal([]byte(row.Content), &toolData) == nil {
				if noteID, ok := toolData["noteID"].(string); ok && noteID != "" {
					matchedFolders := toolData["matchedFolderNames"]
					matchedJSON, _ := json.Marshal(matchedFolders)
					if matchedJSON == nil {
						matchedJSON = []byte("[]")
					}
					content := `<note-embed note-id="` + noteID + `"></note-embed><note-folder-selector note-id="` + noteID + `" matched-folders='` + string(matchedJSON) + `'></note-folder-selector>`
					result = append(result, map[string]interface{}{
						"role":    "note_embed",
						"content": content,
					})
				}
			}
			continue
		}
		msg := map[string]interface{}{
			"role":    row.Role,
			"content": row.Content,
		}
		if row.Role == "assistant" {
			msg["checkpoint_id"] = row.ID
		}
		if len(row.AttachedItems) > 0 {
			msg["attachedItems"] = row.AttachedItems
		}
		result = append(result, msg)
	}
	return result
}
