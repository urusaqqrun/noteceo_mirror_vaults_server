package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urusaqqrun/vault-mirror-service/database"
	"github.com/urusaqqrun/vault-mirror-service/executor"
	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WsSession represents a single WebSocket connection session.
type WsSession struct {
	conn     *websocket.Conn
	memberID string
	threadID string
	mode     string
	taskID   string
	status   string // idle, asking, interrupted
	mu       sync.Mutex
	cli      *executor.PersistentCLI // persistent CLI process for this session
}

// Send writes a JSON message to the WebSocket connection (thread-safe).
func (s *WsSession) Send(msg map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(msg)
}

// StreamTaskExecutor is the interface for streaming task execution.
// ExecuteStream returns (vaultChanged, error).
type StreamTaskExecutor interface {
	ExecuteStream(task *Task, eventCh chan<- executor.StreamEvent) (bool, error)
	Cancel(taskID string) error
}

// WsHandler is the WebSocket endpoint handler.
type WsHandler struct {
	executor  StreamTaskExecutor
	store     TaskStore
	chatStore ChatStore
	vaultRoot string
	vaultFS   mirror.VaultFS
	sessions  sync.Map // sessionKey -> *WsSession
}

// NewWsHandler creates a new WsHandler.
func NewWsHandler(exec StreamTaskExecutor, store TaskStore, chatStore ChatStore, vaultRoot string, vaultFS mirror.VaultFS) *WsHandler {
	return &WsHandler{executor: exec, store: store, chatStore: chatStore, vaultRoot: vaultRoot, vaultFS: vaultFS}
}

// RegisterRoutes registers the WebSocket route on the provided mux.
func (h *WsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ws/chat", h.HandleWebSocket)
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and starts the read loop.
func (h *WsHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	memberID := q.Get("memberID")
	threadID := q.Get("threadID")
	mode := q.Get("mode")
	token := q.Get("token")

	if memberID == "" {
		http.Error(w, "memberID required", 400)
		return
	}

	// TODO: verify token (JWT)
	_ = token

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] upgrade error: %v", err)
		return
	}

	session := &WsSession{
		conn:     conn,
		memberID: memberID,
		threadID: threadID,
		mode:     mode,
		status:   "idle",
	}
	sessionKey := memberID + ":" + threadID
	h.sessions.Store(sessionKey, session)

	defer func() {
		// Kill persistent CLI on WebSocket close
		session.mu.Lock()
		if session.cli != nil {
			session.cli.Kill()
			session.cli = nil
		}
		session.mu.Unlock()
		h.sessions.Delete(sessionKey)
		conn.Close()
	}()

	// Pre-warm: start persistent CLI in background
	go func() {
		workDir := filepath.Join(h.vaultRoot, memberID)
		cli, err := executor.NewPersistentCLI(workDir, "chat", memberID, 5*time.Minute)
		if err != nil {
			log.Printf("[WS] CLI pre-start failed for %s: %v", memberID, err)
			return
		}
		session.mu.Lock()
		session.cli = cli
		session.mu.Unlock()
		log.Printf("[WS] CLI pre-started for session %s", sessionKey)
	}()

	session.Send(map[string]interface{}{
		"type":      "connected",
		"sessionId": sessionKey,
	})

	// Read loop
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		msgType, _ := msg["type"].(string)

		switch msgType {
		case "message":
			go h.handleMessage(session, sessionKey, msg)
		case "interrupt":
			h.handleInterrupt(session)
		case "ping":
			id, _ := msg["id"].(string)
			session.Send(map[string]interface{}{"type": "pong", "id": id})
		}
	}
}

