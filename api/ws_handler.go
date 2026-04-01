package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

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

type sessionOperationKind string

const (
	sessionOperationKindChat        sessionOperationKind = "chat"
	sessionOperationKindPluginForge sessionOperationKind = "plugin_forge"
)

type sessionOperation struct {
	id               string
	kind             sessionOperationKind
	toolCallID       string
	ctx              context.Context
	cancel           context.CancelFunc
	interruptFn      func()
	cancelOnce       sync.Once
	skipOnDisconnect bool // true = WS 斷線時不取消（forge 背景繼續）
}

func newSessionOperation(kind sessionOperationKind, id, toolCallID string, interruptFn func()) *sessionOperation {
	if id == "" {
		id = fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &sessionOperation{
		id:               id,
		kind:             kind,
		toolCallID:       toolCallID,
		ctx:              ctx,
		cancel:           cancel,
		interruptFn:      interruptFn,
		skipOnDisconnect: false,
	}
}

func (op *sessionOperation) Interrupt() {
	op.cancelOnce.Do(func() {
		op.cancel()
		if op.interruptFn != nil {
			op.interruptFn()
		}
	})
}

// WsSession represents a single WebSocket connection session.
type WsSession struct {
	conn             *websocket.Conn
	memberID         string
	sessionID        string
	mode             string
	model            string
	lang             string
	taskID           string
	status           string // idle, asking, interrupted
	mu               sync.Mutex
	cli              *executor.StreamCLI
	done             chan struct{} // 關閉時通知心跳 goroutine 停止
	forgeToolCallIDs chan string   // plugin forge tool_call_id queue
	operations       map[string]*sessionOperation
}

// Send writes a JSON message to the WebSocket connection (thread-safe).
func (s *WsSession) Send(msg map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn.WriteJSON(msg)
}

func (s *WsSession) registerOperation(op *sessionOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operations == nil {
		s.operations = make(map[string]*sessionOperation)
	}
	s.operations[op.id] = op
}

func (s *WsSession) unregisterOperation(opID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.operations, opID)
}

func (s *WsSession) activeOperations() []*sessionOperation {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := make([]*sessionOperation, 0, len(s.operations))
	for _, op := range s.operations {
		ops = append(ops, op)
	}
	return ops
}

func (s *WsSession) interruptCLI() bool {
	s.mu.Lock()
	cli := s.cli
	s.mu.Unlock()

	if cli == nil {
		return false
	}
	if cli.Interrupt() {
		return true
	}

	// SIGINT failed (process already dead), fall back to Kill
	log.Printf("[WS] interrupt: SIGINT failed, falling back to Kill")
	cli.Kill()
	return true
}

// interruptActiveOperations 中斷活躍作業（WS 斷線時呼叫，跳過 skipOnDisconnect 的作業）
func (s *WsSession) interruptActiveOperations() (hasChat bool, count int) {
	ops := s.activeOperations()
	for _, op := range ops {
		if op.skipOnDisconnect {
			continue
		}
		if op.kind == sessionOperationKindChat {
			hasChat = true
		}
		op.Interrupt()
		count++
	}
	return hasChat, count
}

