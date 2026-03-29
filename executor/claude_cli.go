package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ClaudeExecutor 管理 Claude CLI process
type ClaudeExecutor struct {
	maxConcurrent int
	timeout       time.Duration
	vaultRoot     string
	sem           chan struct{} // semaphore 控制並發
	running       int32        // atomic 計數佔用的 semaphore slots

	mu        sync.Mutex
	processes map[string]*exec.Cmd // taskID → 正在執行的 cmd
}

func NewClaudeExecutor(maxConcurrent int, timeout time.Duration, vaultRoot string) *ClaudeExecutor {
	return &ClaudeExecutor{
		maxConcurrent: maxConcurrent,
		timeout:       timeout,
		vaultRoot:     vaultRoot,
		sem:           make(chan struct{}, maxConcurrent),
		processes:     make(map[string]*exec.Cmd),
	}
}

// ExecuteTask 啟動 Claude CLI 執行任務
// workDir 為用戶的 Vault 目錄路徑，scope 和 userID 會注入環境變數供 hooks 使用
func (e *ClaudeExecutor) ExecuteTask(ctx context.Context, taskID, workDir, instruction, scope, userID string) (string, error) {
	// 等待 semaphore（並發排隊）
	select {
	case e.sem <- struct{}{}:
		atomic.AddInt32(&e.running, 1)
		defer func() {
			<-e.sem
			atomic.AddInt32(&e.running, -1)
		}()
	case <-ctx.Done():
		return "", fmt.Errorf("task %s cancelled while waiting in queue", taskID)
	}

	// 建立帶超時的 context
	execCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// 構建 Claude CLI 命令
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--output-format", "text",
		"-p", instruction,
	}

	cmd := exec.CommandContext(execCtx, "claude", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"TASK_SCOPE="+scope,
		"VAULT_USER_ID="+userID,
	)

	e.mu.Lock()
	e.processes[taskID] = cmd
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		delete(e.processes, taskID)
		e.mu.Unlock()
	}()

	output, err := cmd.CombinedOutput()
	if execCtx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("task %s timed out after %v", taskID, e.timeout)
	}
	if err != nil {
		return string(output), fmt.Errorf("claude cli error: %w\noutput: %s", err, string(output))
	}

	return string(output), nil
}

// Cancel 中斷指定任務
func (e *ClaudeExecutor) Cancel(taskID string) error {
	e.mu.Lock()
	cmd, ok := e.processes[taskID]
	e.mu.Unlock()

	if !ok {
		return nil
	}

	if cmd.Process != nil {
		log.Printf("[ClaudeExecutor] killing task %s (pid=%d)", taskID, cmd.Process.Pid)
		return cmd.Process.Kill()
	}
	return nil
}

// RunningCount 目前佔用 semaphore 的任務數
func (e *ClaudeExecutor) RunningCount() int {
	return int(atomic.LoadInt32(&e.running))
}

// AvailableSlots 可用的 semaphore 插槽數
func (e *ClaudeExecutor) AvailableSlots() int {
	return e.maxConcurrent - e.RunningCount()
}

// ActiveCount 目前正在執行的任務數（已啟動 CLI process）
func (e *ClaudeExecutor) ActiveCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.processes)
}

// StreamEvent represents a single event from the Claude CLI streaming output.
type StreamEvent struct {
	Type string // "stdout", "error", "done"
	Data string
}

// ExecuteTaskStream runs the Claude CLI in streaming mode (--output-format stream-json)
// and sends each stdout line as a StreamEvent to eventCh. The caller must consume eventCh.
func (e *ClaudeExecutor) ExecuteTaskStream(
	ctx context.Context,
	taskID, workDir, instruction, scope, userID string,
	eventCh chan<- StreamEvent,
) error {
	select {
	case e.sem <- struct{}{}:
		atomic.AddInt32(&e.running, 1)
		defer func() {
			<-e.sem
			atomic.AddInt32(&e.running, -1)
		}()
	case <-ctx.Done():
		return fmt.Errorf("task %s cancelled while waiting in queue", taskID)
	}

	execCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	args := []string{
		"--print",
		"--verbose",
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"-p", instruction,
	}

	cmd := exec.CommandContext(execCtx, "claude", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"TASK_SCOPE="+scope,
		"VAULT_USER_ID="+userID,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	e.mu.Lock()
	e.processes[taskID] = cmd
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.processes, taskID)
		e.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude cli: %w", err)
	}

	var stderrBuf strings.Builder
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			stderrBuf.WriteString(scanner.Text() + "\n")
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		select {
		case eventCh <- StreamEvent{Type: "stdout", Data: line}:
		case <-execCtx.Done():
			return execCtx.Err()
		}
	}

	<-stderrDone
	waitErr := cmd.Wait()

	if execCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("task %s timed out after %v", taskID, e.timeout)
	}
	if waitErr != nil {
		errMsg := stderrBuf.String()
		if len(errMsg) > 500 {
			errMsg = errMsg[:500]
		}
		return fmt.Errorf("claude cli exit code %d: %s", cmd.ProcessState.ExitCode(), errMsg)
	}

	return nil
}

// ---------------------------------------------------------------------------
// StreamCLI — 長駐 process，用 --print --input-format stream-json 取得真正逐字串流
// ---------------------------------------------------------------------------

// StreamCLI 長駐 Claude CLI process，stdin 送訊息、stdout 逐字串流
type StreamCLI struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Scanner
	mu        sync.Mutex
	workDir    string
	sessionID  string
	alive      bool
	idleTimer  *time.Timer
	idleTTL    time.Duration
	CacheBuilt bool // true after first message builds Anthropic prompt cache
}

