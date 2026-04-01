package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// UID 隔離相關變數
var (
	uidIsolationAvailable bool
	mirrorUID             uint64
	mirrorGID             uint64
	vaultPermOnce         sync.Map // workDir → bool：追蹤已設定權限的 vault
)

func init() {
	// 查找 mirror user 的 UID/GID（root 環境下用於保底）
	if u, err := user.Lookup("mirror"); err == nil {
		mirrorUID, _ = strconv.ParseUint(u.Uid, 10, 32)
		mirrorGID, _ = strconv.ParseUint(u.Gid, 10, 32)
	}

	if os.Getuid() != 0 {
		log.Printf("[Sandbox] not running as root, UID isolation disabled")
		return
	}

	if testUIDIsolation() {
		uidIsolationAvailable = true
		log.Printf("[Sandbox] UID isolation available — per-member filesystem permissions enabled")
		migrateExistingVaults()
	} else {
		log.Printf("[Sandbox] UID isolation NOT effective (EFS may override UID), using mirror user fallback")
	}
}

// migrateExistingVaults 啟動時掃描所有既有 vault，chown + chmod 700 以堵住舊 UID 1000 漏洞
func migrateExistingVaults() {
	vaultRoot := os.Getenv("VAULT_ROOT")
	if vaultRoot == "" {
		vaultRoot = "/vaults"
	}

	entries, err := os.ReadDir(vaultRoot)
	if err != nil {
		log.Printf("[Sandbox] migrate: cannot read %s: %v", vaultRoot, err)
		return
	}

	start := time.Now()
	migrated := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "shared" {
			continue
		}
		memberID := e.Name()
		uid, gid := MemberCredentials(memberID)
		ensurePasswdEntry(uid, gid)
		workDir := filepath.Join(vaultRoot, memberID)
		EnsureVaultPermissions(workDir, uid, gid)
		migrated++
	}

	log.Printf("[Sandbox] vault migration complete: %d vaults in %dms", migrated, time.Since(start).Milliseconds())
}

// testUIDIsolation 在 /vaults/ 建立臨時目錄測試 chown + setuid 是否真正隔離
func testUIDIsolation() bool {
	vaultRoot := os.Getenv("VAULT_ROOT")
	if vaultRoot == "" {
		vaultRoot = "/vaults"
	}

	testDir := filepath.Join(vaultRoot, fmt.Sprintf(".uid-test-%d", time.Now().UnixNano()))
	defer os.RemoveAll(testDir)

	ownerUID, otherUID := uint32(12345), uint32(54321)

	if err := os.MkdirAll(testDir, 0700); err != nil {
		log.Printf("[Sandbox] UID test: mkdir failed: %v", err)
		return false
	}
	if err := os.Chown(testDir, int(ownerUID), int(ownerUID)); err != nil {
		log.Printf("[Sandbox] UID test: chown failed: %v", err)
		return false
	}

	testFile := filepath.Join(testDir, "test")
	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		log.Printf("[Sandbox] UID test: write failed: %v", err)
		return false
	}
	os.Chown(testFile, int(ownerUID), int(ownerUID))

	// owner UID 應可存取
	cmd1 := exec.Command("cat", testFile)
	cmd1.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: ownerUID, Gid: ownerUID},
	}
	if _, err := cmd1.CombinedOutput(); err != nil {
		log.Printf("[Sandbox] UID test: owner access failed: %v", err)
		return false
	}

	// 其他 UID 應被拒絕
	cmd2 := exec.Command("cat", testFile)
	cmd2.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: otherUID, Gid: otherUID},
	}
	if _, err := cmd2.CombinedOutput(); err == nil {
		log.Printf("[Sandbox] UID test: other UID CAN access — EFS overriding UID, isolation ineffective")
		return false
	}

	log.Printf("[Sandbox] UID test: owner OK, other denied ✓")
	return true
}

// MemberCredentials 根據 memberID 計算專屬 UID/GID（FNV-32 hash → 10000-59999）
func MemberCredentials(memberID string) (uint32, uint32) {
	h := fnv.New32a()
	h.Write([]byte(memberID))
	uid := 10000 + (h.Sum32() % 50000)
	return uid, uid
}