// interruptAllOperations 強制中斷所有作業（使用者明確中斷時呼叫）
func (s *WsSession) interruptAllOperations() (hasChat bool, count int) {
	ops := s.activeOperations()
	for _, op := range ops {
		if op.kind == sessionOperationKindChat {
			hasChat = true
		}
		op.Interrupt()
		count++
	}
	return hasChat, count
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
	chatStore          ChatStore
	snapshotStore      SnapshotStore
	itemWriter         executor.DataWriter
	vaultRoot          string
	vaultFS            mirror.VaultFS
	sessions           sync.Map // sessionKey -> *WsSession
	cliPool            sync.Map // memberID -> *warmCLI (warmup pool)
	snapCache          sync.Map // memberID -> *cachedSnapshot
	pendingForgeResult sync.Map // memberID -> map[string]interface{} (forge 背景完成後的待交付結果)
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

	log.Printf("[ForgeAPI] received forge request: title=%q member=%s wsSession=%s", req.Title, req.MemberID, req.WsSessionID)

	// 精確查找 WebSocket session（優先用 wsSessionID，fallback 用 memberID）
	var session *WsSession
	if req.WsSessionID != "" {
		sessionKey := req.MemberID + ":" + req.WsSessionID
		if val, ok := h.sessions.Load(sessionKey); ok {
			session = val.(*WsSession)
			log.Printf("[ForgeAPI] found session by exact key: %s", sessionKey)
		}
	}
	if session == nil {
		h.sessions.Range(func(key, value interface{}) bool {
			if s, ok := value.(*WsSession); ok && s.memberID == req.MemberID {
				session = s
				return false
			}
			return true
		})
		if session != nil {
			log.Printf("[ForgeAPI] found session by memberID fallback: %s", req.MemberID)
		} else {
			log.Printf("[ForgeAPI] no active WebSocket session for member %s", req.MemberID)
		}
	}

	// 從 session 取出對應的 tool_call_id
	var toolCallID string
	if session != nil {
		select {
		case toolCallID = <-session.forgeToolCallIDs:
			log.Printf("[ForgeAPI] got tool_call_id: %s", toolCallID)
		default:
			log.Printf("[ForgeAPI] no tool_call_id available in queue")
		}
	}

	// 執行插件鍛造（同步，阻塞到完成）
	result := h.executePluginForge(session, req.MemberID, req.Title, req.Prompt, toolCallID)

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
		conn:             conn,
		memberID:         memberID,
		sessionID:        sessionID,
		mode:             mode,
		model:            model,
		lang:             lang,
		status:           "idle",
		done:             make(chan struct{}),
		forgeToolCallIDs: make(chan string, 10),
		operations:       make(map[string]*sessionOperation),
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
		session.interruptActiveOperations()
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

	// 交付背景 forge 完成的待交付結果
	if pending, ok := h.pendingForgeResult.LoadAndDelete(memberID); ok {
		if msg, ok := pending.(map[string]interface{}); ok {
			log.Printf("[PluginForge] delivering pending forge result to reconnected member %s", memberID)
			session.Send(msg)
			session.Send(map[string]interface{}{"type": "vault_changed"})
		}
	}

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
				// #region agent log
				debugMirrorLog("api/ws_handler.go:368", "MirrorThinking-DEBUG rebuild_session_jsonl", debugRunInitial, "H2", map[string]interface{}{
					"sessionID":    sessionID,
					"source":       "ws_resume_bootstrap",
					"messageCount": len(smList),
					"tailMessages": debugSessionMessageSummary(smList),
				})
				// #endregion
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

func (h *WsHandler) handleMessage(session *WsSession, sessionKey string, msg map[string]interface{}) {
	handleStart := time.Now()
	log.Printf("[CacheProfile] handleMessage START — member=%s session=%s", session.memberID, session.sessionID)

	// 立刻生成 title（不等 AI 回覆）
	go h.generateTitleFromUserMessage(session, msg)

	if err := h.checkCredits(session.memberID); err != nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
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

	chatOp := newSessionOperation(sessionOperationKindChat, "", "", func() {
		session.interruptCLI()
	})
	session.registerOperation(chatOp)
	defer session.unregisterOperation(chatOp.id)

	isChatCancelled := func() bool {
		return chatOp.ctx.Err() != nil
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
		if err := h.chatStore.InsertChatMessage(context.Background(), userMsg); err != nil {
			log.Printf("[SessionMapping] InsertChatMessage(user) FAILED — sessionID=%s, err=%v", session.sessionID, err)
		} else {
			log.Printf("[SessionMapping] InsertChatMessage(user) OK — sessionID=%s, msgID=%s", session.sessionID, userMsg.ID)
		}
	}

	if isChatCancelled() {
		return
	}

	// 2. Ensure StreamCLI is alive
	ensureStart := time.Now()
	cli := h.ensureCLI(session)
	log.Printf("[CacheProfile] ensureCLI DONE — %dms", time.Since(ensureStart).Milliseconds())
	if cli == nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    "failed to start CLI process",
		})
		return
	}

	if isChatCancelled() {
		return
	}

	// 3. stream_start
	session.Send(map[string]interface{}{
		"type":     "stream_start",
		"memberID": session.memberID,
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

	// 6. Send and process events (with resume-failure retry)
	var (
		accumulatedText      string
		accumulatedThinking  string
		prevTurnThinking     string
		sentToolUseIDs       map[string]bool
		pushedForgeIDs       map[string]bool
		accumulatedToolCalls []map[string]interface{}
		tokenUsage           json.RawMessage
		resumeRetried        bool
		resumeFailedNoConv   bool
		sendStart            time.Time
		eventCh              <-chan executor.StreamEvent
		sendErr              error
		cliReadySent         bool
		firstStdoutReceived  bool
	)

sendAndProcess:
	accumulatedText = ""
	accumulatedThinking = ""
	prevTurnThinking = ""
	sentToolUseIDs = map[string]bool{}
	pushedForgeIDs = map[string]bool{}
	accumulatedToolCalls = nil
	tokenUsage = nil
	resumeFailedNoConv = false
	cliReadySent = false
	firstStdoutReceived = false

	sendStart = time.Now()
	if isChatCancelled() {
		return
	}
	eventCh, sendErr = cli.SendMessage(cliContent)
	log.Printf("[CacheProfile] sendMessage DONE — %dms", time.Since(sendStart).Milliseconds())
	if sendErr != nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    fmt.Sprintf("CLI send error: %v", sendErr),
		})
		return
	}

