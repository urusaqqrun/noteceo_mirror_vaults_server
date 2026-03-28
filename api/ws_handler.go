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
	model     string
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
}

// HandleWarmup pre-warms a CLI process into the pool.
func (h *WsHandler) HandleWarmup(w http.ResponseWriter, r *http.Request) {
	memberID := r.URL.Query().Get("memberID")
	if memberID == "" {
		http.Error(w, "memberID required", 400)
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
	memberID := q.Get("memberID")
	sessionID := q.Get("sessionID")
	mode := q.Get("mode")
	model := q.Get("model")
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
		model:     model,
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
	// attachedItems 在 msg["message"] 內，不在頂層
	if msgObj != nil {
		if items, ok := msgObj["attachedItems"]; ok {
			if b, err := json.Marshal(items); err == nil {
				userMsg.AttachedItems = b
			}
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
				"type":     "stream_error",
				"memberID": session.memberID,
				"error":    errStr,
			})

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

		// 10. Generate session title (fire-and-forget)
		h.maybeGenerateTitle(session, messageText, accumulatedText)
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

func buildInstruction(messageText string, msgObj map[string]interface{}, pageCtx map[string]interface{}) string {
	var parts []string
	parts = append(parts, messageText)

	// 1. pageContext → 告訴 AI 用戶當前在看什麼
	if pageCtx != nil {
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

	// 2. attachedItems → 非圖片項目直接序列化為完整 JSON（保留所有欄位）
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
				// 直接將完整 item 序列化為 JSON，保留所有欄位（content、tags 等）
				itemJSON, err := json.Marshal(item)
				if err == nil {
					parts = append(parts, fmt.Sprintf("\n[附加項目]\n%s", string(itemJSON)))
				}
			}
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