func (h *WsHandler) handleMessage(session *WsSession, sessionKey string, msg map[string]interface{}) {
	// message 可能是 string（直接文字）或 object（{role, content}）
	messageText, _ := msg["message"].(string)
	if messageText == "" {
		if msgObj, ok := msg["message"].(map[string]interface{}); ok {
			messageText, _ = msgObj["content"].(string)
		}
	}
	if messageText == "" {
		return
	}

	session.mu.Lock()
	session.status = "asking"
	session.mu.Unlock()

	defer func() {
		session.mu.Lock()
		session.status = "idle"
		session.mu.Unlock()
	}()

	// 1. Save user message
	userMsg := &database.ChatMessage{
		ThreadID: session.threadID,
		Mode:     session.mode,
		Role:     "user",
		Content:  messageText,
	}
	if items, ok := msg["attachedItems"]; ok {
		if b, err := json.Marshal(items); err == nil {
			userMsg.AttachedItems = b
		}
	}
	h.chatStore.InsertChatMessage(context.Background(), userMsg)

	// 1b. mode-aware instruction wrapping
	cliInstruction := messageText
	if session.mode == "createCard" {
		aiServiceURL := os.Getenv("AI_SERVICE_URL")
		if aiServiceURL == "" {
			aiServiceURL = "http://chatbot.svc.local:8000"
		}
		cliInstruction = fmt.Sprintf(`請補完以下卡片的空白欄位。

規則：
1. 已填寫的欄位（非空字串）保持不變，不要覆蓋
2. IMAGE 欄位必須搜尋圖片 URL：curl -s -X POST %s/cubelv/search_card_image -H 'Content-Type: application/json' -d '{"query":"搜尋詞"}'
3. 空白欄位嘗試上網搜尋補全，找不到則留空
4. 完成後直接修改該卡片的 .json 檔案
5. 不要詢問用戶，直接執行

卡片資料：
%s`, aiServiceURL, messageText)
	}

	// 2. Ensure persistent CLI is alive
	cli := h.ensureCLI(session)
	if cli == nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    "failed to start CLI process",
		})
		return
	}

	// 3. stream_start
	session.Send(map[string]interface{}{
		"type":     "stream_start",
		"memberID": session.memberID,
	})

	// 4. Take vault snapshot BEFORE CLI runs (for diff + writeback)
	beforeSnap, beforeIDMap, snapErr := executor.TakeSnapshotAndPathIDMap(
		h.vaultFS, session.memberID,
	)
	if snapErr != nil {
		log.Printf("[WS] snapshot before error: %v", snapErr)
	}

	// 5. Send message to persistent CLI (use wrapped instruction for createCard mode)
	eventCh, err := cli.SendMessage(cliInstruction)
	if err != nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    fmt.Sprintf("CLI send error: %v", err),
		})
		return
	}

	// 6. Push events to WebSocket
	accumulatedText := ""
	accumulatedThinking := ""
	var tokenUsage json.RawMessage

	for event := range eventCh {
		if event.Type != "stdout" {
			continue
		}

		log.Printf("[WS-CLI] raw line: %s", event.Data[:min(len(event.Data), 200)])

		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
			log.Printf("[WS-CLI] parse error: %v for line: %s", err, event.Data[:min(len(event.Data), 100)])
			continue
		}

		eventType, _ := parsed["type"].(string)
		subtype, _ := parsed["subtype"].(string)

		switch {
		case eventType == "assistant":
			if msgContent, ok := parsed["message"].(map[string]interface{}); ok {
				if contentArr, ok := msgContent["content"].([]interface{}); ok {
					for _, block := range contentArr {
						blockMap, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						blockType, _ := blockMap["type"].(string)
						switch blockType {
						case "text":
							text, _ := blockMap["text"].(string)
							accumulatedText += text
							session.Send(map[string]interface{}{
								"type":        "stream_token",
								"memberID":    session.memberID,
								"token":       text,
								"accumulated": accumulatedText,
							})
						case "thinking":
							text, _ := blockMap["thinking"].(string)
							accumulatedThinking += text
							session.Send(map[string]interface{}{
								"type":        "thinking_token",
								"memberID":    session.memberID,
								"token":       text,
								"accumulated": accumulatedThinking,
							})
						case "tool_use":
							inputJSON, _ := json.Marshal(blockMap["input"])
							session.Send(map[string]interface{}{
								"type":    "message",
								"role":    "assistant",
								"content": "",
								"tool_calls": []map[string]interface{}{{
									"id":   blockMap["id"],
									"type": "function",
									"function": map[string]interface{}{
										"name":      blockMap["name"],
										"arguments": string(inputJSON),
									},
								}},
							})
						}
					}
				}
			}

		case eventType == "result" && subtype == "tool_result":
			session.Send(map[string]interface{}{
				"type":         "message",
				"role":         "tool",
				"content":      parsed["content"],
				"tool_call_id": parsed["tool_use_id"],
			})

		case eventType == "result" && subtype == "success":
			if tu, ok := parsed["cost_usd"]; ok {
				if b, err := json.Marshal(map[string]interface{}{"cost_usd": tu}); err == nil {
					tokenUsage = b
				}
			}
		}
	}

	// 7. Save assistant message
	assistantMsg := &database.ChatMessage{
		ThreadID:   session.threadID,
		Mode:       session.mode,
		Role:       "assistant",
		Content:    accumulatedText,
		Thinking:   accumulatedThinking,
		TokenUsage: tokenUsage,
	}
	h.chatStore.InsertChatMessage(context.Background(), assistantMsg)

	// 8. stream_end
	session.Send(map[string]interface{}{
		"type":           "stream_end",
		"memberID":       session.memberID,
		"final_response": accumulatedText,
		"checkpoint_id":  assistantMsg.ID,
		"token_usage":    tokenUsage,
	})

	// 9. Vault diff + writeback (post-CLI)
	if snapErr == nil {
		vaultChanged := h.diffAndNotify(session, beforeSnap, beforeIDMap)
		if vaultChanged {
			session.Send(map[string]interface{}{
				"type":     "vault_changed",
				"memberID": session.memberID,
			})
		}
	}

	// 10. Generate thread title (fire-and-forget)
	go h.maybeGenerateTitle(session, messageText, accumulatedText)
}