eventLoop:
	for event := range eventCh {
		if isChatCancelled() {
			break eventLoop
		}
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

		log.Printf("[WS-CLI] raw line: %s", event.Data[:min(len(event.Data), 2000)])

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
				prevTurnThinking = accumulatedThinking
				accumulatedText = ""
				accumulatedThinking = ""
				// #region agent log
				debugMirrorLog("api/ws_handler.go:631", "MirrorThinking-DEBUG message_start", debugRunInitial, "H5", map[string]interface{}{
					"sessionID":           session.sessionID,
					"prevTurnThinkingLen": len(prevTurnThinking),
					"sentToolUseCount":    len(sentToolUseIDs),
				})
				// #endregion
			}

			if innerType == "content_block_start" {
				if cb, ok := inner["content_block"].(map[string]interface{}); ok {
					if cb["type"] == "tool_use" {
						toolID, _ := cb["id"].(string)
						toolName, _ := cb["name"].(string)
						if toolName == "mcp__plugin-forge__plugin_forge" && toolID != "" {
							pushedForgeIDs[toolID] = true
							select {
							case session.forgeToolCallIDs <- toolID:
								log.Printf("[WS] forge tool_call_id pushed early (content_block_start): %s", toolID)
							default:
								log.Printf("[WS] forgeToolCallIDs channel full, dropping: %s", toolID)
							}
						}
					}
				}
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
							"type":        "stream_token",
							"memberID":    session.memberID,
							"token":       text,
							"accumulated": accumulatedText,
						})
					}
				case "thinking_delta":
					text, _ := delta["thinking"].(string)
					if text != "" {
						accumulatedThinking += text
						// 跨 turn 去重：如果新 turn 的 thinking 仍是前一個 turn 的前綴，跳過發送
						if prevTurnThinking == "" || !strings.HasPrefix(prevTurnThinking, accumulatedThinking) {
							session.Send(map[string]interface{}{
								"type":        "thinking_token",
								"memberID":    session.memberID,
								"token":       text,
								"accumulated": accumulatedThinking,
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
							if text != "" && text != accumulatedText {
								accumulatedText = text
								session.Send(map[string]interface{}{
									"type":        "stream_token",
									"memberID":    session.memberID,
									"token":       text,
									"accumulated": accumulatedText,
								})
							}
						case "thinking":
							text, _ := blockMap["thinking"].(string)
							if text != "" && text != accumulatedThinking && text != prevTurnThinking {
								accumulatedThinking = text
								// #region agent log
								debugMirrorLog("api/ws_handler.go:696", "MirrorThinking-DEBUG assistant_thinking_block", debugRunInitial, "H5", map[string]interface{}{
									"sessionID":            session.sessionID,
									"thinkingLen":          len(text),
									"matchesPrevTurn":      text == prevTurnThinking,
									"currentToolCallCount": len(accumulatedToolCalls),
									"currentTextLen":       len(accumulatedText),
								})
								// #endregion
								session.Send(map[string]interface{}{
									"type":        "thinking_token",
									"memberID":    session.memberID,
									"token":       text,
									"accumulated": accumulatedThinking,
								})
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
							// content_block_start 已提前 push 的不重複推（fallback：stream 沒有 content_block_start 時由此補推）
							toolName, _ := blockMap["name"].(string)
							if toolName == "mcp__plugin-forge__plugin_forge" && toolID != "" && !pushedForgeIDs[toolID] {
								pushedForgeIDs[toolID] = true
								select {
								case session.forgeToolCallIDs <- toolID:
									log.Printf("[WS] forge tool_call_id pushed (assistant fallback): %s", toolID)
								default:
									log.Printf("[WS] forgeToolCallIDs channel full, dropping: %s", toolID)
								}
							}
							// #region agent log
							debugMirrorLog("api/ws_handler.go:729", "MirrorThinking-DEBUG assistant_tool_use", debugRunInitial, "H1", map[string]interface{}{
								"sessionID":           session.sessionID,
								"toolID":              toolID,
								"toolName":            toolName,
								"accumulatedTextLen":  len(accumulatedText),
								"accumulatedThinkLen": len(accumulatedThinking),
								"toolCallCount":       len(accumulatedToolCalls),
							})
							// #endregion
							session.Send(map[string]interface{}{
								"type":       "message",
								"role":       "assistant",
								"content":    "",
								"tool_calls": []map[string]interface{}{tc},
							})
						}
					}
				}
			}

		case eventType == "result" && subtype == "tool_result":
			toolCallID, _ := parsed["tool_use_id"].(string)
			toolContent, _ := parsed["content"].(string)
			// #region agent log
			debugMirrorLog("api/ws_handler.go:743", "MirrorThinking-DEBUG tool_result_event", debugRunInitial, "H4", map[string]interface{}{
				"sessionID":  session.sessionID,
				"source":     "result.tool_result",
				"toolCallID": toolCallID,
				"contentLen": len(toolContent),
			})
			// #endregion
			session.Send(map[string]interface{}{
				"type":         "message",
				"role":         "tool",
				"content":      toolContent,
				"tool_call_id": toolCallID,
			})
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
							// #region agent log
							debugMirrorLog("api/ws_handler.go:770", "MirrorThinking-DEBUG tool_result_event", debugRunInitial, "H4", map[string]interface{}{
								"sessionID":  session.sessionID,
								"source":     "user.tool_result",
								"toolCallID": tcID,
								"contentLen": len(tcContent),
							})
							// #endregion
							session.Send(map[string]interface{}{
								"type":         "message",
								"role":         "tool",
								"content":      tcContent,
								"tool_call_id": tcID,
							})
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
			if !resumeRetried && strings.Contains(errStr, "No conversation found") {
				resumeFailedNoConv = true
				log.Printf("[WS-CLI] resume failed (No conversation found), will fallback to fresh session")
			} else {
				session.Send(map[string]interface{}{
					"type":     "stream_error",
					"memberID": session.memberID,
					"error":    errStr,
				})
			}

		case eventType == "result" && subtype == "success":
			if resultText, ok := parsed["result"].(string); ok && resultText != "" && accumulatedText == "" {
				accumulatedText = resultText
				session.Send(map[string]interface{}{
					"type":        "stream_token",
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
					"type":     "stream_error",
					"memberID": session.memberID,
					"error":    err.Error(),
				})
			}
		}
	}

	if isChatCancelled() {
		return
	}

	// Resume fallback: CLI 回報 "No conversation found" 時，用新 session + 歷史注入重試
	if resumeFailedNoConv && !resumeRetried {
		resumeRetried = true
		log.Printf("[WS-CLI] resume fallback: killing broken CLI, starting fresh session with history injection")

		session.mu.Lock()
		if session.cli != nil {
			session.cli.Kill()
			session.cli = nil
		}
		session.mu.Unlock()

		workDir := filepath.Join(h.vaultRoot, session.memberID)
		if session.sessionID != "" {
			executor.CleanupSessionJSONL(session.sessionID, workDir)
		}

		newCLI, newErr := executor.NewStreamCLI(workDir, "chat", session.memberID, session.sessionID, session.model, false, 5*time.Minute)
		if newErr != nil {
			log.Printf("[WS-CLI] fallback CLI start failed: %v", newErr)
			session.Send(map[string]interface{}{
				"type":     "stream_error",
				"memberID": session.memberID,
				"error":    fmt.Sprintf("fallback CLI error: %v", newErr),
			})
		} else {
			session.mu.Lock()
			session.cli = newCLI
			session.mu.Unlock()
			cli = newCLI

			historyCtx := h.buildResumeHistoryContext(session.sessionID, session.mode)
			if historyCtx != "" {
				newInstruction := historyCtx + "\n\n---\n\n" + instruction
				cliContent = newInstruction
				if msgObj != nil {
					if blocks := buildContentBlocks(newInstruction, msgObj); blocks != nil {
						cliContent = blocks
					}
				}
			}

			isCacheBuilding = true
			session.Send(map[string]interface{}{"type": "cli_preparing"})
			goto sendAndProcess
		}
	}

	if isChatCancelled() {
		return
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
		// #region agent log
		debugMirrorLog("api/ws_handler.go:922", "MirrorThinking-DEBUG persist_assistant", debugRunInitial, "H1", map[string]interface{}{
			"sessionID":       session.sessionID,
			"contentLen":      len(assistantMsg.Content),
			"thinkingLen":     len(assistantMsg.Thinking),
			"toolCallCount":   len(accumulatedToolCalls),
			"hasEmptyContent": assistantMsg.Content == "",
		})
		// #endregion
		if err := h.chatStore.InsertChatMessage(context.Background(), assistantMsg); err != nil {
			log.Printf("[SessionMapping] InsertChatMessage(assistant) FAILED — sessionID=%s, err=%v", session.sessionID, err)
		} else {
			log.Printf("[SessionMapping] InsertChatMessage(assistant) OK — sessionID=%s, msgID=%s, contentLen=%d",
				session.sessionID, assistantMsg.ID, len(accumulatedText))
		}
		assistantMsgID = assistantMsg.ID
	}

	// 8. stream_end
	session.Send(map[string]interface{}{
		"type":           "stream_end",
		"memberID":       session.memberID,
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

		// 10. title 已在 handleMessage 開頭生成，這裡不再呼叫
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
// task mode 不受此限制，避免排程任務被一般 chat 擠掉。
func (h *WsHandler) enforceMaxCLIs(memberID string, maxCount int) {
	type sessionEntry struct {
		key     string
		session *WsSession
	}
	var userSessions []sessionEntry

	// task mode 不受此限制，避免排程任務被一般 chat 擠掉
	h.sessions.Range(func(key, val any) bool {
		s := val.(*WsSession)
		if s.memberID == memberID && s.mode != "task" {
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

	// #region agent log
	debugMirrorLog("api/ws_handler.go:1240", "MirrorThinking-DEBUG rebuild_session_jsonl", debugRunInitial, "H2", map[string]interface{}{
		"sessionID":    session.sessionID,
		"source":       "ensure_cli_rebuild",
		"messageCount": len(smList),
		"tailMessages": debugSessionMessageSummary(smList),
	})
	// #endregion
	if err := executor.RebuildSessionJSONL(session.sessionID, workDir, session.memberID, smList); err != nil {
		log.Printf("[WS] rebuild JSONL error for session %s: %v", session.sessionID, err)
	} else {
		log.Printf("[WS] rebuilt JSONL for session %s (%d messages)", session.sessionID, len(smList))
	}
}

// buildResumeHistoryContext 從 DB 取得歷史訊息，找到最新 3 則 user 發話，
// 從最舊的那則開始截取所有訊息，組成上下文字串注入給新 session。
func (h *WsHandler) buildResumeHistoryContext(sessionID, mode string) string {
	msgs, _, err := h.chatStore.GetMessagesAfter(
		context.Background(), sessionID, mode, "", 200)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	// 排除最後一筆（剛才才 insert 的 user message）
	if len(msgs) > 1 && msgs[len(msgs)-1].Role == "user" {
		msgs = msgs[:len(msgs)-1]
	}
	if len(msgs) == 0 {
		return ""
	}

	// 找所有 user 訊息的 index
	var userIdxs []int
	for i, m := range msgs {
		if m.Role == "user" {
			userIdxs = append(userIdxs, i)
		}
	}
	if len(userIdxs) == 0 {
		return ""
	}

	// 從最新 3 則 user 發話的最舊那則開始截取
	startIdx := userIdxs[0]
	if len(userIdxs) > 3 {
		startIdx = userIdxs[len(userIdxs)-3]
	}

	var parts []string
	parts = append(parts, "[以下是先前的對話歷史，請基於這些上下文繼續對話]")
	for _, m := range msgs[startIdx:] {
		switch m.Role {
		case "user":
			parts = append(parts, fmt.Sprintf("\nUser: %s", m.Content))
		case "assistant":
			content := m.Content
			if len(content) > 800 {
				content = content[:800] + "…（已截斷）"
			}
			parts = append(parts, fmt.Sprintf("\nAssistant: %s", content))
		}
	}
	parts = append(parts, "\n[對話歷史結束]")

	return strings.Join(parts, "\n")
}

func (h *WsHandler) handleInterrupt(session *WsSession) {
	hasChat, opCount := session.interruptAllOperations()
	if opCount == 0 && session.interruptCLI() {
		hasChat = true
	}

	if hasChat {
		session.Send(map[string]interface{}{
			"type":     "stream_interrupted",
			"memberID": session.memberID,
			"message":  "已中斷",
		})
	}
}

// findPluginEntry 在插件目錄下找入口檔（*Plugin.tsx），找不到回退到 main.tsx
func findPluginEntry(vaultFS mirror.VaultFS, pluginRoot string) string {
	entries, err := vaultFS.ListDir(pluginRoot)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, "Plugin.tsx") {
			return name
		}
	}
	return ""
}

// sanitizePluginDir 將 forgeTitle 轉換為安全的目錄名稱
// 保留 Unicode 字母/數字（支援中日韓等文字），空格轉連字號，加短 hash 防碰撞
func sanitizePluginDir(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Sprintf("plugin-%x", sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano()))))[:14]
	}

	var buf strings.Builder
	prevDash := false
	for _, c := range title {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			buf.WriteRune(c)
			prevDash = false
		} else if c == ' ' || c == '-' || c == '_' {
			if !prevDash && buf.Len() > 0 {
				buf.WriteRune('-')
				prevDash = true
			}
		}
	}
	slug := strings.TrimRight(buf.String(), "-")
	if slug == "" {
		slug = "plugin"
	}

	// 加 6 字元短 hash 避免同名碰撞
	h := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", title, time.Now().UnixNano())))
	return fmt.Sprintf("%s-%x", slug, h[:3])
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// executePluginForge 啟動插件鍛造 Sub-Agent（同步阻塞到完成）
// session 可為 nil（沒有 WebSocket 時不發意圖事件）
func (h *WsHandler) executePluginForge(session *WsSession, memberID, forgeTitle, userPrompt, toolCallID string) map[string]interface{} {
	log.Printf("[PluginForge] starting sub-agent: title=%q member=%s tool_call_id=%s", forgeTitle, memberID, toolCallID)

	sendWS := func(msg map[string]interface{}) {
		if toolCallID != "" {
			msg["tool_call_id"] = toolCallID
		}
		if session != nil {
			if err := session.Send(msg); err != nil {
				log.Printf("[PluginForge] WS send error: %v (type=%v)", err, msg["type"])
			} else {
				log.Printf("[PluginForge] WS sent: type=%v", msg["type"])
			}
		}
	}

	// forge 使用獨立 context：WS 斷線不會取消，只有使用者明確中斷才會取消（無 timeout）
	forgeCtx, forgeCancel := context.WithCancel(context.Background())
	defer forgeCancel()

	var forgeOp *sessionOperation
	if session != nil {
		forgeOp = newSessionOperation(sessionOperationKindPluginForge, "", toolCallID, forgeCancel)
		forgeOp.skipOnDisconnect = true
		session.registerOperation(forgeOp)
		defer session.unregisterOperation(forgeOp.id)
	}

	cancelledResult := func() map[string]interface{} {
		log.Printf("[PluginForge] forge 被使用者中斷")
		sendWS(map[string]interface{}{
			"type":    "sub_agent_complete",
			"status":  "cancelled",
			"message": "已中斷",
		})
		return map[string]interface{}{
			"status":  "cancelled",
			"message": "已中斷",
		}
	}

	checkForgeContext := func() map[string]interface{} {
		if errors.Is(forgeCtx.Err(), context.Canceled) {
			return cancelledResult()
		}
		return nil
	}

	// keepalive goroutine：定期送心跳防止 WS 閒置斷線
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sendWS(map[string]interface{}{"type": "sub_agent_intent", "step": "forge_heartbeat"})
			case <-keepaliveDone:
				return
			case <-forgeCtx.Done():
				return
			}
		}
	}()

	// Sub-Agent 模型繼承 Main Agent 的模型選擇
	forgeModel := "claude-sonnet-4-6" // fallback
	if session != nil && session.model != "" {
		forgeModel = session.model
	}
	log.Printf("[PluginForge] using model: %s (session model: %v)", forgeModel, session != nil && session.model != "")

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

	cleanDir := sanitizePluginDir(forgeTitle)

	userPromptFull := fmt.Sprintf("插件目錄名稱：plugins/%s/\n用戶需求：%s", cleanDir, userPrompt)
	workDir := filepath.Join(vaultRoot, memberID)

	// 清理舊的未完成 forge 目錄（沒有 bundle.js 的插件目錄 = forge 沒跑完）
	pluginsRoot := filepath.Join(workDir, "plugins")
	if dirEntries, err := os.ReadDir(pluginsRoot); err == nil {
		for _, de := range dirEntries {
			if !de.IsDir() {
				continue
			}
			subDir := filepath.Join(pluginsRoot, de.Name())
			files, _ := os.ReadDir(subDir)
			hasBundleJS := false
			hasSourceFiles := false
			for _, f := range files {
				name := f.Name()
				if name == "bundle.js" {
					hasBundleJS = true
				}
				if strings.HasSuffix(name, ".tsx") || strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".css") {
					hasSourceFiles = true
				}
			}
			if hasSourceFiles && !hasBundleJS {
				log.Printf("[PluginForge] cleanup incomplete forge dir: %s", de.Name())
				os.RemoveAll(subDir)
			}
		}
	}

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "step": "forge_init"})

	if result := checkForgeContext(); result != nil {
		return result
	}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--model", forgeModel,
		"--max-thinking-tokens", "10000",
		"--dangerously-skip-permissions",
		"--mcp-config", "/app/config/claude-forge-settings.json",
		"--system-prompt", instructions,
		"-p", userPromptFull,
	}

	log.Printf("[PluginForge] === SUB-AGENT LAUNCH ===")
	log.Printf("[PluginForge] workDir=%s", workDir)
	log.Printf("[PluginForge] args=%v", args[:len(args)-2]) // 不印 -p prompt 內容（太長）
	log.Printf("[PluginForge] system-prompt length=%d bytes", len(instructions))
	if len(instructions) > 200 {
		log.Printf("[PluginForge] system-prompt preview: %s...", instructions[:200])
	}

	cmd := executor.NewClaudeCmdExported(forgeCtx, workDir, "plugin", memberID, args)
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

	// 讀取 Sub-Agent 事件，完整 log + 過濾後發送意圖流
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	lastIntent := ""
	eventCount := 0
	for scanner.Scan() {
		if result := checkForgeContext(); result != nil {
			return result
		}
		line := scanner.Text()
		if line == "" {
			continue
		}
		eventCount++
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(line), &parsed) != nil {
			log.Printf("[PluginForge-RAW] non-json: %s", truncateStr(line, 300))
			continue
		}

		evtType, _ := parsed["type"].(string)

		switch evtType {
		case "system":
			log.Printf("[PluginForge-EVT] system: %s", truncateStr(line, 500))

		case "assistant":
			if mc, ok := parsed["message"].(map[string]interface{}); ok {
				model, _ := mc["model"].(string)
				if eventCount <= 3 {
					log.Printf("[PluginForge-EVT] assistant model=%s", model)
				}
				if ca, ok := mc["content"].([]interface{}); ok {
					for _, block := range ca {
						bm, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						blockType, _ := bm["type"].(string)

						switch blockType {
						case "thinking":
							thinking, _ := bm["thinking"].(string)
							log.Printf("[PluginForge-THINK] %s", truncateStr(thinking, 500))

						case "text":
							text, _ := bm["text"].(string)
							log.Printf("[PluginForge-TEXT] %s", truncateStr(text, 300))

						case "tool_use":
							toolName, _ := bm["name"].(string)
							input, _ := bm["input"].(map[string]interface{})
							inputJSON, _ := json.Marshal(input)
							log.Printf("[PluginForge-TOOL] %s input=%s", toolName, truncateStr(string(inputJSON), 300))

							// 轉發意圖流到前端
							evt := map[string]interface{}{"type": "sub_agent_intent", "tool_name": toolName}
							switch toolName {
							case "Read", "Write", "Edit":
								if fp, _ := input["file_path"].(string); fp != "" {
									evt["file_name"] = filepath.Base(fp)
								}
							case "Bash":
								if d, _ := input["description"].(string); d != "" {
									evt["description"] = d
								} else if c, _ := input["command"].(string); c != "" {
									if len(c) > 50 {
										c = c[:50] + "..."
									}
									evt["command"] = c
								}
							case "Glob":
								if p, _ := input["pattern"].(string); p != "" {
									evt["pattern"] = p
								}
							case "Grep":
								if p, _ := input["pattern"].(string); p != "" {
									evt["pattern"] = p
								}
							}
							intentKey := toolName
							if fn, ok := evt["file_name"].(string); ok {
								intentKey += ":" + fn
							}
							if intentKey != lastIntent {
								lastIntent = intentKey
								sendWS(evt)
							}

						case "tool_result":
							content, _ := bm["content"].(string)
							log.Printf("[PluginForge-RESULT] %s", truncateStr(content, 300))
						}
					}
				}
			}

		case "result":
			log.Printf("[PluginForge-EVT] result: %s", truncateStr(line, 500))
			h.recordBilling(memberID, "forge", session.sessionID, parsed)
		}
	}
	log.Printf("[PluginForge] === SUB-AGENT STREAM END === total events=%d", eventCount)

	if waitErr := cmd.Wait(); waitErr != nil {
		if errors.Is(forgeCtx.Err(), context.Canceled) {
			return cancelledResult()
		}
		log.Printf("[PluginForge] sub-agent exited with error: %v", waitErr)
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": waitErr.Error()})
		return map[string]interface{}{"status": "error", "error": waitErr.Error()}
	}

	// 使用先前推導的 pluginDir
	pluginDir := cleanDir

	// 找到實際的 plugin 目錄和入口檔（*Plugin.tsx）
	entryFile := findPluginEntry(h.vaultFS, filepath.Join(memberID, "plugins", pluginDir))
	if entryFile == "" {
		pluginsPath := filepath.Join(memberID, "plugins")
		entries, _ := h.vaultFS.ListDir(pluginsPath)
		for _, e := range entries {
			if e.IsDir() {
				if ef := findPluginEntry(h.vaultFS, filepath.Join(memberID, "plugins", e.Name())); ef != "" {
					pluginDir = e.Name()
					entryFile = ef
					break
				}
			}
		}
	}

	if entryFile == "" {
		// 列出插件目錄內容以利偵錯
		debugDir := filepath.Join(workDir, "plugins", pluginDir)
		dirEntries, _ := os.ReadDir(debugDir)
		var fileNames []string
		for _, de := range dirEntries {
			fileNames = append(fileNames, de.Name())
		}
		errMsg := fmt.Sprintf("找不到入口檔案 *Plugin.tsx，插件目錄 %s 內容: %v", pluginDir, fileNames)
		log.Printf("[PluginForge] %s", errMsg)
		sendWS(map[string]interface{}{"type": "sub_agent_complete", "status": "error", "error": errMsg})
		return map[string]interface{}{"status": "error", "error": errMsg}
	}

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "step": "forge_compile"})

	// 如果插件目錄有 package.json，先安裝第三方依賴
	pluginAbsDir := filepath.Join(workDir, "plugins", pluginDir)
	pkgJSONPath := filepath.Join(pluginAbsDir, "package.json")
	if _, statErr := os.Stat(pkgJSONPath); statErr == nil {
		npmCmd := exec.CommandContext(forgeCtx, "npm", "install", "--production", "--no-audit", "--no-fund")
		npmCmd.Dir = pluginAbsDir
		npmOut, npmErr := npmCmd.CombinedOutput()
		if npmErr != nil {
			log.Printf("[PluginForge] npm install failed: %s", string(npmOut))
		} else {
			log.Printf("[PluginForge] npm install success in %s", pluginAbsDir)
		}
	}

	// esbuild 打包（只 external 共享模組，第三方庫打包進 bundle）
	entryPath := filepath.Join(pluginAbsDir, entryFile)
	bundlePath := filepath.Join(workDir, "plugins", pluginDir, "bundle.js")
	esbuildCmd := exec.CommandContext(forgeCtx, "node", "/app/config/esbuild-plugin-bundle.mjs",
		entryPath, bundlePath)
	esbuildOut, esbuildErr := esbuildCmd.CombinedOutput()
	if esbuildErr != nil {
		if errors.Is(forgeCtx.Err(), context.Canceled) {
			return cancelledResult()
		}
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

	sendWS(map[string]interface{}{"type": "sub_agent_intent", "step": "forge_register"})

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

	completeMsg := map[string]interface{}{"type": "sub_agent_complete", "status": "success", "title": forgeTitle,
		"plugin": map[string]interface{}{"id": pluginID, "pluginDir": pluginDir, "bundleHash": bundleHash}}
	sendWS(completeMsg)
	sendWS(map[string]interface{}{"type": "vault_changed"})

	// 若 WS 已斷線，儲存結果供使用者 reconnect 時交付
	if session != nil {
		if err := session.Send(map[string]interface{}{"type": "ping"}); err != nil {
			log.Printf("[PluginForge] WS disconnected, storing pending result for member %s", memberID)
			h.pendingForgeResult.Store(memberID, completeMsg)
		}
	}

	return map[string]interface{}{
		"status": "success", "title": forgeTitle,
		"pluginDir": pluginDir, "bundleHash": bundleHash,
	}
}

