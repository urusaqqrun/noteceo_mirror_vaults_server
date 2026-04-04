package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// WsBroadcaster 用於發送 WS 事件給指定用戶的所有 session
type WsBroadcaster interface {
	BroadcastToMember(memberID string, msg map[string]interface{})
}

// PluginRebuilder 能觸發插件重新編譯
type PluginRebuilder interface {
	Rebuild(ctx context.Context, req RebuildReq) (*RebuildResp, error)
}

// VaultSyncHandler 提供 vault 檔案級同步 API
type VaultSyncHandler struct {
	vaultFS     mirror.VaultFS
	vaultRoot   string
	broadcaster WsBroadcaster
	rebuilder   PluginRebuilder
}

func NewVaultSyncHandler(vaultFS mirror.VaultFS, vaultRoot string, broadcaster WsBroadcaster, rebuilder PluginRebuilder) *VaultSyncHandler {
	return &VaultSyncHandler{vaultFS: vaultFS, vaultRoot: vaultRoot, broadcaster: broadcaster, rebuilder: rebuilder}
}

func (h *VaultSyncHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/vault/snapshot", h.HandleSnapshot)
	mux.HandleFunc("GET /api/vault/file", h.HandleFileDownload)
	mux.HandleFunc("PUT /api/vault/file", h.HandleFileUpload)
	mux.HandleFunc("DELETE /api/vault/file", h.HandleFileDelete)
}

// FileEntry 單一檔案的 snapshot 資訊
type FileEntry struct {
	Hash  string `json:"hash"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

// HandleSnapshot 回傳用戶 vault 的完整檔案清單 + hash
func (h *VaultSyncHandler) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, "unauthorized", 401)
		return
	}

	userRoot := filepath.Join(memberID)
	files := make(map[string]FileEntry)

	h.vaultFS.Walk(userRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// 排除不同步的目錄
			if name == "node_modules" || name == ".sync" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		// 取相對路徑（相對於 memberID）
		relPath, relErr := filepath.Rel(userRoot, path)
		if relErr != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		// plugins/ 由 Git 管理，不在 snapshot 中
		if strings.HasPrefix(relPath, "plugins/") {
			return nil
		}

		// 算 hash
		content, readErr := h.vaultFS.ReadFile(path)
		if readErr != nil {
			return nil
		}
		hashSum := sha256.Sum256(content)

		files[relPath] = FileEntry{
			Hash:  fmt.Sprintf("%x", hashSum),
			Size:  info.Size(),
			Mtime: info.ModTime().UnixMilli(),
		}

		return nil
	})

	log.Printf("[VaultSync] snapshot for %s: %d files", memberID, len(files))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"files": files,
	})
}

// HandleFileDownload 下載單一檔案
func (h *VaultSyncHandler) HandleFileDownload(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, "unauthorized", 401)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "path required", 400)
		return
	}

	// 安全檢查：不能用 .. 逃逸
	if strings.Contains(filePath, "..") {
		http.Error(w, "invalid path", 400)
		return
	}

	fullPath := filepath.Join(memberID, filePath)
	content, err := h.vaultFS.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "file not found", 404)
		return
	}

	// 設定 Content-Type
	ext := filepath.Ext(filePath)
	switch ext {
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	case ".tsx", ".ts", ".js", ".mjs":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.Write(content)
}

// HandleFileUpload 上傳/覆蓋單一檔案（atomic write: temp + rename）
func (h *VaultSyncHandler) HandleFileUpload(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, "unauthorized", 401)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "path required", 400)
		return
	}

	if strings.Contains(filePath, "..") {
		http.Error(w, "invalid path", 400)
		return
	}

	// plugins/ 走 Git API，不走 vault file API
	if strings.HasPrefix(filePath, "plugins/") {
		http.Error(w, "plugins/ files must use /api/vault/plugins/git/push", 400)
		return
	}

	// 讀取 body
	content, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024)) // max 10MB
	if err != nil {
		http.Error(w, "read body failed", 400)
		return
	}

	fullPath := filepath.Join(memberID, filePath)
	dir := filepath.Dir(fullPath)

	// 確保目錄存在
	h.vaultFS.MkdirAll(dir)

	// Atomic write: 寫 temp 檔再 rename
	tmpPath := fullPath + ".tmp." + fmt.Sprintf("%d", time.Now().UnixNano())
	if writeErr := h.vaultFS.WriteFile(tmpPath, content); writeErr != nil {
		http.Error(w, "write failed: "+writeErr.Error(), 500)
		return
	}
	if renameErr := h.vaultFS.Rename(tmpPath, fullPath); renameErr != nil {
		// rename 失敗，清理 temp
		h.vaultFS.Remove(tmpPath)
		http.Error(w, "rename failed: "+renameErr.Error(), 500)
		return
	}

	// 算 hash
	hashSum := sha256.Sum256(content)
	hash := fmt.Sprintf("%x", hashSum)

	log.Printf("[VaultSync] file uploaded: %s/%s (%d bytes, hash=%s)", memberID, filePath, len(content), hash[:16])

	// 通知該用戶的其他裝置
	if h.broadcaster != nil {
		h.broadcaster.BroadcastToMember(memberID, map[string]interface{}{
			"type":   "vault_files_changed",
			"files":  []string{filePath},
			"hashes": []string{hash},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"path":   filePath,
		"hash":   hash,
		"size":   len(content),
	})
}

// HandleFileDelete 刪除單一檔案
func (h *VaultSyncHandler) HandleFileDelete(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, "unauthorized", 401)
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "path required", 400)
		return
	}

	if strings.Contains(filePath, "..") {
		http.Error(w, "invalid path", 400)
		return
	}

	// plugins/ 走 Git API
	if strings.HasPrefix(filePath, "plugins/") {
		http.Error(w, "plugins/ files must use /api/vault/plugins/git/push", 400)
		return
	}

	fullPath := filepath.Join(memberID, filePath)

	if !h.vaultFS.Exists(fullPath) {
		http.Error(w, "file not found", 404)
		return
	}

	if err := h.vaultFS.Remove(fullPath); err != nil {
		http.Error(w, "delete failed: "+err.Error(), 500)
		return
	}

	log.Printf("[VaultSync] file deleted: %s/%s", memberID, filePath)

	// 通知該用戶的其他裝置
	if h.broadcaster != nil {
		h.broadcaster.BroadcastToMember(memberID, map[string]interface{}{
			"type":   "vault_files_changed",
			"files":  []string{filePath},
			"hashes": []string{"deleted"},
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"path":   filePath,
	})
}

// isPluginSource and extractPluginDir removed — plugins now use Git API
