package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

// PluginGitHandler manages Git repositories in users' plugins/ directories.
type PluginGitHandler struct {
	vaultRoot string
	locker    vaultsync.VaultLocker
	rebuilder PluginRebuilder
}

func NewPluginGitHandler(vaultRoot string, locker vaultsync.VaultLocker, rebuilder PluginRebuilder) *PluginGitHandler {
	return &PluginGitHandler{vaultRoot: vaultRoot, locker: locker, rebuilder: rebuilder}
}

func (h *PluginGitHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/vault/plugins/git/head", h.HandleHead)
	mux.HandleFunc("GET /api/vault/plugins/git/status", h.HandleStatus)
	mux.HandleFunc("GET /api/vault/plugins/git/archive", h.HandleArchive)
	mux.HandleFunc("POST /api/vault/plugins/git/push", h.HandlePush)
	mux.HandleFunc("GET /api/vault/plugins/git/log", h.HandleLog)
}

func (h *PluginGitHandler) pluginsDir(userID string) string {
	return filepath.Join(h.vaultRoot, userID, "plugins")
}

// ensureRepo initializes a Git repo in plugins/ if it doesn't exist (idempotent).
func (h *PluginGitHandler) ensureRepo(userID string) error {
	dir := h.pluginsDir(userID)
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := gitExec(dir, "init"); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	gitignore := "*/frontend/node_modules/\n*/backend/node_modules/\n*/frontend/bundle.js\n*/frontend/bundle.css\n*/backend/dist/\n.DS_Store\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0644); err != nil {
		return err
	}
	if err := gitExec(dir, "add", "-A"); err != nil {
		return err
	}
	if err := gitExec(dir, "commit", "--allow-empty", "-m", "init: plugins repository"); err != nil {
		return err
	}
	log.Printf("[PluginGit] initialized repo for %s", userID)
	return nil
}

// commit stages all changes and commits. Returns commit hash or empty string if nothing to commit.
func (h *PluginGitHandler) commit(userID, message, author string) (string, error) {
	dir := h.pluginsDir(userID)
	if err := h.ensureRepo(userID); err != nil {
		return "", err
	}
	if err := gitExec(dir, "add", "-A"); err != nil {
		return "", err
	}
	status, _ := gitOutput(dir, "status", "--porcelain")
	if strings.TrimSpace(status) == "" {
		return "", nil // nothing to commit
	}
	authorArg := fmt.Sprintf("%s <noreply@cubelv.com>", author)
	if err := gitExec(dir, "commit", "-m", message, "--author", authorArg); err != nil {
		return "", err
	}
	hash, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(hash), nil
}

func (h *PluginGitHandler) headCommit(userID string) (string, error) {
	dir := h.pluginsDir(userID)
	if err := h.ensureRepo(userID); err != nil {
		return "", err
	}
	hash, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(hash), nil
}

// --- HTTP Handlers ---

func (h *PluginGitHandler) HandleHead(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	hash, err := h.headCommit(memberID)
	if err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}
	chatWriteJSON(w, 200, map[string]string{"commit": hash})
}

func (h *PluginGitHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	since := r.URL.Query().Get("since")
	dir := h.pluginsDir(memberID)

	if err := h.ensureRepo(memberID); err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}

	head, _ := gitOutput(dir, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)

	var files []map[string]string
	if since == "" || since == "0" {
		// 全量：列出所有 tracked files
		output, _ := gitOutput(dir, "ls-files")
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line != "" {
				files = append(files, map[string]string{"path": line, "action": "A"})
			}
		}
	} else {
		// 差量：since..HEAD
		output, err := gitOutput(dir, "diff", "--name-status", since+"..HEAD")
		if err != nil {
			// since commit might not exist (first sync)
			output, _ = gitOutput(dir, "ls-files")
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				if line != "" {
					files = append(files, map[string]string{"path": line, "action": "A"})
				}
			}
		} else {
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				if line == "" {
					continue
				}
				parts := strings.SplitN(line, "\t", 2)
				if len(parts) == 2 {
					files = append(files, map[string]string{"action": parts[0], "path": parts[1]})
				}
			}
		}
	}

	if files == nil {
		files = []map[string]string{}
	}

	chatWriteJSON(w, 200, map[string]interface{}{
		"headCommit": head,
		"files":      files,
	})
}

