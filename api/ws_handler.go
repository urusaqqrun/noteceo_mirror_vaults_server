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
	conn      *websocket.Conn
	memberID  string
	sessionID string
	mode      string
	taskID    string
	status    string // idle, asking, interrupted
	mu        sync.Mutex
	cli       *executor.StreamCLI
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

// SnapshotStore DB-based vault snapshot CRUD.
type SnapshotStore interface {
	GetSnapshot(ctx context.Context, memberID string) ([]database.SnapshotRow, error)
	UpsertSnapshotFiles(ctx context.Context, memberID string, files []database.SnapshotRow) error
	DeleteSnapshotFiles(ctx context.Context, memberID string, paths []string) error
	ReplaceSnapshot(ctx context.Context, memberID string, files []database.SnapshotRow) error
	SnapshotExists(ctx context.Context, memberID string) (bool, error)
}

// WsHandler is the WebSocket endpoint handler.
type WsHandler struct {
	executor      StreamTaskExecutor
	store         TaskStore
	chatStore     ChatStore
	snapshotStore SnapshotStore
	vaultRoot     string
	vaultFS       mirror.VaultFS
	sessions      sync.Map // sessionKey -> *WsSession
	cliPool       sync.Map // memberID -> *executor.StreamCLI (warmup pool)
}

// NewWsHandler creates a new WsHandler.
func NewWsHandler(exec StreamTaskExecutor, store TaskStore, chatStore ChatStore, snapshotStore SnapshotStore, vaultRoot string, vaultFS mirror.VaultFS) *WsHandler {
	return &WsHandler{
		executor:      exec,
		store:         store,
		chatStore:     chatStore,
		snapshotStore: snapshotStore,
		vaultRoot:     vaultRoot,
		vaultFS:       vaultFS,
	}
}

// RegisterRoutes registers the WebSocket route on the provided mux.
func (h *WsHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ws/chat", h.HandleWebSocket)
	mux.HandleFunc("POST /cli_warmup", h.HandleWarmup)
}

