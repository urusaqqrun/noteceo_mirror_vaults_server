package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/executor"
)

// ChatCLI 介面：遠端 CLI Worker 代理
type ChatCLI interface {
	SendMessage(content interface{}) (<-chan executor.StreamEvent, error)
	IsAlive() bool
	Kill()
	Interrupt() bool
	Pid() int
	GetCacheBuilt() bool
	SetCacheBuilt(v bool)
}

// WorkerClient 封裝對 CLI Worker 的 HTTP 呼叫
type WorkerClient struct {
	baseURL    string
	secret     string
	httpClient *http.Client
}

func NewWorkerClient(baseURL, secret string) *WorkerClient {
	return &WorkerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		httpClient: &http.Client{
			Timeout: 0, // SSE 串流不設超時
		},
	}
}

func (w *WorkerClient) do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.secret != "" {
		req.Header.Set("X-Internal-Secret", w.secret)
	}
	return w.httpClient.Do(req)
}

func (w *WorkerClient) doJSON(ctx context.Context, method, path string, body interface{}, result interface{}) error {
	resp, err := w.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker %s %s → %d: %s", method, path, resp.StatusCode, string(b))
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

type CreateSessionReq struct {
	MemberID  string `json:"memberID"`
	SessionID string `json:"sessionID"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	IsNew     bool   `json:"isNew"`
}

type CreateSessionResp struct {
	Status    string `json:"status"`
	SessionID string `json:"sessionID"`
	Pid       int    `json:"pid"`
}

func (w *WorkerClient) CreateSession(ctx context.Context, req CreateSessionReq) (*CreateSessionResp, error) {
	var resp CreateSessionResp
	if err := w.doJSON(ctx, "POST", "/internal/session/create", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type InterruptReq struct {
	MemberID  string `json:"memberID"`
	SessionID string `json:"sessionID"`
}

func (w *WorkerClient) Interrupt(ctx context.Context, req InterruptReq) error {
	return w.doJSON(ctx, "POST", "/internal/session/interrupt", req, nil)
}

type KillReq struct {
	MemberID  string `json:"memberID"`
	SessionID string `json:"sessionID"`
}

func (w *WorkerClient) KillSession(ctx context.Context, req KillReq) error {
	return w.doJSON(ctx, "POST", "/internal/session/kill", req, nil)
}

type SendMessageReq struct {
	MemberID  string      `json:"memberID"`
	SessionID string      `json:"sessionID"`
	Content   interface{} `json:"content"`
}

// SendMessageSSE 發送訊息並回傳 SSE 事件串流
func (w *WorkerClient) SendMessageSSE(ctx context.Context, req SendMessageReq) (<-chan executor.StreamEvent, error) {
	resp, err := w.do(ctx, "POST", "/internal/session/message", req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("worker send message → %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan executor.StreamEvent, 256)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			var ev struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				log.Printf("[WorkerClient] SSE parse error: %v", err)
				continue
			}
			ch <- executor.StreamEvent{Type: ev.Type, Data: ev.Data}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[WorkerClient] SSE read error: %v", err)
			ch <- executor.StreamEvent{Type: "error", Data: fmt.Sprintf("SSE read error: %v", err)}
		}
	}()
	return ch, nil
}

type ForgeReq struct {
	MemberID   string `json:"memberID"`
	Title      string `json:"title"`
	Prompt     string `json:"prompt"`
	ToolCallID string `json:"toolCallID"`
	ForgeType  string `json:"forgeType"`
}

// ForgeSSE 發送 forge 請求並回傳 SSE 事件串流
func (w *WorkerClient) ForgeSSE(ctx context.Context, req ForgeReq) (<-chan json.RawMessage, error) {
	resp, err := w.do(ctx, "POST", "/internal/forge", req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("worker forge → %d: %s", resp.StatusCode, string(b))
	}

	ch := make(chan json.RawMessage, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			ch <- json.RawMessage(data)
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[WorkerClient] Forge SSE read error: %v", err)
			errMsg, _ := json.Marshal(map[string]string{"type": "error", "error": fmt.Sprintf("SSE read error: %v", err)})
			ch <- json.RawMessage(errMsg)
		}
	}()
	return ch, nil
}

// RebuildReq 插件重新編譯請求
type RebuildReq struct {
	MemberID  string `json:"memberID"`
	PluginDir string `json:"pluginDir"`
}

// RebuildResp 插件重新編譯回應
type RebuildResp struct {
	Status     string `json:"status"`
	PluginDir  string `json:"pluginDir"`
	BundleHash string `json:"bundleHash"`
	Error      string `json:"error,omitempty"`
}

// Rebuild 觸發 CLI Worker 重新編譯插件（npm install + esbuild + validator）
func (w *WorkerClient) Rebuild(ctx context.Context, req RebuildReq) (*RebuildResp, error) {
	var resp RebuildResp
	if err := w.doJSON(ctx, "POST", "/internal/rebuild", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RemoteCLI 實現 ChatCLI 介面，透過 WorkerClient 代理到 CLI Worker
type RemoteCLI struct {
	worker    *WorkerClient
	memberID  string
	sessionID  string
	model      string
	alive      atomic.Bool
	pid        int
	mu         sync.Mutex
	cacheBuilt bool
	cancelCtx  context.Context
	cancelFunc context.CancelFunc
}

func NewRemoteCLI(worker *WorkerClient, memberID, sessionID, model string, pid int) *RemoteCLI {
	ctx, cancel := context.WithCancel(context.Background())
	r := &RemoteCLI{
		worker:     worker,
		memberID:   memberID,
		sessionID:  sessionID,
		model:      model,
		pid:        pid,
		cancelCtx:  ctx,
		cancelFunc: cancel,
	}
	r.alive.Store(true)
	return r
}

func (r *RemoteCLI) SendMessage(content interface{}) (<-chan executor.StreamEvent, error) {
	if !r.alive.Load() {
		return nil, fmt.Errorf("remote CLI is not alive")
	}
	return r.worker.SendMessageSSE(r.cancelCtx, SendMessageReq{
		MemberID:  r.memberID,
		SessionID: r.sessionID,
		Content:   content,
	})
}

func (r *RemoteCLI) IsAlive() bool {
	return r.alive.Load()
}

func (r *RemoteCLI) Kill() {
	r.alive.Store(false)
	r.cancelFunc()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.worker.KillSession(ctx, KillReq{
		MemberID:  r.memberID,
		SessionID: r.sessionID,
	}); err != nil {
		log.Printf("[RemoteCLI] kill failed: %v", err)
	}
}

func (r *RemoteCLI) Interrupt() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.worker.Interrupt(ctx, InterruptReq{
		MemberID:  r.memberID,
		SessionID: r.sessionID,
	}); err != nil {
		log.Printf("[RemoteCLI] interrupt failed: %v", err)
		return false
	}
	return true
}

func (r *RemoteCLI) Pid() int {
	return r.pid
}

func (r *RemoteCLI) GetCacheBuilt() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cacheBuilt
}

func (r *RemoteCLI) SetCacheBuilt(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cacheBuilt = v
}