// EnsureVaultPermissions 設定 vault 目錄的 owner 為 member UID，mode 700。
// 每個 vault 目錄只在第一次呼叫時執行 recursive chown；如果 UID + mode 已正確則跳過。
func EnsureVaultPermissions(workDir string, uid, gid uint32) {
	if _, loaded := vaultPermOnce.LoadOrStore(workDir, true); loaded {
		return
	}

	// 快速檢查：目錄已是正確 UID + mode 700 → 跳過 recursive walk
	if info, err := os.Stat(workDir); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if stat.Uid == uid && stat.Gid == gid && info.Mode().Perm() == 0700 {
				return
			}
		}
	}

	start := time.Now()

	os.Chown(workDir, int(uid), int(gid))
	os.Chmod(workDir, 0700)

	count := 0
	filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		os.Chown(path, int(uid), int(gid))
		count++
		return nil
	})

	log.Printf("[Sandbox] vault permissions set: %s → uid=%d, mode=0700, files=%d, elapsed=%dms",
		workDir, uid, count, time.Since(start).Milliseconds())
}

// MemberCredentialsIfAvailable 回傳 member UID/GID（僅在 UID 隔離可用時）
func MemberCredentialsIfAvailable(memberID string) (uid, gid uint32, ok bool) {
	if !uidIsolationAvailable {
		return 0, 0, false
	}
	u, g := MemberCredentials(memberID)
	return u, g, true
}

// ChownToMember 將指定路徑的 owner 設為 member UID（用於 Go 服務寫入 vault 後）
func ChownToMember(fullPath, memberID string) {
	if !uidIsolationAvailable {
		return
	}
	uid, gid := MemberCredentials(memberID)
	os.Chown(fullPath, int(uid), int(gid))
}

// passwdMu 保護 /etc/passwd 寫入
var passwdMu sync.Mutex

// ensurePasswdEntry 為 member UID 建立 /etc/passwd 條目，
// 避免 Node.js os.userInfo() 因找不到 UID 而拋出 ENOENT。
func ensurePasswdEntry(uid, gid uint32) {
	if _, err := user.LookupId(fmt.Sprintf("%d", uid)); err == nil {
		return
	}

	passwdMu.Lock()
	defer passwdMu.Unlock()

	// double-check after lock
	if _, err := user.LookupId(fmt.Sprintf("%d", uid)); err == nil {
		return
	}

	f, err := os.OpenFile("/etc/passwd", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[Sandbox] cannot append /etc/passwd: %v", err)
		return
	}
	defer f.Close()

	entry := fmt.Sprintf("m%d:x:%d:%d::/home/mirror:/bin/bash\n", uid, uid, gid)
	f.WriteString(entry)
}

// fixClaudeHomePerms 確保 /home/mirror 及 .claude/ 下所有檔案可被任意 UID 存取。
// CLI 動態建立的 .claude.json、sessions/、policy-limits.json 預設 mode 600/700，需修正。
func fixClaudeHomePerms() {
	// HOME 目錄需可寫，member UID 才能建立 .claude.json 等檔案
	os.Chmod("/home/mirror", 0777)
	// .claude.json 在 HOME 根目錄（非 .claude/ 內），需單獨修正
	os.Chmod("/home/mirror/.claude.json", 0666)
	filepath.WalkDir("/home/mirror/.claude", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			os.Chmod(path, 0777)
		} else {
			os.Chmod(path, 0666)
		}
		return nil
	})
}

// NewClaudeCmdExported 是 newClaudeCmd 的 exported 版本，供其他 package 使用
func NewClaudeCmdExported(ctx context.Context, workDir, scope, userID string, claudeArgs []string) *exec.Cmd {
	return newClaudeCmd(ctx, workDir, scope, userID, claudeArgs)
}

