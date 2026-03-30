package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/urusaqqrun/vault-mirror-service/database"
	"github.com/urusaqqrun/vault-mirror-service/executor"
	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	wsPingInterval = 30 * time.Second
	wsPongWait     = 90 * time.Second
	wsWriteWait    = 10 * time.Second
)

// WsSession represents a single WebSocket connection session.
type WsSession struct {
	conn      *websocket.Conn
	memberID  string
	sessionID string
	mode      string
	model     string
	taskID    string
	status    string // idle, asking, interrupted
	mu        sync.Mutex
	cli       *executor.StreamCLI
	done      chan struct{} // 關閉時通知心跳 goroutine 停止
}

// Send writes a JSON message to the WebSocket connection (thread-safe).
func (s *WsSession) Send(msg map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(msg)
}

// SnapshotStore DB-based vault snapshot CRUD.
type SnapshotStore interface {
	GetSnapshot(ctx context.Context, memberID string) ([]database.SnapshotRow, error)
	UpsertSnapshotFiles(ctx context.Context, memberID string, files []database.SnapshotRow) error
	DeleteSnapshotFiles(ctx context.Context, memberID string, paths []string) error
	ReplaceSnapshot(ctx context.Context, memberID string, files []database.SnapshotRow) error
	SnapshotExists(ctx context.Context, memberID string) (bool, error)
}

// warmCLI 預熱池中的 CLI，附帶啟動時使用的模型
type warmCLI struct {
	cli   *executor.StreamCLI
	model string
}

// cachedSnapshot 記憶體中快取的 snapshot（避免每句都查 DB）
type cachedSnapshot struct {
	snap  map[string]executor.FileSnapshot
	idMap map[string]string
}

// WsHandler is the WebSocket endpoint handler.
type WsHandler struct {
	chatStore     ChatStore
	snapshotStore SnapshotStore
	itemWriter    executor.DataWriter
	vaultRoot     string
	vaultFS       mirror.VaultFS
	sessions      sync.Map // sessionKey -> *WsSession
	cliPool       sync.Map // memberID -> *warmCLI (warmup pool)
	snapCache     sync.Map // memberID -> *cachedSnapshot
}

// NewWsHandler creates a new WsHandler.
func NewWsHandler(chatStore ChatStore, snapshotStore SnapshotStore, itemWriter executor.DataWriter, vaultRoot string, vaultFS mirror.VaultFS) *WsHandler {
	return &WsHandler{
		chatStore:     chatStore,
		snapshotStore: snapshotStore,
		itemWriter:    itemWriter,
		vaultRoot:     vaultRoot,
		vaultFS:       vaultFS,
	}
}

// RegisterRoutes registers the WebSocket route on the provided mux.
func (h *WsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ws/chat", h.HandleWebSocket)
	mux.HandleFunc("POST /cli_warmup", h.HandleWarmup)
	mux.HandleFunc("POST /api/internal/forge", h.HandleForgeAPI)
}