// HandleWarmup pre-warms a CLI process into the pool.
func (h *WsHandler) HandleWarmup(w http.ResponseWriter, r *http.Request) {
	memberID := r.URL.Query().Get("memberID")
	if memberID == "" {
		http.Error(w, "memberID required", 400)
		return
	}

	if val, ok := h.cliPool.Load(memberID); ok {
		if cli, ok := val.(*executor.StreamCLI); ok && cli.IsAlive() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]string{"status": "already_warm"})
			return
		}
		h.cliPool.Delete(memberID)
	}

	go func() {
		warmupStart := time.Now()
		workDir := filepath.Join(h.vaultRoot, memberID)
		cli, err := executor.NewStreamCLI(workDir, "chat", memberID, "", 5*time.Minute)
		if err != nil {
			log.Printf("[Warmup] CLI start failed for %s: %v", memberID, err)
			return
		}
		h.cliPool.Store(memberID, cli)
		log.Printf("[CacheProfile] Warmup DONE — %dms, member=%s, pid=%d",
			time.Since(warmupStart).Milliseconds(), memberID, cli.Pid())
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]string{"status": "warming"})
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

	isNew := q.Get("isNew") == "true"

	go func() {
		wsCliStart := time.Now()
		workDir := filepath.Join(h.vaultRoot, memberID)

		if isNew {
			if val, loaded := h.cliPool.LoadAndDelete(memberID); loaded {
				if cli, ok := val.(*executor.StreamCLI); ok && cli.IsAlive() {
					session.mu.Lock()
					session.cli = cli
					session.mu.Unlock()
					log.Printf("[CacheProfile] WS goroutine: reused warm CLI — %dms, pid=%d",
						time.Since(wsCliStart).Milliseconds(), cli.Pid())
					return
				}
			}
			cli, err := executor.NewStreamCLI(workDir, "chat", memberID, "", 5*time.Minute)
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
						ID:         m.ID,
						Role:       m.Role,
						Content:    m.Content,
						Thinking:   m.Thinking,
						ToolCalls:  m.ToolCalls,
						ToolCallID: m.ToolCallID,
						CreatedAt:  m.CreatedAt,
					})
				}

				rebuildStart := time.Now()
				if err := executor.RebuildSessionJSONL(sessionID, workDir, memberID, smList); err != nil {
					log.Printf("[WS] rebuild JSONL error: %v", err)
				} else {
					log.Printf("[CacheProfile] WS goroutine: RebuildSessionJSONL — %dms, msgs=%d",
						time.Since(rebuildStart).Milliseconds(), len(smList))
				}

				cli, err := executor.NewStreamCLI(workDir, "chat", memberID, sessionID, 5*time.Minute)
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

		cli, err := executor.NewStreamCLI(workDir, "chat", memberID, "", 5*time.Minute)
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

	if err := h.checkCredits(session.memberID); err != nil {
		session.Send(map[string]interface{}{
			"type":     "stream_error",
			"memberID": session.memberID,
			"error":    err.Error(),
		})
		return
	}

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
		Mode:      session.mode,
		Role:      "user",
		Content:   messageText,
	}
	if items, ok := msg["attachedItems"]; ok {
		if b, err := json.Marshal(items); err == nil {
			userMsg.AttachedItems = b
		}
	}
	h.chatStore.InsertChatMessage(context.Background(), userMsg)

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

	// 4. snapshot_before: read from DB
	snapBeforeStart := time.Now()
	beforeSnap, beforeIDMap, snapErr := h.loadBeforeSnapshot(session.memberID)
	if snapErr != nil {
		log.Printf("[WS] snapshot before error: %v", snapErr)
	} else {
		log.Printf("[CacheProfile] snapshot_before — %dms, files=%d",
			time.Since(snapBeforeStart).Milliseconds(), len(beforeSnap))
	}

	// 5. Send message to StreamCLI
	sendStart := time.Now()
	eventCh, err := cli.SendMessage(messageText)
	log.Printf("[CacheProfile] sendMessage DONE — %dms", time.Since(sendStart).Milliseconds())
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
	sentToolUseIDs := map[string]bool{}
	var tokenUsage json.RawMessage

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
				accumulatedText = ""
				accumulatedThinking = ""
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
						session.Send(map[string]interface{}{
							"type":        "thinking_token",
							"memberID":    session.memberID,
							"token":       text,
							"accumulated": accumulatedThinking,
						})
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
							if text != "" && text != accumulatedThinking {
								accumulatedThinking = text
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
						session.Send(map[string]interface{}{
							"type":    "message",
							"role":    "assistant",
							"content": "",
							"tool_calls": []map[string]interface{}{{
								"id":   toolID,
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

		case eventType == "user":
			if msgContent, ok := parsed["message"].(map[string]interface{}); ok {
				if contentArr, ok := msgContent["content"].([]interface{}); ok {
					for _, block := range contentArr {
						blockMap, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						if blockMap["type"] == "tool_result" {
							session.Send(map[string]interface{}{
								"type":         "message",
								"role":         "tool",
								"content":      blockMap["content"],
								"tool_call_id": blockMap["tool_use_id"],
							})
						}
					}
				}
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

	// 9. snapshot_after: incremental EFS scan + diff + DB update
	if snapErr == nil {
		snapAfterStart := time.Now()
		afterSnap, afterIDMap, afterErr := executor.TakeIncrementalSnapshot(
			h.vaultFS, session.memberID, beforeSnap, beforeIDMap,
		)
		if afterErr != nil {
			log.Printf("[WS] incremental snapshot after error: %v", afterErr)
		} else {
			diff := executor.ComputeDiff(beforeSnap, afterSnap)
			hasChanges := len(diff.Created)+len(diff.Modified)+len(diff.Deleted)+len(diff.Moved) > 0
			log.Printf("[CacheProfile] snapshot_after INCREMENTAL+diff — %dms, changed=%v, +%d ~%d -%d mv%d",
				time.Since(snapAfterStart).Milliseconds(), hasChanges,
				len(diff.Created), len(diff.Modified), len(diff.Deleted), len(diff.Moved))

			if hasChanges {
				h.updateDBSnapshot(session.memberID, afterSnap, afterIDMap, diff)
				session.Send(map[string]interface{}{
					"type":     "vault_changed",
					"memberID": session.memberID,
				})
			}
		}
	}

	log.Printf("[CacheProfile] handleMessage DONE — total=%dms, member=%s",
		time.Since(handleStart).Milliseconds(), session.memberID)

	// 10. Generate session title (fire-and-forget)
	go h.maybeGenerateTitle(session, messageText, accumulatedText)
}

// loadBeforeSnapshot reads the snapshot from DB. Falls back to full EFS scan
// if no DB record exists (first use), and stores the result to DB.
func (h *WsHandler) loadBeforeSnapshot(memberID string) (map[string]executor.FileSnapshot, map[string]string, error) {
	ctx := context.Background()
	rows, err := h.snapshotStore.GetSnapshot(ctx, memberID)
	if err == nil && len(rows) > 0 {
		snap, idMap := dbRowsToSnapshot(rows)
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
	return snap, idMap, nil
}

// updateDBSnapshot applies diff changes to the DB snapshot incrementally.
func (h *WsHandler) updateDBSnapshot(memberID string, afterSnap map[string]executor.FileSnapshot, afterIDMap map[string]string, diff executor.VaultDiff) {
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
	newCLI, err := executor.NewStreamCLI(workDir, "chat", session.memberID, session.sessionID, 5*time.Minute)
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
			ID:         m.ID,
			Role:       m.Role,
			Content:    m.Content,
			Thinking:   m.Thinking,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			CreatedAt:  m.CreatedAt,
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
	if session.cli != nil {
		session.cli.Kill()
	}
	session.mu.Unlock()

	session.Send(map[string]interface{}{
		"type":     "stream_interrupted",
		"memberID": session.memberID,
		"message":  "已中斷",
	})
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

func buildInstruction(messageText string, msg map[string]interface{}) string {
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

	reqBody, _ := json.Marshal(map[string]string{"memberId": memberID})
	resp, err := http.Post(mcURL+"/api/internal/quota/ai/check", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[WS] credits check failed (allowing): %v", err)
		return nil
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Allowed bool    `json:"allowed"`
			Reason  string  `json:"reason"`
			Balance float64 `json:"balance"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil
	}

	if result.Success && result.Data.Allowed == false {
		return fmt.Errorf("INSUFFICIENT_CREDITS: 額度不足 (餘額: %.2f)", result.Data.Balance)
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