// ensureCLI returns a live PersistentCLI for the session, starting one if needed.
func (h *WsHandler) ensureCLI(session *WsSession) *executor.PersistentCLI {
	session.mu.Lock()
	cli := session.cli
	session.mu.Unlock()

	if cli != nil && cli.IsAlive() {
		return cli
	}

	// CLI not started or died — start synchronously
	workDir := filepath.Join(h.vaultRoot, session.memberID)
	newCLI, err := executor.NewPersistentCLI(workDir, "chat", session.memberID, 5*time.Minute)
	if err != nil {
		log.Printf("[WS] CLI start failed for %s: %v", session.memberID, err)
		return nil
	}

	session.mu.Lock()
	// Kill old CLI if it existed
	if session.cli != nil {
		session.cli.Kill()
	}
	session.cli = newCLI
	session.mu.Unlock()

	return newCLI
}

// diffAndNotify computes the vault diff after CLI execution and triggers writeback.
// Returns true if the vault changed.
func (h *WsHandler) diffAndNotify(
	session *WsSession,
	beforeSnap map[string]executor.FileSnapshot,
	beforeIDMap map[string]string,
) bool {
	afterSnap, err := executor.TakeSnapshot(h.vaultFS, session.memberID)
	if err != nil {
		log.Printf("[WS] snapshot after error: %v", err)
		return false
	}

	diff := executor.ComputeDiff(beforeSnap, afterSnap)
	hasChanges := len(diff.Created)+len(diff.Modified)+len(diff.Deleted)+len(diff.Moved) > 0
	if !hasChanges {
		return false
	}

	log.Printf("[WS-Persistent] diff: +%d ~%d -%d mv%d",
		len(diff.Created), len(diff.Modified), len(diff.Deleted), len(diff.Moved))

	// Writeback is delegated to the fullTaskExecutor via ExecuteStream in the
	// non-persistent path. For persistent CLI, the ws_handler signals
	// vault_changed and the caller (main.go fullTaskExecutor) is not involved.
	// The actual DB writeback for persistent mode will be handled by the sync
	// worker picking up filesystem changes.
	return true
}


func (h *WsHandler) handleInterrupt(session *WsSession) {
	session.mu.Lock()
	taskID := session.taskID
	session.mu.Unlock()

	if taskID != "" {
		h.executor.Cancel(taskID)
	}
	session.Send(map[string]interface{}{
		"type":     "stream_interrupted",
		"memberID": session.memberID,
		"message":  "已中斷",
	})
}

func (h *WsHandler) maybeGenerateTitle(session *WsSession, userMsg, assistantMsg string) {
	// Only generate title for the first message in a thread
	msgs, _, err := h.chatStore.GetMessagesAfter(context.Background(), session.threadID, session.mode, "", 5)
	if err != nil || len(msgs) > 2 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[WS] generating title for thread %s via ai-service...", session.threadID)

	// Call Python ai-service's generate_thread_title endpoint (uses GPT-4o-mini)
	aiServiceURL := os.Getenv("AI_SERVICE_URL")
	if aiServiceURL == "" {
		aiServiceURL = "http://chatbot.svc.local:8000"
	}

	reqBody, _ := json.Marshal(map[string]interface{}{
		"userMessage":      userMsg,
		"assistantMessage": assistantMsg,
		"lang":             "繁體中文",
	})

	req, err := http.NewRequestWithContext(ctx, "POST", aiServiceURL+"/cubelv/generate_thread_title", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[WS] title request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[WS] title API error: %v", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Title == "" {
		log.Printf("[WS] title parse error: %v", err)
		return
	}

	title := result.Title
	if title == "" {
		return
	}

	log.Printf("[WS] generated title: %s", title)
	h.chatStore.AddThreadMapping(context.Background(), session.memberID, session.threadID, title, session.mode)

	session.Send(map[string]interface{}{
		"type":      "thread_title_update",
		"memberID":  session.memberID,
		"thread_id": session.threadID,
		"title":     title,
	})
}

// GetSessionStatus returns the session status for a given member/thread pair.
func (h *WsHandler) GetSessionStatus(memberID, threadID string) string {
	key := memberID + ":" + threadID
	if v, ok := h.sessions.Load(key); ok {
		s := v.(*WsSession)
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.status
	}
	return "idle"
}

func buildInstruction(messageText string, msg map[string]interface{}) string {
	// Simple version; can be extended for attachment handling
	return messageText
}

func formatToolCall(event map[string]interface{}) map[string]interface{} {
	input, _ := json.Marshal(event["input"])
	return map[string]interface{}{
		"id":   event["tool_use_id"],
		"type": "function",
		"function": map[string]interface{}{
			"name":      event["tool"],
			"arguments": string(input),
		},
	}
}