// HandleForgeAPI 接收來自 MCP plugin-forge 的插件鍛造請求
// 啟動 Sub-Agent，透過 WebSocket 串流意圖事件，完成後回傳 JSON 結果
func (h *WsHandler) HandleForgeAPI(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string `json:"title"`
		Prompt      string `json:"prompt"`
		MemberID    string `json:"memberID"`
		WsSessionID string `json:"wsSessionID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Title == "" || req.Prompt == "" || req.MemberID == "" {
		http.Error(w, "title, prompt, memberID required", http.StatusBadRequest)
		return
	}

	log.Printf("[ForgeAPI] received forge request: title=%q member=%s", req.Title, req.MemberID)

	// 找到對應的 WebSocket session 以發送意圖事件
	var session *WsSession
	sessionCount := 0
	h.sessions.Range(func(key, value interface{}) bool {
		sessionCount++
		if s, ok := value.(*WsSession); ok {
			log.Printf("[ForgeAPI] checking session key=%v memberID=%s", key, s.memberID)
			if s.memberID == req.MemberID {
				session = s
				return false
			}
		}
		return true
	})

	if session == nil {
		log.Printf("[ForgeAPI] no active WebSocket session for member %s (checked %d sessions)", req.MemberID, sessionCount)
	} else {
		log.Printf("[ForgeAPI] found WebSocket session for member %s", req.MemberID)
	}

	// 執行插件鍛造（同步，阻塞到完成）
	result := h.executePluginForge(session, req.MemberID, req.Title, req.Prompt)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// HandleWarmup pre-warms a CLI process into the pool.
// 認證由 Nginx auth_request 處理，memberID 從 X-User-ID header 取得。
func (h *WsHandler) HandleWarmup(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, "unauthorized", 401)
		return
	}
	model := r.URL.Query().Get("model")

	if val, ok := h.cliPool.Load(memberID); ok {
		wc := val.(*warmCLI)
		if wc.cli.IsAlive() && wc.model == model {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]string{"status": "already_warm"})
			return
		}
		if wc.cli.IsAlive() {
			wc.cli.Kill()
		}
		h.cliPool.Delete(memberID)
	}

	go func() {
		warmupStart := time.Now()
		workDir := filepath.Join(h.vaultRoot, memberID)
		cli, err := executor.NewStreamCLI(workDir, "chat", memberID, "", model, false, 5*time.Minute)
		if err != nil {
			log.Printf("[Warmup] CLI start failed for %s: %v", memberID, err)
			return
		}
		h.cliPool.Store(memberID, &warmCLI{cli: cli, model: model})
		log.Printf("[CacheProfile] Warmup DONE — %dms, member=%s, model=%s, pid=%d",
			time.Since(warmupStart).Milliseconds(), memberID, model, cli.Pid())
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]string{"status": "warming"})
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and starts the read loop.
func (h *WsHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("sessionID")
	mode := q.Get("mode")
	model := q.Get("model")
	token := q.Get("token")
	timezone := q.Get("timezone")
	lang := q.Get("lang")

	if token == "" {
		http.Error(w, "token required", 401)
		return
	}
	token = strings.TrimPrefix(token, "Bearer ")

	claims, err := verifyJWT(token)
	if err != nil {
		log.Printf("[WS] JWT verification failed: %v", err)
		http.Error(w, "invalid token", 401)
		return
	}
	memberID := claims.UserID

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
		model:     model,
		status:    "idle",
		done:      make(chan struct{}),
	}
	sessionKey := memberID + ":" + sessionID
	h.sessions.Store(sessionKey, session)

	// pong handler：收到 pong 就延長 read deadline（只在 asking 狀態下有意義）
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})

	defer func() {
		close(session.done)
		session.mu.Lock()
		if session.cli != nil {
			session.cli.Kill()
			session.cli = nil
		}
		session.mu.Unlock()
		h.sessions.Delete(sessionKey)
		conn.Close()
	}()

	// 心跳 goroutine：只在 AI 回應中（asking）才發 ping，閒置時不打
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				session.mu.Lock()
				if session.status == "asking" {
					err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
					session.mu.Unlock()
					if err != nil {
						return
					}
				} else {
					session.mu.Unlock()
				}
			case <-session.done:
				return
			}
		}
	}()

	session.Send(map[string]interface{}{
		"type":      "connected",
		"sessionId": sessionKey,
	})

	isNew := q.Get("isNew") == "true"

	// Inject timezone & lang into user CLAUDE.md
	if timezone != "" || lang != "" {
		h.updateUserLocale(memberID, timezone, lang)
	}

	go func() {
		wsCliStart := time.Now()
		workDir := filepath.Join(h.vaultRoot, memberID)

		// 限制同一 user 最多 3 個活躍 CLI，超過時 kill 最舊的
		h.enforceMaxCLIs(memberID, 3)

		if isNew {
			if val, loaded := h.cliPool.LoadAndDelete(memberID); loaded {
				wc := val.(*warmCLI)
				if wc.cli.IsAlive() && wc.model == model {
					session.mu.Lock()
					session.cli = wc.cli
					session.mu.Unlock()
					log.Printf("[CacheProfile] WS goroutine: reused warm CLI — %dms, model=%s, pid=%d",
						time.Since(wsCliStart).Milliseconds(), model, wc.cli.Pid())
					return
				}
				if wc.cli.IsAlive() {
					wc.cli.Kill()
				}
				log.Printf("[WS] warmup model mismatch (pool=%s, req=%s), creating new CLI", wc.model, model)
			}
			if sessionID != "" {
				executor.CleanupSessionJSONL(sessionID, workDir)
			}
			cli, err := executor.NewStreamCLI(workDir, "chat", memberID, sessionID, model, false, 5*time.Minute)
			if err != nil {
				log.Printf("[WS] CLI start failed for %s: %v", memberID, err)
				return
			}
			session.mu.Lock()
			session.cli = cli
			session.mu.Unlock()
			log.Printf("[CacheProfile] WS goroutine: new CLI for new session — %dms, pid=%d",
				time.Since(wsCliStart).Milliseconds(), cli.Pid())
			return
		}

		if sessionID != "" {
			queryStart := time.Now()
			msgs, _, err := h.chatStore.GetMessagesAfter(
				context.Background(), sessionID, mode, "", 200)
			log.Printf("[CacheProfile] WS goroutine: GetMessagesAfter — %dms, count=%d",
				time.Since(queryStart).Milliseconds(), len(msgs))
			if err == nil && len(msgs) > 0 {
				smList := make([]executor.SessionMessage, 0, len(msgs))
				for _, m := range msgs {
					smList = append(smList, executor.SessionMessage{
						ID:            m.ID,
						Role:          m.Role,
						Content:       m.Content,
						Thinking:      m.Thinking,
						ToolCalls:     m.ToolCalls,
						ToolCallID:    m.ToolCallID,
						AttachedItems: m.AttachedItems,
						CreatedAt:     m.CreatedAt,
					})
				}

				rebuildStart := time.Now()
				if err := executor.RebuildSessionJSONL(sessionID, workDir, memberID, smList); err != nil {
					log.Printf("[WS] rebuild JSONL error: %v", err)
				} else {
					log.Printf("[CacheProfile] WS goroutine: RebuildSessionJSONL — %dms, msgs=%d",
						time.Since(rebuildStart).Milliseconds(), len(smList))
				}

			cli, err := executor.NewStreamCLI(workDir, "chat", memberID, sessionID, model, true, 5*time.Minute)
			if err != nil {
				log.Printf("[WS] CLI resume start failed for %s: %v", memberID, err)
					return
				}
				session.mu.Lock()
				session.cli = cli
				session.mu.Unlock()
				log.Printf("[CacheProfile] WS goroutine: CLI resumed — total %dms, session=%s",
					time.Since(wsCliStart).Milliseconds(), sessionID)
				return
			}
		}

		if sessionID != "" {
			executor.CleanupSessionJSONL(sessionID, workDir)
		}
		cli, err := executor.NewStreamCLI(workDir, "chat", memberID, sessionID, model, false, 5*time.Minute)
		if err != nil {
			log.Printf("[WS] CLI start failed for %s: %v", memberID, err)
			return
		}
		session.mu.Lock()
		session.cli = cli
		session.mu.Unlock()
		log.Printf("[CacheProfile] WS goroutine: new CLI (no history) — %dms, pid=%d",
			time.Since(wsCliStart).Milliseconds(), cli.Pid())
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

// toolNameToPhase 將 tool 名稱映射為高層級階段（前端根據 phase 決定顯示文案）
func toolNameToPhase(toolName string, input map[string]interface{}) string {
	action, _ := input["action"].(string)
	actionLower := strings.ToLower(action)

	switch toolName {
	case "think_purpose":
		return "thinking"
	case "Read", "Grep", "Glob", "get_tree", "get_folders", "get_notes",
		"get_card_type", "get_card", "get_chart_type", "get_chart", "TodoRead":
		return "reading"
	case "web_search", "fetch", "search_mergeable_note_in_folder":
		return "searching"
	case "Write", "create_note", "create_card_type", "create_chart_type", "create_todo":
		return "creating"
	case "Edit", "MultiEdit", "merge_notes", "edit_todo", "TodoWrite":
		return "editing"
	case "edit_note", "edit_folder", "edit_folder_tree", "edit_task", "report_task",
		"edit_card", "edit_card_group", "edit_chart", "edit_chart_type", "edit_card_type":
		switch actionLower {
		case "create", "create_group":
			return "creating"
		case "delete", "delete_group":
			return "deleting"
		default:
			return "editing"
		}
	case "Bash":
		return "working"
	default:
		return "working"
	}
}

func (h *WsHandler) handleMessage(session *WsSession, sessionKey string, msg map[string]interface{}) {
	handleStart := time.Now()
	log.Printf("[CacheProfile] handleMessage START — member=%s session=%s", session.memberID, session.sessionID)

	if err := h.checkCredits(session.memberID); err != nil {
		session.Send(map[string]interface{}{
			"type":     "turn_error",
			"memberID": session.memberID,
			"error":    err.Error(),
		})
		return
	}

	// 解析 message 物件
	var msgObj map[string]interface{}
	messageText, _ := msg["message"].(string)
	if messageText == "" {
		msgObj, _ = msg["message"].(map[string]interface{})
		if msgObj != nil {
			messageText, _ = msgObj["content"].(string)
		}
	}
	if messageText == "" {
		return
	}

	// noSave: 跳過所有 DB 寫入（用於卡片自動補完等一次性操作）
	noSave := false
	if msgObj != nil {
		noSave, _ = msgObj["noSave"].(bool)
	}

	session.mu.Lock()
	session.status = "asking"
	session.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	session.mu.Unlock()

	defer func() {
		session.mu.Lock()
		session.status = "idle"
		session.conn.SetReadDeadline(time.Time{})
		session.mu.Unlock()
	}()

	// 1. Save user message
	if !noSave {
		userMsg := &database.ChatMessage{
			SessionID: session.sessionID,
			Mode:      session.mode,
			Role:      "user",
			Content:   messageText,
		}
		if msgObj != nil {
			if items, ok := msgObj["attachedItems"]; ok {
				if b, err := json.Marshal(items); err == nil {
					userMsg.AttachedItems = b
				}
			}
		}
		h.chatStore.InsertChatMessage(context.Background(), userMsg)
	}

	// 2. Ensure StreamCLI is alive
	ensureStart := time.Now()
	cli := h.ensureCLI(session)
	log.Printf("[CacheProfile] ensureCLI DONE — %dms", time.Since(ensureStart).Milliseconds())
	if cli == nil {
		session.Send(map[string]interface{}{
			"type":     "turn_error",
			"memberID": session.memberID,
			"error":    "failed to start CLI process",
		})
		return
	}

	// 3. turn_start（生成 turn_id 供前端群組所有事件）
	turnID := fmt.Sprintf("%s-%d", session.sessionID, time.Now().UnixNano())
	session.Send(map[string]interface{}{
		"type":     "turn_start",
		"memberID": session.memberID,
		"turn_id":  turnID,
	})

	isCacheBuilding := !cli.CacheBuilt
	if isCacheBuilding {
		session.Send(map[string]interface{}{
			"type": "cli_preparing",
		})
	}

	// 4. snapshot_before: 背景載入（結果只在 streaming 結束後才需要）
	type snapBeforeResult struct {
		snap  map[string]executor.FileSnapshot
		idMap map[string]string
		err   error
	}
	snapCh := make(chan snapBeforeResult, 1)
	go func() {
		snapBeforeStart := time.Now()
		snap, idMap, err := h.loadBeforeSnapshot(session.memberID)
		if err != nil {
			log.Printf("[WS] snapshot before error: %v", err)
		} else {
			log.Printf("[CacheProfile] snapshot_before — %dms, files=%d",
				time.Since(snapBeforeStart).Milliseconds(), len(snap))
		}
		snapCh <- snapBeforeResult{snap, idMap, err}
	}()

	// 5. Send message to StreamCLI（不再被 snapshot 阻塞）
	pageCtx, _ := msg["pageContext"].(map[string]interface{})
	instruction := buildInstruction(messageText, msgObj, pageCtx)

	var cliContent interface{} = instruction
	if msgObj != nil {
		if blocks := buildContentBlocks(instruction, msgObj); blocks != nil {
			cliContent = blocks
		}
	}

	sendStart := time.Now()
	eventCh, err := cli.SendMessage(cliContent)
	log.Printf("[CacheProfile] sendMessage DONE — %dms", time.Since(sendStart).Milliseconds())
	if err != nil {
		session.Send(map[string]interface{}{
			"type":     "turn_error",
			"memberID": session.memberID,
			"error":    fmt.Sprintf("CLI send error: %v", err),
		})
		return
	}

	// 6. Push events to WebSocket
	accumulatedText := ""
	accumulatedThinking := ""
	sentToolUseIDs := map[string]bool{}
	var accumulatedToolCalls []map[string]interface{}
	var tokenUsage json.RawMessage
	currentPhase := ""

	cliReadySent := false
	firstStdoutReceived := false
	for event := range eventCh {
		if event.Type != "stdout" {
			continue
		}

		if !firstStdoutReceived {
			firstStdoutReceived = true
			log.Printf("[CacheProfile] first_stdout DONE — %dms since sendMessage, %dms since handleMessage start, cacheBuilding=%v",
				time.Since(sendStart).Milliseconds(), time.Since(handleStart).Milliseconds(), isCacheBuilding)
		}

		if isCacheBuilding && !cliReadySent {
			cliReadySent = true
			cli.CacheBuilt = true
			session.Send(map[string]interface{}{
				"type": "cli_ready",
			})
		}

		log.Printf("[WS-CLI] raw line: len=%d", len(event.Data))

		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
			log.Printf("[WS-CLI] parse error: %v for line: %s", err, event.Data[:min(len(event.Data), 100)])
			continue
		}

		eventType, _ := parsed["type"].(string)
		subtype, _ := parsed["subtype"].(string)

		switch {
		case eventType == "stream_event":
			inner, ok := parsed["event"].(map[string]interface{})
			if !ok {
				continue
			}
			innerType, _ := inner["type"].(string)

			if innerType == "message_start" {
				// 不重設 accumulatedText/Thinking，多 turn 內容持續累加
			}

			if innerType == "content_block_delta" {
				delta, _ := inner["delta"].(map[string]interface{})
				if delta == nil {
					continue
				}
				deltaType, _ := delta["type"].(string)
				switch deltaType {
			case "text_delta":
				text, _ := delta["text"].(string)
				if text != "" {
					accumulatedText += text
					session.Send(map[string]interface{}{
						"type":        "assistant_chunk",
						"memberID":    session.memberID,
						"token":       text,
						"accumulated": accumulatedText,
					})
				}
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			if text != "" {
				accumulatedThinking += text
				if currentPhase != "thinking" {
					currentPhase = "thinking"
					session.Send(map[string]interface{}{
						"type":  "phase_update",
						"phase": "thinking",
					})
				}
			}
				}
			}

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
							if text != "" && len(text) > len(accumulatedText) {
								accumulatedText = text
								session.Send(map[string]interface{}{
									"type":        "assistant_chunk",
									"memberID":    session.memberID,
									"token":       text,
									"accumulated": accumulatedText,
								})
							}
						case "thinking":
							text, _ := blockMap["thinking"].(string)
							if text != "" && text != accumulatedThinking {
								accumulatedThinking = text
								if currentPhase != "thinking" {
									currentPhase = "thinking"
									session.Send(map[string]interface{}{
										"type":  "phase_update",
										"phase": "thinking",
									})
								}
							}
						case "tool_use":
							toolID, _ := blockMap["id"].(string)
							if toolID != "" && sentToolUseIDs[toolID] {
								continue
							}
							sentToolUseIDs[toolID] = true
							inputJSON, _ := json.Marshal(blockMap["input"])
							tc := map[string]interface{}{
								"id":   toolID,
								"type": "function",
								"function": map[string]interface{}{
									"name":      blockMap["name"],
									"arguments": string(inputJSON),
								},
							}
							accumulatedToolCalls = append(accumulatedToolCalls, tc)
							toolName, _ := blockMap["name"].(string)
							toolInput, _ := blockMap["input"].(map[string]interface{})
							phase := toolNameToPhase(toolName, toolInput)
							if phase != currentPhase {
								currentPhase = phase
								session.Send(map[string]interface{}{
									"type":  "phase_update",
									"phase": phase,
								})
							}
						}
					}
				}
			}

	case eventType == "result" && subtype == "tool_result":
		toolCallID, _ := parsed["tool_use_id"].(string)
		toolContent, _ := parsed["content"].(string)
		if !noSave {
			h.chatStore.InsertChatMessage(context.Background(), &database.ChatMessage{
				SessionID:  session.sessionID,
				Mode:       session.mode,
				Role:       "tool",
				Content:    toolContent,
				ToolCallID: toolCallID,
			})
		}

	case eventType == "user":
		if msgContent, ok := parsed["message"].(map[string]interface{}); ok {
			if contentArr, ok := msgContent["content"].([]interface{}); ok {
				for _, block := range contentArr {
					blockMap, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					if blockMap["type"] == "tool_result" {
						tcID, _ := blockMap["tool_use_id"].(string)
						tcContent, _ := blockMap["content"].(string)
						if !noSave {
							h.chatStore.InsertChatMessage(context.Background(), &database.ChatMessage{
								SessionID:  session.sessionID,
								Mode:       session.mode,
								Role:       "tool",
								Content:    tcContent,
								ToolCallID: tcID,
							})
						}
					}
				}
			}
		}

		case eventType == "result" && subtype == "error_during_execution":
			errMsgs, _ := parsed["errors"].([]interface{})
			errStr := "CLI error"
			if len(errMsgs) > 0 {
				if s, ok := errMsgs[0].(string); ok {
					errStr = s
				}
			}
			log.Printf("[WS-CLI] error_during_execution: %s", errStr)
			session.Send(map[string]interface{}{
				"type":     "turn_error",
				"memberID": session.memberID,
				"error":    errStr,
			})

		case eventType == "result" && subtype == "success":
			if resultText, ok := parsed["result"].(string); ok && resultText != "" && accumulatedText == "" {
				accumulatedText = resultText
				session.Send(map[string]interface{}{
					"type":        "assistant_chunk",
					"memberID":    session.memberID,
					"token":       resultText,
					"accumulated": accumulatedText,
				})
			}

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

			h.recordBilling(session.memberID, session.mode, session.sessionID, parsed)

			if err := h.checkCredits(session.memberID); err != nil {
				log.Printf("[WS] credits exhausted mid-task for %s, killing CLI", session.memberID)
				session.mu.Lock()
				if session.cli != nil {
					session.cli.Kill()
				}
				session.mu.Unlock()
				session.Send(map[string]interface{}{
					"type":     "turn_error",
					"memberID": session.memberID,
					"error":    err.Error(),
				})
			}
		}
	}

	// （FORGE 標記偵測已移除 — 改用 MCP tool plugin_forge 觸發）

	// 7. Save assistant message
	var assistantMsgID interface{}
	if !noSave {
		assistantMsg := &database.ChatMessage{
			SessionID:  session.sessionID,
			Mode:       session.mode,
			Role:       "assistant",
			Content:    accumulatedText,
			Thinking:   accumulatedThinking,
			TokenUsage: tokenUsage,
		}
		if len(accumulatedToolCalls) > 0 {
			if b, err := json.Marshal(accumulatedToolCalls); err == nil {
				assistantMsg.ToolCalls = b
			}
		}
		h.chatStore.InsertChatMessage(context.Background(), assistantMsg)
		assistantMsgID = assistantMsg.ID
	}

	// 8. turn_end
	session.Send(map[string]interface{}{
		"type":           "turn_end",
		"memberID":       session.memberID,
		"turn_id":        turnID,
		"final_response": accumulatedText,
		"checkpoint_id":  assistantMsgID,
		"token_usage":    tokenUsage,
	})

	log.Printf("[CacheProfile] handleMessage DONE — total=%dms, member=%s",
		time.Since(handleStart).Milliseconds(), session.memberID)

	// 9. snapshot_after + title：全部在背景執行，不阻塞下一次 handleMessage
	go func() {
		snapBefore := <-snapCh
		if snapBefore.err == nil {
			if len(sentToolUseIDs) == 0 {
				log.Printf("[CacheProfile] snapshot_after SKIPPED — no tool_use, member=%s", session.memberID)
				h.snapCache.Store(session.memberID, &cachedSnapshot{snap: snapBefore.snap, idMap: snapBefore.idMap})
			} else {
				snapAfterStart := time.Now()
				afterSnap, afterIDMap, afterErr := executor.TakeIncrementalSnapshot(
					h.vaultFS, session.memberID, snapBefore.snap, snapBefore.idMap,
				)
				if afterErr != nil {
					log.Printf("[WS] incremental snapshot after error: %v", afterErr)
				} else {
					diff := executor.ComputeDiff(snapBefore.snap, afterSnap)
					hasChanges := len(diff.Created)+len(diff.Modified)+len(diff.Deleted)+len(diff.Moved) > 0
					log.Printf("[CacheProfile] snapshot_after INCREMENTAL+diff — %dms, changed=%v, +%d ~%d -%d mv%d, tools=%d",
						time.Since(snapAfterStart).Milliseconds(), hasChanges,
						len(diff.Created), len(diff.Modified), len(diff.Deleted), len(diff.Moved), len(sentToolUseIDs))

					if hasChanges {
						movedEntries := make([]mirror.MovedFileEntry, len(diff.Moved))
						for i, m := range diff.Moved {
							movedEntries[i] = mirror.MovedFileEntry{OldPath: m.OldPath, NewPath: m.NewPath}
						}
						importer := mirror.NewImporter(h.vaultFS)
						entries, importErr := importer.ProcessDiff(
							session.memberID, diff.Created, diff.Modified, diff.Deleted, movedEntries, snapBefore.idMap,
						)
						if importErr != nil {
							log.Printf("[WS] importer.ProcessDiff error: %v", importErr)
					} else if len(entries) > 0 {
						wbResult := executor.WriteBack(
							context.Background(), h.itemWriter,
							session.memberID, entries,
						)
						log.Printf("[WS] WriteBack: +%d ~%d mv%d -%d err%d",
							wbResult.Created, wbResult.Updated, wbResult.Moved,
							wbResult.Deleted, wbResult.Errors)

						// 從 ProcessDiff 結果建立 artifact 清單
						var artifacts []map[string]interface{}
						for _, entry := range entries {
							if entry.Action == mirror.ImportActionSkip {
								continue
							}
							art := map[string]interface{}{
								"action":   string(entry.Action),
								"itemType": entry.ItemType,
								"id":       entry.DocID,
							}
							if entry.ItemData != nil && entry.ItemData.Name != "" {
								art["title"] = entry.ItemData.Name
							}
							artifacts = append(artifacts, art)
						}
						if len(artifacts) > 0 {
							session.Send(map[string]interface{}{
								"type":      "artifact_upsert",
								"turn_id":   turnID,
								"artifacts": artifacts,
							})
							// 持久化 artifacts 供歷史重載
							if !noSave {
								artJSON, _ := json.Marshal(artifacts)
								h.chatStore.InsertChatMessage(context.Background(), &database.ChatMessage{
									SessionID: session.sessionID,
									Mode:      session.mode,
									Role:      "artifacts",
									Content:   string(artJSON),
								})
							}
						}
					}

					h.updateDBSnapshot(session.memberID, afterSnap, afterIDMap, diff)
					session.Send(map[string]interface{}{
						"type":     "vault_changed",
						"memberID": session.memberID,
					})
					}
					h.snapCache.Store(session.memberID, &cachedSnapshot{snap: afterSnap, idMap: afterIDMap})
				}
			}
		}

		// 10. Generate session title (fire-and-forget)
		if !noSave {
			h.maybeGenerateTitle(session, messageText, accumulatedText)
		}
	}()
}

// loadBeforeSnapshot 先查記憶體 cache，cache miss 再查 DB，DB 也沒有才做完整 EFS scan。
func (h *WsHandler) loadBeforeSnapshot(memberID string) (map[string]executor.FileSnapshot, map[string]string, error) {
	start := time.Now()
	if cached, ok := h.snapCache.Load(memberID); ok {
		cs := cached.(*cachedSnapshot)
		log.Printf("[CacheProfile] snapshot_before HIT memcache, files=%d, elapsed=%dms",
			len(cs.snap), time.Since(start).Milliseconds())
		return cs.snap, cs.idMap, nil
	}

	ctx := context.Background()
	rows, err := h.snapshotStore.GetSnapshot(ctx, memberID)
	if err == nil && len(rows) > 0 {
		snap, idMap := dbRowsToSnapshot(rows)
		h.snapCache.Store(memberID, &cachedSnapshot{snap: snap, idMap: idMap})
		log.Printf("[CacheProfile] snapshot_before from DB, cached, files=%d, elapsed=%dms",
			len(snap), time.Since(start).Milliseconds())
		return snap, idMap, nil
	}

	log.Printf("[CacheProfile] snapshot DB empty for %s, doing full EFS scan", memberID)
	snap, idMap, scanErr := executor.TakeSnapshotAndPathIDMap(h.vaultFS, memberID)
	if scanErr != nil {
		return nil, nil, scanErr
	}

	dbRows := snapshotToDBRows(snap, idMap)
	if storeErr := h.snapshotStore.ReplaceSnapshot(ctx, memberID, dbRows); storeErr != nil {
		log.Printf("[WS] DB snapshot initial store failed for %s: %v", memberID, storeErr)
	}
	h.snapCache.Store(memberID, &cachedSnapshot{snap: snap, idMap: idMap})
	log.Printf("[CacheProfile] snapshot_before fullScan+DB, files=%d, elapsed=%dms",
		len(snap), time.Since(start).Milliseconds())
	return snap, idMap, nil
}

// updateDBSnapshot applies diff changes to the DB snapshot incrementally.
func (h *WsHandler) updateDBSnapshot(memberID string, afterSnap map[string]executor.FileSnapshot, afterIDMap map[string]string, diff executor.VaultDiff) {
	start := time.Now()
	ctx := context.Background()

	var upserts []database.SnapshotRow
	for _, p := range diff.Created {
		if fs, ok := afterSnap[p]; ok {
			upserts = append(upserts, database.SnapshotRow{
				FilePath: p, Hash: fs.Hash, Mtime: fs.ModTime.UnixMilli(), DocID: afterIDMap[p],
			})
		}
	}
	for _, p := range diff.Modified {
		if fs, ok := afterSnap[p]; ok {
			upserts = append(upserts, database.SnapshotRow{
				FilePath: p, Hash: fs.Hash, Mtime: fs.ModTime.UnixMilli(), DocID: afterIDMap[p],
			})
		}
	}
	for _, mv := range diff.Moved {
		if fs, ok := afterSnap[mv.NewPath]; ok {
			upserts = append(upserts, database.SnapshotRow{
				FilePath: mv.NewPath, Hash: fs.Hash, Mtime: fs.ModTime.UnixMilli(), DocID: afterIDMap[mv.NewPath],
			})
		}
	}
	if err := h.snapshotStore.UpsertSnapshotFiles(ctx, memberID, upserts); err != nil {
		log.Printf("[WS] DB snapshot upsert error: %v", err)
	}

	var deletes []string
	deletes = append(deletes, diff.Deleted...)
	for _, mv := range diff.Moved {
		deletes = append(deletes, mv.OldPath)
	}
	if err := h.snapshotStore.DeleteSnapshotFiles(ctx, memberID, deletes); err != nil {
		log.Printf("[WS] DB snapshot delete error: %v", err)
	}
	log.Printf("[CacheProfile] updateDBSnapshot DONE — upserted=%d, deleted=%d, elapsed=%dms, member=%s",
		len(upserts), len(deletes), time.Since(start).Milliseconds(), memberID)
}

// updateUserLocale writes timezone & lang into the user's vault CLAUDE.md using markers.
// Only updates if values changed to avoid unnecessary writes on reconnect.
func (h *WsHandler) updateUserLocale(memberID, timezone, lang string) {
	claudeMDPath := filepath.Join(memberID, "CLAUDE.md")

	// Build new locale block
	var lines []string
	if timezone != "" {
		lines = append(lines, fmt.Sprintf("用戶時區：%s", timezone))
	}
	if lang != "" {
		lines = append(lines, fmt.Sprintf("回應語言：%s", lang))
	}
	if len(lines) == 0 {
		return
	}
	newBlock := "<!-- LOCALE:START -->\n## 用戶地區設定\n" + strings.Join(lines, "\n") + "\n<!-- LOCALE:END -->"

	existing, err := h.vaultFS.ReadFile(claudeMDPath)
	if err != nil {
		// No user CLAUDE.md yet — create with locale block
		content := fmt.Sprintf("# 用戶個人化設定\n\n%s\n\n<!-- AIHINTS:START -->\n<!-- AIHINTS:END -->\n\n<!-- AI_MEMORY:START -->\n<!-- AI_MEMORY:END -->\n", newBlock)
		h.vaultFS.WriteFile(claudeMDPath, []byte(content))
		executor.ChownToMember(filepath.Join(h.vaultRoot, claudeMDPath), memberID)
		return
	}

	content := string(existing)
	startMarker := "<!-- LOCALE:START -->"
	endMarker := "<!-- LOCALE:END -->"
	startIdx := strings.Index(content, startMarker)
	endIdx := strings.Index(content, endMarker)

	if startIdx >= 0 && endIdx >= 0 && endIdx > startIdx {
		// Check if unchanged
		oldBlock := content[startIdx : endIdx+len(endMarker)]
		if oldBlock == newBlock {
			return // no change
		}
		// Replace
		content = content[:startIdx] + newBlock + content[endIdx+len(endMarker):]
	} else {
		// Insert after title line or at beginning
		titleEnd := strings.Index(content, "\n\n")
		if titleEnd >= 0 {
			content = content[:titleEnd+2] + newBlock + "\n\n" + content[titleEnd+2:]
		} else {
			content = newBlock + "\n\n" + content
		}
	}

	h.vaultFS.WriteFile(claudeMDPath, []byte(content))
	executor.ChownToMember(filepath.Join(h.vaultRoot, claudeMDPath), memberID)
}

// enforceMaxCLIs kills oldest CLIs for a user if they exceed maxCount.
func (h *WsHandler) enforceMaxCLIs(memberID string, maxCount int) {
	type sessionEntry struct {
		key     string
		session *WsSession
	}
	var userSessions []sessionEntry

	h.sessions.Range(func(key, val any) bool {
		s := val.(*WsSession)
		if s.memberID == memberID {
			s.mu.Lock()
			alive := s.cli != nil && s.cli.IsAlive()
			s.mu.Unlock()
			if alive {
				userSessions = append(userSessions, sessionEntry{key: key.(string), session: s})
			}
		}
		return true
	})

	// Kill oldest sessions until within limit (leave room for the new one)
	for len(userSessions) >= maxCount {
		oldest := userSessions[0]
		userSessions = userSessions[1:]
		oldest.session.mu.Lock()
		if oldest.session.cli != nil {
			log.Printf("[WS] enforceMaxCLIs: killing CLI pid=%d for member=%s (over limit %d)",
				oldest.session.cli.Pid(), memberID, maxCount)
			oldest.session.cli.Kill()
		}
		oldest.session.mu.Unlock()
	}
}

// ensureCLI returns an alive StreamCLI, rebuilding with --resume if needed.
func (h *WsHandler) ensureCLI(session *WsSession) *executor.StreamCLI {
	session.mu.Lock()
	cli := session.cli
	session.mu.Unlock()

	if cli != nil && cli.IsAlive() {
		log.Printf("[CacheProfile] ensureCLI: reusing alive CLI pid=%d", cli.Pid())
		return cli
	}

	log.Printf("[CacheProfile] ensureCLI: CLI dead or nil, rebuilding")

	session.mu.Lock()
	if session.cli != nil {
		session.cli.Kill()
	}
	session.mu.Unlock()

	workDir := filepath.Join(h.vaultRoot, session.memberID)

	rebuildStart := time.Now()
	h.rebuildSessionJSONL(session, workDir)
	log.Printf("[CacheProfile] rebuildSessionJSONL DONE — %dms", time.Since(rebuildStart).Milliseconds())

	cliStart := time.Now()
	newCLI, err := executor.NewStreamCLI(workDir, "chat", session.memberID, session.sessionID, session.model, true, 5*time.Minute)
	if err != nil {
		log.Printf("[CacheProfile] NewStreamCLI FAILED — %dms, error=%v", time.Since(cliStart).Milliseconds(), err)
		return nil
	}
	log.Printf("[CacheProfile] NewStreamCLI DONE — %dms, pid=%d", time.Since(cliStart).Milliseconds(), newCLI.Pid())

	session.mu.Lock()
	session.cli = newCLI
	session.mu.Unlock()

	return newCLI
}

// rebuildSessionJSONL queries chat messages from DB and writes the .jsonl file
// so that CLI --resume can restore the conversation.
func (h *WsHandler) rebuildSessionJSONL(session *WsSession, workDir string) {
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

	smList := make([]executor.SessionMessage, 0, len(msgs))
	for _, m := range msgs {
		smList = append(smList, executor.SessionMessage{
			ID:            m.ID,
			Role:          m.Role,
			Content:       m.Content,
			Thinking:      m.Thinking,
			ToolCalls:     m.ToolCalls,
			ToolCallID:    m.ToolCallID,
			AttachedItems: m.AttachedItems,
			CreatedAt:     m.CreatedAt,
		})
	}

	if err := executor.RebuildSessionJSONL(session.sessionID, workDir, session.memberID, smList); err != nil {
		log.Printf("[WS] rebuild JSONL error for session %s: %v", session.sessionID, err)
	} else {
		log.Printf("[WS] rebuilt JSONL for session %s (%d messages)", session.sessionID, len(smList))
	}
}

func (h *WsHandler) handleInterrupt(session *WsSession) {
	session.mu.Lock()
	cli := session.cli
	session.mu.Unlock()

	if cli != nil {
		if !cli.Interrupt() {
			// SIGINT failed (process already dead), fall back to Kill
			log.Printf("[WS] interrupt: SIGINT failed, falling back to Kill")
			cli.Kill()
		}
	}

	session.Send(map[string]interface{}{
		"type":     "turn_interrupted",
		"memberID": session.memberID,
		"message":  "已中斷",
	})
}

// executePluginForge 啟動插件鍛造 Sub-Agent（同步阻塞到完成）
// session 可為 nil（沒有 WebSocket 時不發意圖事件）
func (h *WsHandler) executePluginForge(session *WsSession, memberID, forgeTitle, userPrompt string) map[string]interface{} {
	log.Printf("[PluginForge] starting sub-agent: title=%q member=%s", forgeTitle, memberID)

	sendWS := func(msg map[string]interface{}) {
		if session != nil {
			if err := session.Send(msg); err != nil {
				log.Printf("[PluginForge] WS send error: %v (type=%v)", err, msg["type"])
			} else {
				log.Printf("[PluginForge] WS sent: type=%v", msg["type"])
			}
		}
	}

	sendWS(map[string]interface{}{"type": "sub_agent_start", "title": forgeTitle})

	vaultRoot := os.Getenv("VAULT_ROOT")
	if vaultRoot == "" {
		vaultRoot = "/vaults"
	}
	sharedDir := filepath.Join(vaultRoot, "shared")

	pluginTemplate, err := os.ReadFile("/app/config/CLAUDE-plugin.md.template")
	if err != nil {
		log.Printf("[PluginForge] failed to read plugin template: %v", err)
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": "插件鍛造系統啟動失敗"})
		return map[string]interface{}{"status": "error", "error": "failed to read plugin template"}
	}

	instructions := strings.ReplaceAll(string(pluginTemplate), "{VAULT_SHARED}", sharedDir)

	// 預先推導 pluginDir 名稱給 Sub-Agent
	suggestedDir := strings.ToLower(strings.ReplaceAll(forgeTitle, " ", "-"))
	cleanDir := ""
	for _, c := range suggestedDir {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			cleanDir += string(c)
		}
	}
	if cleanDir == "" {
		cleanDir = "plugin"
	}

	fullPrompt := fmt.Sprintf("%s\n\n---\n\n插件目錄名稱：plugins/%s/\n用戶需求：%s", instructions, cleanDir, userPrompt)
	workDir := filepath.Join(vaultRoot, memberID)

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "intent": "啟動插件鍛造環境..."})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--mcp-config", "/home/mirror/.claude/settings.json",
		"-p", fullPrompt,
	}

	cmd := executor.NewClaudeCmdExported(ctx, workDir, "plugin", memberID, args)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": err.Error()})
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": err.Error()})
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}

	if err := cmd.Start(); err != nil {
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": err.Error()})
		return map[string]interface{}{"status": "error", "error": err.Error()}
	}

	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			log.Printf("[PluginForge-stderr] %s", scanner.Text())
		}
	}()

	// 讀取 Sub-Agent 事件，過濾後發送意圖流
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	lastIntent := ""
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(line), &parsed) != nil {
			continue
		}
		if parsed["type"] == "assistant" {
			if mc, ok := parsed["message"].(map[string]interface{}); ok {
				if ca, ok := mc["content"].([]interface{}); ok {
					for _, block := range ca {
						bm, ok := block.(map[string]interface{})
						if !ok || bm["type"] != "tool_use" {
							continue
						}
						toolName, _ := bm["name"].(string)
						input, _ := bm["input"].(map[string]interface{})
						var intent string
						switch toolName {
						case "Read":
							fp, _ := input["file_path"].(string)
							intent = "讀取 " + filepath.Base(fp)
						case "Write":
							fp, _ := input["file_path"].(string)
							intent = "建立 " + filepath.Base(fp)
						case "Edit":
							fp, _ := input["file_path"].(string)
							intent = "修改 " + filepath.Base(fp)
						case "Bash":
							if d, _ := input["description"].(string); d != "" {
								intent = d
							} else if c, _ := input["command"].(string); c != "" {
								if len(c) > 50 {
									c = c[:50] + "..."
								}
								intent = "執行: " + c
							}
						case "Glob":
							intent = "搜尋檔案..."
						case "Grep":
							intent = "搜尋程式碼..."
						default:
							intent = toolName
						}
						if intent != "" && intent != lastIntent {
							lastIntent = intent
							sendWS(map[string]interface{}{"type": "sub_agent_intent", "intent": intent})
						}
					}
				}
			}
		}
	}

	if waitErr := cmd.Wait(); waitErr != nil {
		log.Printf("[PluginForge] sub-agent exited with error: %v", waitErr)
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": waitErr.Error()})
		return map[string]interface{}{"status": "error", "error": waitErr.Error()}
	}

	// 使用先前推導的 pluginDir
	pluginDir := cleanDir

	// 找到實際的 plugin 目錄
	mainTsxPath := filepath.Join(memberID, "plugins", pluginDir, "main.tsx")
	if !h.vaultFS.Exists(mainTsxPath) {
		pluginsPath := filepath.Join(memberID, "plugins")
		entries, _ := h.vaultFS.ListDir(pluginsPath)
		for _, e := range entries {
			if e.IsDir() {
				if h.vaultFS.Exists(filepath.Join(memberID, "plugins", e.Name(), "main.tsx")) {
					pluginDir = e.Name()
					break
				}
			}
		}
	}

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "intent": "編譯插件..."})

	// esbuild 打包（通用 external：所有 bare import 都 external，前端 require shim 解析）
	entryPath := filepath.Join(workDir, "plugins", pluginDir, "main.tsx")
	bundlePath := filepath.Join(workDir, "plugins", pluginDir, "bundle.js")
	esbuildCmd := exec.CommandContext(ctx, "node", "/app/config/esbuild-plugin-bundle.mjs",
		entryPath, bundlePath)
	esbuildOut, esbuildErr := esbuildCmd.CombinedOutput()
	if esbuildErr != nil {
		errMsg := fmt.Sprintf("esbuild 編譯失敗: %s", string(esbuildOut))
		log.Printf("[PluginForge] %s", errMsg)
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": errMsg})
		return map[string]interface{}{"status": "error", "error": errMsg}
	}
	log.Printf("[PluginForge] esbuild success: %s", bundlePath)

	// 計算 bundle hash
	bundleFullPath := filepath.Join(memberID, "plugins", pluginDir, "bundle.js")
	bundleBytes, _ := h.vaultFS.ReadFile(bundleFullPath)
	bundleHash := ""
	if len(bundleBytes) > 0 {
		hashSum := sha256.Sum256(bundleBytes)
		bundleHash = fmt.Sprintf("%x", hashSum[:8])
	}

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "intent": "註冊插件..."})

	// 寫入 PLUGIN item（vault JSON + PG）
	pluginID := fmt.Sprintf("%x%012x", time.Now().UnixNano()&0xFFFFFFFF, time.Now().UnixNano()>>32)
	if len(pluginID) > 24 {
		pluginID = pluginID[:24]
	}
	pluginDoc := executor.Doc{
		"_id": pluginID, "itemType": "PLUGIN", "name": forgeTitle,
		"fields": map[string]interface{}{
			"pluginDir": pluginDir, "bundleHash": bundleHash, "version": 1,
			"status": "active", "description": forgeTitle,
			"createdAt": time.Now().UnixMilli(), "updatedAt": time.Now().UnixMilli(),
		},
	}
	if upsertErr := h.itemWriter.UpsertItem(context.Background(), memberID, pluginDoc); upsertErr != nil {
		log.Printf("[PluginForge] failed to write PLUGIN item: %v", upsertErr)
	} else {
		log.Printf("[PluginForge] PLUGIN item written: id=%s dir=%s hash=%s", pluginID, pluginDir, bundleHash)
	}

	// 同時寫 PLUGIN JSON 到 vault 讓 AI 可管理
	pluginJSON, _ := json.MarshalIndent(map[string]interface{}{
		"_id": pluginID, "itemType": "PLUGIN", "name": forgeTitle,
		"pluginDir": pluginDir, "bundleHash": bundleHash,
		"version": 1, "status": "active", "description": forgeTitle,
	}, "", "  ")
	pluginJSONPath := filepath.Join(memberID, "PLUGIN", pluginDir+".json")
	if writeErr := h.vaultFS.WriteFile(pluginJSONPath, pluginJSON); writeErr != nil {
		log.Printf("[PluginForge] failed to write PLUGIN json to vault: %v", writeErr)
	}

	sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "success", "title": forgeTitle,
		"plugin": map[string]interface{}{"id": pluginID, "pluginDir": pluginDir, "bundleHash": bundleHash}})
	sendWS(map[string]interface{}{"type": "vault_changed"})

	return map[string]interface{}{
		"status": "success", "title": forgeTitle,
		"pluginDir": pluginDir, "bundleHash": bundleHash,
	}
}

func (h *WsHandler) maybeGenerateTitle(session *WsSession, userMsg, assistantMsg string) {
	msgs, _, err := h.chatStore.GetMessagesAfter(context.Background(), session.sessionID, session.mode, "", 5)
	if err != nil || len(msgs) > 2 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("[WS] generating title for session %s via ai-service...", session.sessionID)

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

func buildInstruction(messageText string, msgObj map[string]interface{}, pageCtx map[string]interface{}) string {
	var parts []string
	parts = append(parts, messageText)

	// 收集附加項目（非圖片）
	var attachedParts []string
	if msgObj != nil {
		if items, ok := msgObj["attachedItems"].([]interface{}); ok {
			for _, raw := range items {
				item, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				itemType, _ := item["type"].(string)
				if itemType == "image" {
					continue // 圖片由 buildContentBlocks 處理
				}
				itemJSON, err := json.Marshal(item)
				if err == nil {
					attachedParts = append(attachedParts, fmt.Sprintf("\n[附加項目]\n%s", string(itemJSON)))
				}
			}
		}
	}

	// 有附加項目時不加 pageContext（附加項目本身就是上下文，避免混淆）
	if len(attachedParts) > 0 {
		parts = append(parts, attachedParts...)
	} else if pageCtx != nil {
		// 無附加項目時才加 pageContext（資料夾 + 選取項目）
		if folder, ok := pageCtx["folder"].(map[string]interface{}); ok {
			pageID, _ := folder["pageId"].(string)
			pageName, _ := folder["pageName"].(string)
			pageType, _ := folder["pageType"].(string)
			parts = append(parts, fmt.Sprintf("\n[目前所在資料夾] ID: %s, 名稱: %s, 類型: %s", pageID, pageName, pageType))
		}
		if item, ok := pageCtx["item"].(map[string]interface{}); ok {
			pageID, _ := item["pageId"].(string)
			pageName, _ := item["pageName"].(string)
			parts = append(parts, fmt.Sprintf("\n[目前選取項目] ID: %s, 名稱: %s", pageID, pageName))
		}
	}

	return strings.Join(parts, "")
}

// buildContentBlocks 檢查 attachedItems 中是否有圖片，
// 有的話組成多模態 content blocks 陣列（image URL + text）。
// 無圖片時回傳 nil，表示用純文字即可。
func buildContentBlocks(instruction string, msgObj map[string]interface{}) []map[string]interface{} {
	items, ok := msgObj["attachedItems"].([]interface{})
	if !ok {
		return nil
	}

	var imageBlocks []map[string]interface{}
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		itemType, _ := item["type"].(string)
		if itemType != "image" {
			continue
		}
		url, _ := item["url"].(string)
		if url == "" {
			continue
		}
		imageBlocks = append(imageBlocks, map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type": "url",
				"url":  url,
			},
		})
	}

	if len(imageBlocks) == 0 {
		return nil // 無圖片，走純文字路徑
	}

	// 有圖片：組成 content blocks 陣列（先圖片後文字）
	var blocks []map[string]interface{}
	blocks = append(blocks, imageBlocks...)
	blocks = append(blocks, map[string]interface{}{
		"type": "text",
		"text": instruction,
	})
	return blocks
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

// --- Snapshot DB <-> memory conversion helpers ---

func snapshotToDBRows(snap map[string]executor.FileSnapshot, idMap map[string]string) []database.SnapshotRow {
	rows := make([]database.SnapshotRow, 0, len(snap))
	for path, fs := range snap {
		rows = append(rows, database.SnapshotRow{
			FilePath: path,
			Hash:     fs.Hash,
			Mtime:    fs.ModTime.UnixMilli(),
			DocID:    idMap[path],
		})
	}
	return rows
}

func dbRowsToSnapshot(rows []database.SnapshotRow) (map[string]executor.FileSnapshot, map[string]string) {
	snap := make(map[string]executor.FileSnapshot, len(rows))
	idMap := make(map[string]string, len(rows))
	for _, r := range rows {
		snap[r.FilePath] = executor.FileSnapshot{
			Path:    r.FilePath,
			Hash:    r.Hash,
			ModTime: time.UnixMilli(r.Mtime),
		}
		if r.DocID != "" {
			idMap[r.FilePath] = r.DocID
		}
	}
	return snap, idMap
}

// --- Credits & Billing ---

func (h *WsHandler) checkCredits(memberID string) error {
	mcURL := os.Getenv("MEMBERCENTER_URL")
	if mcURL == "" {
		mcURL = "http://membercenter.svc.local:3006"
	}

	// 查詢 credits 餘額（與 AI prompt server 一致）
	balanceURL := fmt.Sprintf("%s/api/internal/credits/balance/%s", mcURL, memberID)
	resp, err := http.Get(balanceURL)
	if err != nil {
		log.Printf("[WS] credits balance check failed: %v", err)
		return fmt.Errorf("CREDITS_CHECK_FAILED: 無法驗證額度")
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			TotalCredits float64 `json:"totalCredits"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[WS] credits balance decode failed: %v", err)
		return fmt.Errorf("CREDITS_CHECK_FAILED: 無法解析額度資訊")
	}

	if !result.Success {
		log.Printf("[WS] credits balance query not successful for %s", memberID)
		return fmt.Errorf("CREDITS_CHECK_FAILED: 額度查詢失敗")
	}

	if result.Data.TotalCredits <= 0 {
		log.Printf("[WS] insufficient credits for %s: %.2f", memberID, result.Data.TotalCredits)
		return fmt.Errorf("INSUFFICIENT_CREDITS: Credits 不足，當前餘額: %.2f", result.Data.TotalCredits)
	}

	return nil
}

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

	modelName := "claude-sonnet-4-6"
	if mu, ok := resultEvent["modelUsage"].(map[string]interface{}); ok {
		for name := range mu {
			modelName = name
			break
		}
	}

	body := map[string]interface{}{
		"member_id":             memberID,
		"model":                 modelName,
		"input_tokens":          int(inputTokens),
		"output_tokens":         int(outputTokens),
		"cache_creation_tokens": int(cacheCreation),
		"cache_read_tokens":     int(cacheRead),
		"category":              mode,
		"action":                "response",
		"session_id":            sessionID,
		"mode":                  mode,
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

// jwtClaims 對應 membercenter 簽發的 JWT 結構
type jwtClaims struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	TokenType string `json:"token_type"`
	jwt.RegisteredClaims
}

// verifyJWT 驗證 JWT 簽名並回傳 claims；驗簽失敗回傳 error。
func verifyJWT(tokenStr string) (*jwtClaims, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("JWT_SECRET not configured")
	}

	claims := &jwtClaims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("JWT parse error: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("invalid JWT token")
	}
	if claims.TokenType != "access" {
		return nil, fmt.Errorf("invalid token type: %s", claims.TokenType)
	}
	if claims.UserID == "" {
		return nil, fmt.Errorf("missing user_id in JWT")
	}
	return claims, nil
}