func (h *WsHandler) maybeGenerateTitle(session *WsSession, userMsg, assistantMsg string) {
	log.Printf("[SessionMapping] maybeGenerateTitle called — sessionID=%s, member=%s, mode=%s",
		session.sessionID, session.memberID, session.mode)

	msgs, _, err := h.chatStore.GetMessagesAfter(context.Background(), session.sessionID, session.mode, "", 5)
	if err != nil {
		log.Printf("[SessionMapping] GetMessagesAfter error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	if len(msgs) > 2 {
		log.Printf("[SessionMapping] skip title generation — sessionID=%s, msgCount=%d (>2, not first round)",
			session.sessionID, len(msgs))
		return
	}
	log.Printf("[SessionMapping] will generate title — sessionID=%s, msgCount=%d", session.sessionID, len(msgs))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aiServiceURL := os.Getenv("AI_SERVICE_URL")
	if aiServiceURL == "" {
		aiServiceURL = "http://chatbot.svc.local:8000"
	}

	titleLang := session.lang
	if titleLang == "" {
		titleLang = "繁體中文"
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"userMessage":      userMsg,
		"assistantMessage": assistantMsg,
		"lang":             titleLang,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", aiServiceURL+"/cubelv/generate_thread_title", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[SessionMapping] title request create error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[SessionMapping] calling ai-service for title — sessionID=%s, url=%s", session.sessionID, aiServiceURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SessionMapping] title API call FAILED — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("[SessionMapping] title API responded — sessionID=%s, status=%d", session.sessionID, resp.StatusCode)

	var result struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[SessionMapping] title JSON decode error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	if result.Title == "" {
		log.Printf("[SessionMapping] title is empty — sessionID=%s", session.sessionID)
		return
	}

	log.Printf("[SessionMapping] AddSessionMapping — sessionID=%s, member=%s, title=%s", session.sessionID, session.memberID, result.Title)
	if err := h.chatStore.AddSessionMapping(context.Background(), session.memberID, session.sessionID, result.Title, session.mode); err != nil {
		log.Printf("[SessionMapping] AddSessionMapping FAILED — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	log.Printf("[SessionMapping] AddSessionMapping OK — sessionID=%s", session.sessionID)

	session.Send(map[string]interface{}{
		"type":       "session_title_update",
		"memberID":   session.memberID,
		"session_id": session.sessionID,
		"title":      result.Title,
	})
}

// generateTitleFromUserMessage 在 user 發訊息時立刻生成 title
func (h *WsHandler) generateTitleFromUserMessage(session *WsSession, msg map[string]interface{}) {
	// 先確保 session mapping 存在（用空 title）
	if err := h.chatStore.AddSessionMapping(context.Background(), session.memberID, session.sessionID, "", session.mode); err != nil {
		log.Printf("[SessionMapping] EnsureMapping FAILED — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	log.Printf("[SessionMapping] EnsureMapping OK — sessionID=%s, member=%s", session.sessionID, session.memberID)

	// 檢查是否第一輪對話
	msgs, _, err := h.chatStore.GetMessagesAfter(context.Background(), session.sessionID, session.mode, "", 5)
	if err != nil {
		log.Printf("[SessionMapping] GetMessagesAfter error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	if len(msgs) > 1 {
		log.Printf("[SessionMapping] skip title — sessionID=%s, msgCount=%d (not first message)", session.sessionID, len(msgs))
		return
	}

	// 取得 user message
	messageText, _ := msg["message"].(string)
	if messageText == "" {
		if msgObj, ok := msg["message"].(map[string]interface{}); ok {
			messageText, _ = msgObj["content"].(string)
		}
	}
	if messageText == "" {
		return
	}

	// 呼叫 AI 生成 title
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	aiServiceURL := os.Getenv("AI_SERVICE_URL")
	if aiServiceURL == "" {
		aiServiceURL = "http://chatbot.svc.local:8000"
	}

	titleLang := session.lang
	if titleLang == "" {
		titleLang = "繁體中文"
	}
	reqBody, _ := json.Marshal(map[string]interface{}{
		"userMessage":      messageText,
		"assistantMessage": "",
		"lang":             titleLang,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", aiServiceURL+"/cubelv/generate_thread_title", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[SessionMapping] title request error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("[SessionMapping] calling ai-service for title (user msg only) — sessionID=%s", session.sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[SessionMapping] title API FAILED — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[SessionMapping] title JSON decode error — sessionID=%s, err=%v", session.sessionID, err)
		return
	}
	if result.Title == "" {
		log.Printf("[SessionMapping] title is empty — sessionID=%s", session.sessionID)
		return
	}

	log.Printf("[SessionMapping] AddSessionMapping — sessionID=%s, title=%s", session.sessionID, result.Title)
	if err := h.chatStore.AddSessionMapping(context.Background(), session.memberID, session.sessionID, result.Title, session.mode); err != nil {
		log.Printf("[SessionMapping] AddSessionMapping FAILED — sessionID=%s, err=%v", session.sessionID, err)
		return
	}

	session.Send(map[string]interface{}{
		"type":       "session_title_update",
		"memberID":   session.memberID,
		"session_id": session.sessionID,
		"title":      result.Title,
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