// NewStreamCLI 啟動帶有 streaming 功能的長駐 CLI。
// model 若非空，會傳 --model 給 claude CLI 以指定使用的模型。
func NewStreamCLI(workDir, scope, userID, sessionID, model string, resume bool, idleTTL time.Duration) (*StreamCLI, error) {
	funcStart := time.Now()

	// 清理可能殘留的 session lock file（CLI 被 kill 後 lock 不會自動清除）
	if sessionID != "" {
		cleanStaleSessionLock(workDir, sessionID)
	}

	// 診斷：計算當前正在跑的 claude process 數量
	if out, err := exec.Command("pgrep", "-c", "claude").Output(); err == nil {
		log.Printf("[StreamCLI-diag] current claude process count: %s", strings.TrimSpace(string(out)))
	}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--input-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--mcp-config", "/home/mirror/.claude/settings.json",
	}

	if model != "" {
		args = append(args, "--model", model)
	}

	if sessionID != "" {
		if resume {
			args = append(args, "--resume", sessionID)
			log.Printf("[StreamCLI] resuming session %s", sessionID)
		} else {
			args = append(args, "--session-id", sessionID)
			log.Printf("[StreamCLI] new session with ID %s", sessionID)
		}
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"TASK_SCOPE="+scope,
		"VAULT_USER_ID="+userID,
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmdStartTime := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude cli: %w", err)
	}
	log.Printf("[CacheProfile] cmd.Start DONE — %dms, pid=%d, workDir=%s, sessionID=%s, resume=%v",
		time.Since(cmdStartTime).Milliseconds(), cmd.Process.Pid, workDir, sessionID, resume)

	// 背景讀取 stderr 並寫入 log，避免 CLI 錯誤被靜默吞掉
	go func() {
		stderrScanner := bufio.NewScanner(stderrPipe)
		stderrScanner.Buffer(make([]byte, 64*1024), 64*1024)
		for stderrScanner.Scan() {
			log.Printf("[StreamCLI-stderr] pid=%d: %s", cmd.Process.Pid, stderrScanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	s := &StreamCLI{
		cmd:       cmd,
		stdin:     stdinPipe,
		stdout:    scanner,
		idleTTL:   idleTTL,
		workDir:   workDir,
		sessionID: sessionID,
		alive:     true,
	}
	s.resetIdleTimer()

	log.Printf("[CacheProfile] NewStreamCLI total — %dms, pid=%d, workDir=%s, sessionID=%s, resume=%v",
		time.Since(funcStart).Milliseconds(), cmd.Process.Pid, workDir, sessionID, resume)
	return s, nil
}

// SendMessage 寫入使用者訊息，回傳 stdout 事件 channel（result 事件後關閉）
// content 可以是 string（純文字）或 []map[string]interface{}（多模態 content blocks）
func (s *StreamCLI) SendMessage(content interface{}) (<-chan StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.alive {
		return nil, fmt.Errorf("CLI process not alive")
	}

	s.resetIdleTimer()

	// 根據 content 類型構建 message
	var msgContent interface{}
	switch c := content.(type) {
	case string:
		msgContent = c // 純字串，CLI 直接吃
	default:
		msgContent = c // content blocks 陣列，CLI 也直接吃
	}

	msg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": msgContent,
		},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.stdin.Write(data); err != nil {
		s.alive = false
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	ch := make(chan StreamEvent, 128)
	go func() {
		defer close(ch)
		for s.stdout.Scan() {
			line := s.stdout.Text()
			if line == "" {
				continue
			}
			ch <- StreamEvent{Type: "stdout", Data: line}

			var parsed map[string]interface{}
			if json.Unmarshal([]byte(line), &parsed) == nil {
				if parsed["type"] == "result" {
					if sid, ok := parsed["session_id"].(string); ok && sid != "" {
						s.mu.Lock()
						s.sessionID = sid
						s.mu.Unlock()
					}
					return
				}
				if parsed["type"] == "system" {
					if sid, ok := parsed["session_id"].(string); ok && sid != "" {
						s.mu.Lock()
						s.sessionID = sid
						s.mu.Unlock()
					}
				}
			}
		}
		// stdout 關閉 → process 已死
		s.mu.Lock()
		s.alive = false
		s.mu.Unlock()
	}()

	return ch, nil
}

func (s *StreamCLI) Kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	if s.alive && s.cmd.Process != nil {
		log.Printf("[StreamCLI] killing pid=%d", s.cmd.Process.Pid)
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
		s.alive = false
	}
}

func (s *StreamCLI) IsAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive
}

func (s *StreamCLI) Pid() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

func (s *StreamCLI) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// cleanStaleSessionLock removes session lock files left by killed CLI processes.
// Searches multiple possible lock locations used by Claude CLI.
func cleanStaleSessionLock(workDir, sessionID string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	encodedWorkDir := encodeProjectPath(workDir)

	// Possible lock file locations
	candidates := []string{
		filepath.Join(homeDir, ".claude", "projects", encodedWorkDir, "sessions", sessionID+".lock"),
		filepath.Join(homeDir, ".claude", "sessions", sessionID+".lock"),
		filepath.Join(workDir, ".claude", "sessions", sessionID+".lock"),
	}

	for _, path := range candidates {
		if err := os.Remove(path); err == nil {
			log.Printf("[StreamCLI] cleaned stale lock: %s", path)
		}
	}

	// Also scan the project sessions dir for any .lock files and log them
	sessionsDir := filepath.Join(homeDir, ".claude", "projects", encodedWorkDir, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".lock") {
				log.Printf("[StreamCLI] found lock file in sessions dir: %s", e.Name())
			}
		}
	}
}

func (s *StreamCLI) resetIdleTimer() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(s.idleTTL, func() {
		log.Printf("[StreamCLI] idle timeout, killing pid=%d", s.cmd.Process.Pid)
		s.Kill()
	})
}