// newClaudeCmd 建立 claude CLI 指令，以 member 專屬 UID 運行
func newClaudeCmd(ctx context.Context, workDir, scope, userID string, claudeArgs []string) *exec.Cmd {
	var cmd *exec.Cmd
	if ctx != nil {
		cmd = exec.CommandContext(ctx, "claude", claudeArgs...)
	} else {
		cmd = exec.Command("claude", claudeArgs...)
	}
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"TASK_SCOPE="+scope,
		"VAULT_USER_ID="+userID,
	)

	if os.Getuid() == 0 {
		if uidIsolationAvailable {
			uid, gid := MemberCredentials(userID)
			ensurePasswdEntry(uid, gid)
			EnsureVaultPermissions(workDir, uid, gid)
			// 修正 CLI 動態建立的 sessions/、policy-limits.json 等權限
			fixClaudeHomePerms()
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: uid, Gid: gid},
			}
		} else if mirrorUID > 0 {
			// UID 隔離不可用，以 mirror user 運行（Claude CLI 拒絕 root）
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{
					Uid: uint32(mirrorUID),
					Gid: uint32(mirrorGID),
				},
			}
		}
	}

	return cmd
}

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

	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--output-format", "text",
		"-p", instruction,
	}

	cmd := newClaudeCmd(execCtx, workDir, scope, userID, args)

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

	cmd := newClaudeCmd(execCtx, workDir, scope, userID, args)

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
	timerMu   sync.Mutex // 專門保護 idleTimer，避免 stdout goroutine 與 Kill/SendMessage 競爭
	workDir    string
	sessionID  string
	idleTimer  *time.Timer
	idleTTL    time.Duration
	doneCh     chan struct{} // closed when process exits; all readers unblock immediately
	waitErr    error         // set before doneCh is closed
	CacheBuilt bool         // true after first message builds Anthropic prompt cache
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

	cmd := newClaudeCmd(nil, workDir, scope, userID, args)
	if sessionID != "" {
		cmd.Env = append(cmd.Env, "WS_SESSION_ID="+sessionID)
	}

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
		doneCh:    make(chan struct{}),
	}

	go func() {
		s.waitErr = cmd.Wait()
		close(s.doneCh)
	}()

	s.resetIdleTimer()

	log.Printf("[CacheProfile] NewStreamCLI total — %dms, pid=%d, workDir=%s, sessionID=%s, resume=%v, uidIsolation=%v",
		time.Since(funcStart).Milliseconds(), cmd.Process.Pid, workDir, sessionID, resume, uidIsolationAvailable)
	return s, nil
}

// SendMessage 寫入使用者訊息，回傳 stdout 事件 channel（result 事件後關閉）
// content 可以是 string（純文字）或 []map[string]interface{}（多模態 content blocks）
func (s *StreamCLI) SendMessage(content interface{}) (<-chan StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.IsAliveUnlocked() {
		return nil, fmt.Errorf("CLI process not alive")
	}

	s.stopIdleTimer()

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
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	ch := make(chan StreamEvent, 128)
	go func() {
		defer func() {
			s.resetIdleTimer()
			close(ch)
		}()
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
	}()

	return ch, nil
}

// Interrupt sends SIGINT to the CLI process to abort the current response.
// Unlike Kill(), the process stays alive and can accept new messages.
func (s *StreamCLI) Interrupt() bool {
	if !s.IsAlive() || s.cmd.Process == nil {
		return false
	}
	log.Printf("[StreamCLI] interrupting pid=%d (SIGINT)", s.cmd.Process.Pid)
	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		log.Printf("[StreamCLI] SIGINT failed: %v", err)
		return false
	}
	s.resetIdleTimer()
	return true
}

func (s *StreamCLI) Kill() {
	s.timerMu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.timerMu.Unlock()

	if s.cmd.Process != nil {
		log.Printf("[StreamCLI] killing pid=%d", s.cmd.Process.Pid)
		_ = s.cmd.Process.Kill()
		<-s.doneCh
	}
}

func (s *StreamCLI) IsAlive() bool {
	select {
	case <-s.doneCh:
		return false
	default:
		return true
	}
}

// IsAliveUnlocked is the same check, callable when mu is already held.
func (s *StreamCLI) IsAliveUnlocked() bool {
	select {
	case <-s.doneCh:
		return false
	default:
		return true
	}
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

func (s *StreamCLI) stopIdleTimer() {
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *StreamCLI) resetIdleTimer() {
	s.timerMu.Lock()
	defer s.timerMu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(s.idleTTL, func() {
		log.Printf("[StreamCLI] idle timeout, killing pid=%d", s.cmd.Process.Pid)
		s.Kill()
	})
}

