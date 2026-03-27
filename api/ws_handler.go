package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	conn           *websocket.Conn
	memberID       string
	sessionID      string
	mode           string
	taskID         string
	status         string // idle, asking, interrupted
	cliSessionID   string // CLI session UUID for --resume after idle kill
	mu             sync.Mutex
	cli            *executor.PersistentCLI // persistent CLI process for this session
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
	sessionID := q.Get("sessionID")
	mode := q.Get("mode")
	token := q.Get("token")

	if memberID == "" {
		http.Error(w, "memberID required", 400)
		return
	}

	// Verify JWT: reject guest users
	if token == "" {
		http.Error(w, "token required", 401)
		return
	}
	jwtRole := extractJWTRole(token)
	if jwtRole == "guest" {
		http.Error(w, "guest users cannot use AI features", 403)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] upgrade error: %v", err)
		return
	}

	session := &WsSession{
		conn:      conn,
		memberID:  memberID,
		sessionID: sessionID,
		mode:      mode,
		status:    "idle",
	}
	sessionKey := memberID + ":" + sessionID
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

	session.Send(map[string]interface{}{
		"type":      "connected",
		"sessionId": sessionKey,
	})

	// Pre-warm: start persistent CLI in background (no resume on first connect)
	go func() {
		workDir := filepath.Join(h.vaultRoot, memberID)
		cli, err := executor.NewPersistentCLI(workDir, "chat", memberID, "", 5*time.Minute)
		if err != nil {
			log.Printf("[WS] CLI pre-start failed for %s: %v", memberID, err)
			return
		}
		session.mu.Lock()
		session.cli = cli
		session.mu.Unlock()
		log.Printf("[WS] CLI pre-started for session %s", sessionKey)
	}()

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
	// 0. Credits check — 額度不足就拒絕
	if err := h.checkCredits(session.memberID); err != nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    err.Error(),
		})
		return
	}

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
		SessionID: session.sessionID,
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

	// First message on this CLI → cache miss, show "正在準備 CLI 環境"
	// Subsequent messages → cache hit, skip (frontend shows "正在思考" by default)
	isCacheBuilding := !cli.CacheBuilt
	if isCacheBuilding {
		session.Send(map[string]interface{}{
			"type": "cli_preparing",
		})
	}

	// 4. Take vault snapshot BEFORE CLI runs (for diff + writeback)
	beforeSnap, beforeIDMap, snapErr := executor.TakeSnapshotAndPathIDMap(
		h.vaultFS, session.memberID,
	)
	if snapErr != nil {
		log.Printf("[WS] snapshot before error: %v", snapErr)
	}

	// 5. Send message to persistent CLI
	eventCh, err := cli.SendMessage(messageText)
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

	cliReadySent := false
	for event := range eventCh {
		if event.Type != "stdout" {
			continue
		}

		// First stdout event after cache building → send cli_ready, mark cache as built
		if isCacheBuilding && !cliReadySent {
			cliReadySent = true
			cli.CacheBuilt = true
			session.Send(map[string]interface{}{
				"type": "cli_ready",
			})
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
			// CLI result event contains: total_cost_usd, usage{input_tokens, output_tokens, ...}, modelUsage
			usageData := map[string]interface{}{}
			if cost, ok := parsed["total_cost_usd"]; ok {
				usageData["total_cost_usd"] = cost
			}
			if usage, ok := parsed["usage"].(map[string]interface{}); ok {
				usageData["input_tokens"] = usage["input_tokens"]
				usageData["output_tokens"] = usage["output_tokens"]
				usageData["cache_creation_input_tokens"] = usage["cache_creation_input_tokens"]
				usageData["cache_read_input_tokens"] = usage["cache_read_input_tokens"]
			}
			if mu, ok := parsed["modelUsage"]; ok {
				usageData["modelUsage"] = mu
			}
			if b, err := json.Marshal(usageData); err == nil && len(usageData) > 0 {
				tokenUsage = b
			}

			// 即時扣款（同步，不是 fire-and-forget）
			h.recordBilling(session.memberID, session.mode, session.sessionID, parsed)

			// 每個 turn 結束後檢查餘額，不足就 kill CLI 中斷任務
			if err := h.checkCredits(session.memberID); err != nil {
				log.Printf("[WS] credits exhausted mid-task for %s, killing CLI", session.memberID)
				session.mu.Lock()
				if session.cli != nil {
					session.cli.Kill()
				}
				session.mu.Unlock()
				session.Send(map[string]interface{}{
					"type":     "stream_error",
					"memberID": session.memberID,
					"error":    err.Error(),
				})
				// eventCh will close because CLI was killed
			}
		}
	}

	// 7. Save assistant message
	assistantMsg := &database.ChatMessage{
		SessionID:  session.sessionID,
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

	// 10. Generate session title (fire-and-forget)
	go h.maybeGenerateTitle(session, messageText, accumulatedText)
}