func (h *PluginGitHandler) HandleArchive(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	since := r.URL.Query().Get("since")
	dir := h.pluginsDir(memberID)

	if err := h.ensureRepo(memberID); err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}

	// Get list of files to archive
	var filePaths []string
	if since == "" || since == "0" {
		output, _ := gitOutput(dir, "ls-files")
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line != "" {
				filePaths = append(filePaths, line)
			}
		}
	} else {
		output, _ := gitOutput(dir, "diff", "--name-only", since+"..HEAD")
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if line != "" {
				filePaths = append(filePaths, line)
			}
		}
	}

	// Create tar.gz
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for _, fp := range filePaths {
		fullPath := filepath.Join(dir, fp)
		info, err := os.Stat(fullPath)
		if err != nil {
			continue // file might have been deleted
		}
		header, _ := tar.FileInfoHeader(info, "")
		header.Name = fp
		tw.WriteHeader(header)
		f, err := os.Open(fullPath)
		if err != nil {
			continue
		}
		io.Copy(tw, f)
		f.Close()
	}

	tw.Close()
	gw.Close()

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=plugins.tar.gz")
	w.Write(buf.Bytes())
}

func (h *PluginGitHandler) HandlePush(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}

	if h.locker != nil && h.locker.IsLocked(memberID) {
		chatWriteError(w, 409, "vault locked")
		return
	}

	var req struct {
		BaseCommit string `json:"baseCommit"`
		Message    string `json:"message"`
		Files      []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Action  string `json:"action"` // add, modify, delete
		} `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, 400, "invalid body")
		return
	}

	dir := h.pluginsDir(memberID)
	if err := h.ensureRepo(memberID); err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}

	// Check if baseCommit is ancestor of HEAD
	head, _ := gitOutput(dir, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	if req.BaseCommit != "" && req.BaseCommit != head {
		// Check ancestry
		err := gitExec(dir, "merge-base", "--is-ancestor", req.BaseCommit, "HEAD")
		if err != nil {
			chatWriteJSON(w, 409, map[string]interface{}{
				"conflict":   true,
				"headCommit": head,
			})
			return
		}
	}

	// Apply file changes
	for _, f := range req.Files {
		fullPath := filepath.Join(dir, f.Path)
		if f.Action == "delete" {
			os.Remove(fullPath)
		} else {
			os.MkdirAll(filepath.Dir(fullPath), 0755)
			os.WriteFile(fullPath, []byte(f.Content), 0644)
		}
	}

	msg := req.Message
	if msg == "" {
		msg = "client push"
	}
	commitHash, err := h.commit(memberID, msg, "client-sync")
	if err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}

	// Trigger rebuild for changed plugins
	if h.rebuilder != nil {
		changedDirs := map[string]bool{}
		for _, f := range req.Files {
			parts := strings.SplitN(f.Path, "/", 2)
			if len(parts) >= 1 {
				changedDirs[parts[0]] = true
			}
		}
		for pluginDir := range changedDirs {
			go func(pd string) {
				if _, err := h.rebuilder.Rebuild(r.Context(), RebuildReq{MemberID: memberID, PluginDir: pd}); err != nil {
					log.Printf("[PluginGit] rebuild failed: %s/%s: %v", memberID, pd, err)
				}
			}(pluginDir)
		}
	}

	chatWriteJSON(w, 200, map[string]string{"commitHash": commitHash})
}

func (h *PluginGitHandler) HandleLog(w http.ResponseWriter, r *http.Request) {
	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	dir := h.pluginsDir(memberID)
	if err := h.ensureRepo(memberID); err != nil {
		chatWriteError(w, 500, err.Error())
		return
	}

	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "20"
	}

	output, err := gitOutput(dir, "log", "--format=%H|%s|%an|%aI", "-"+limit)
	if err != nil {
		chatWriteJSON(w, 200, []interface{}{})
		return
	}

	var entries []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) == 4 {
			entries = append(entries, map[string]string{
				"hash":    parts[0],
				"message": parts[1],
				"author":  parts[2],
				"date":    parts[3],
			})
		}
	}
	if entries == nil {
		entries = []map[string]string{}
	}
	chatWriteJSON(w, 200, entries)
}

// Commit is a public method for other handlers to use (plugin delete, etc.)
func (h *PluginGitHandler) Commit(userID, message, author string) (string, error) {
	return h.commit(userID, message, author)
}

// --- Git helpers ---

func gitExec(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=CubeLV", "GIT_AUTHOR_EMAIL=noreply@cubelv.com",
		"GIT_COMMITTER_NAME=CubeLV", "GIT_COMMITTER_EMAIL=noreply@cubelv.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=CubeLV", "GIT_AUTHOR_EMAIL=noreply@cubelv.com",
		"GIT_COMMITTER_NAME=CubeLV", "GIT_COMMITTER_EMAIL=noreply@cubelv.com")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