// ensureCLI returns a live PersistentCLI for the session, starting one if needed.
// If a previous CLI had a session ID, it rebuilds the .jsonl from DB and uses
// --resume to restore conversation context.
func (h *WsHandler) ensureCLI(session *WsSession) *executor.PersistentCLI {
	session.mu.Lock()
	cli := session.cli
	session.mu.Unlock()

	if cli != nil && cli.IsAlive() {
		return cli
	}

	// Capture session ID from old CLI before starting new one
	session.mu.Lock()
	if session.cli != nil {
		if sid := session.cli.SessionID(); sid != "" {
			session.cliSessionID = sid
		}
		session.cli.Kill()
	}
	resumeID := session.cliSessionID
	session.mu.Unlock()

	workDir := filepath.Join(h.vaultRoot, session.memberID)

	// Rebuild .jsonl before resuming so CLI has the full conversation history
	if resumeID != "" {
		h.rebuildSessionJSONL(session, resumeID, workDir)
	}

	// CLI not started or died — start synchronously (with resume if available)
	newCLI, err := executor.NewPersistentCLI(workDir, "chat", session.memberID, resumeID, 5*time.Minute)
	if err != nil {
		log.Printf("[WS] CLI start failed for %s: %v", session.memberID, err)
		return nil
	}

	session.mu.Lock()
	session.cli = newCLI
	session.mu.Unlock()

	return newCLI
}

// rebuildSessionJSONL queries chat messages from DB and writes the .jsonl file
// so that CLI --resume can restore the conversation.
func (h *WsHandler) rebuildSessionJSONL(session *WsSession, cliSessionID, workDir string) {
	msgs, _, err := h.chatStore.GetMessagesAfter(
		context.Background(), session.sessionID, session.mode, "", 200)
	if err != nil {
		log.Printf("[WS] rebuild JSONL: failed to query messages for session %s: %v",
			session.sessionID, err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	// Convert database.ChatMessage to executor.SessionMessage
	smList := make([]executor.SessionMessage, 0, len(msgs))
	for _, m := range msgs {
		smList = append(smList, executor.SessionMessage{
			ID:         m.ID,
			Role:       m.Role,
			Content:    m.Content,
			Thinking:   m.Thinking,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			CreatedAt:  m.CreatedAt,
		})
	}

	if err := executor.RebuildSessionJSONL(cliSessionID, workDir, session.memberID, smList); err != nil {
		log.Printf("[WS] rebuild JSONL error for session %s: %v", session.sessionID, err)
	} else {
		log.Printf("[WS] rebuilt JSONL for CLI session %s (%d messages)", cliSessionID, len(smList))
	}
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
	// Only generate title for the first message in a session
	msgs, _, err := h.chatStore.GetMessagesAfter(context.Background(), session.sessionID, session.mode, "", 5)
	if err != nil || len(msgs) > 2 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[WS] generating title for session %s via ai-service...", session.sessionID)

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
	h.chatStore.AddSessionMapping(context.Background(), session.memberID, session.sessionID, title, session.mode)

	session.Send(map[string]interface{}{
		"type":       "session_title_update",
		"memberID":   session.memberID,
		"session_id": session.sessionID,
		"title":      title,
	})
}

// GetSessionStatus returns the session status for a given member/session pair.
func (h *WsHandler) GetSessionStatus(memberID, sessionID string) string {
	key := memberID + ":" + sessionID
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

// checkCredits checks if the user has sufficient credits before processing.
// Calls MemberCenter's quota check API.
func (h *WsHandler) checkCredits(memberID string) error {
	mcURL := os.Getenv("MEMBERCENTER_URL")
	if mcURL == "" {
		mcURL = "http://membercenter.svc.local:3006"
	}

	reqBody, _ := json.Marshal(map[string]string{"memberId": memberID})
	resp, err := http.Post(mcURL+"/api/internal/quota/ai/check", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[WS] credits check failed (allowing): %v", err)
		return nil // fail open — don't block if check fails
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
			Balance float64 `json:"balance"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil // fail open
	}

	if result.Success && result.Data.Allowed == false {
		return fmt.Errorf("INSUFFICIENT_CREDITS: 額度不足 (餘額: %.2f)", result.Data.Balance)
	}
	return nil
}

// recordBilling sends usage data to Python billing API after CLI response.
func (h *WsHandler) recordBilling(memberID, mode, sessionID string, resultEvent map[string]interface{}) {
	aiServiceURL := os.Getenv("AI_SERVICE_URL")
	if aiServiceURL == "" {
		aiServiceURL = "http://chatbot.svc.local:8000"
	}

	usage, _ := resultEvent["usage"].(map[string]interface{})
	if usage == nil {
		return
	}

	inputTokens, _ := usage["input_tokens"].(float64)
	outputTokens, _ := usage["output_tokens"].(float64)
	cacheCreation, _ := usage["cache_creation_input_tokens"].(float64)
	cacheRead, _ := usage["cache_read_input_tokens"].(float64)

	// Extract model name from modelUsage
	modelName := "claude-sonnet-4-6"
	if mu, ok := resultEvent["modelUsage"].(map[string]interface{}); ok {
		for name := range mu {
			modelName = name
			break
		}
	}

	body := map[string]interface{}{
		"member_id":              memberID,
		"model":                  modelName,
		"input_tokens":           int(inputTokens),
		"output_tokens":          int(outputTokens),
		"cache_creation_tokens":  int(cacheCreation),
		"cache_read_tokens":      int(cacheRead),
		"category":               mode,
		"action":                 "response",
		"session_id":             sessionID,
		"mode":                   mode,
	}

	reqBody, _ := json.Marshal(body)
	resp, err := http.Post(aiServiceURL+"/api/billing/record", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[WS] billing record failed: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("[WS] billing recorded: model=%s in=%d out=%d cache_create=%d cache_read=%d",
		modelName, int(inputTokens), int(outputTokens), int(cacheCreation), int(cacheRead))
}

// extractJWTRole decodes the JWT payload (without verification) and returns the role claim.
func extractJWTRole(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Role
}
