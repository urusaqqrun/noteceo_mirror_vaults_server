package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
	vaultsync "github.com/urusaqqrun/vault-mirror-service/sync"
)

// PluginHandler serves plugin bundle downloads.
type PluginHandler struct {
	vaultFS        mirror.VaultFS
	itemStore      PluginItemStore
	locker         vaultsync.VaultLocker
	projector      *vaultsync.SyncEventHandler
	serviceHandler *ServiceHandler
	gitHandler     *PluginGitHandler
}

type PluginItemStore interface {
	GetItem(ctx context.Context, userID, itemID string) (*model.Item, error)
	IncrementVersion(ctx context.Context, userID string) (int, error)
	DeleteItemDoc(ctx context.Context, userID, docID string, version int) error
}

func NewPluginHandler(
	vaultFS mirror.VaultFS,
	itemStore PluginItemStore,
	locker vaultsync.VaultLocker,
	projector *vaultsync.SyncEventHandler,
	serviceHandler *ServiceHandler,
	gitHandler *PluginGitHandler,
) *PluginHandler {
	return &PluginHandler{
		vaultFS:        vaultFS,
		itemStore:      itemStore,
		locker:         locker,
		projector:      projector,
		serviceHandler: serviceHandler,
		gitHandler:     gitHandler,
	}
}

func (h *PluginHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/vault/plugin", h.HandleDelete)
	mux.HandleFunc("/api/vault/plugin/bundle", h.HandleBundleDownload)
	mux.HandleFunc("/api/vault/plugin/css", h.HandleCSSDownload)
}

type deletePluginRequest struct {
	ItemID    string `json:"itemID"`
	PluginDir string `json:"pluginDir"`
}

func normalizePluginDir(pluginDir string) string {
	pluginDir = filepath.Base(pluginDir)
	if pluginDir == "." || pluginDir == ".." || strings.Contains(pluginDir, "/") {
		return ""
	}
	return pluginDir
}

// HandleDelete deletes a plugin item and its vault files synchronously.
// DELETE /api/vault/plugin
func (h *PluginHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	memberID, ok := memberIDFromHeader(w, r)
	if !ok {
		return
	}
	if h.itemStore == nil {
		chatWriteError(w, http.StatusInternalServerError, "plugin delete store unavailable")
		return
	}
	if h.locker != nil && h.locker.IsLocked(memberID) {
		chatWriteError(w, http.StatusConflict, vaultsync.ErrVaultLocked.Error())
		return
	}

	var req deletePluginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		chatWriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ItemID == "" {
		chatWriteError(w, http.StatusBadRequest, "itemID is required")
		return
	}
	log.Printf("[PluginHandler] DELETE start: member=%s itemID=%s pluginDir=%s", memberID, req.ItemID, req.PluginDir)

	log.Printf("[PluginHandler] GetItem calling: member=%s itemID=%q", memberID, req.ItemID)
	item, err := h.itemStore.GetItem(r.Context(), memberID, req.ItemID)
	log.Printf("[PluginHandler] GetItem returned: member=%s itemID=%q item_nil=%v err=%v", memberID, req.ItemID, item == nil, err)
	if err != nil {
		chatWriteError(w, http.StatusInternalServerError, "failed to load plugin item: "+err.Error())
		return
	}

	pluginDir := normalizePluginDir(req.PluginDir)
	if item != nil {
		if item.Type != "PLUGIN" {
			chatWriteError(w, http.StatusBadRequest, "item is not a plugin")
			return
		}
		if pd, _ := item.Fields["pluginDir"].(string); pd != "" {
			pluginDir = normalizePluginDir(pd)
		}
	}
	if pluginDir == "" {
		chatWriteError(w, http.StatusBadRequest, "pluginDir is required")
		return
	}

	version, err := h.itemStore.IncrementVersion(r.Context(), memberID)
	if err != nil {
		log.Printf("[PluginHandler] increment version failed: member=%s err=%v", memberID, err)
		chatWriteError(w, http.StatusInternalServerError, "failed to allocate version")
		return
	}

	if item != nil {
		if err := h.itemStore.DeleteItemDoc(r.Context(), memberID, req.ItemID, version); err != nil {
			log.Printf("[PluginHandler] delete plugin item failed: member=%s item=%s err=%v", memberID, req.ItemID, err)
			chatWriteError(w, http.StatusInternalServerError, "failed to delete plugin item")
			return
		}
		log.Printf("[PluginHandler] deleted item in PG: member=%s item=%s version=%d", memberID, req.ItemID, version)
	} else {
		log.Printf("[PluginHandler] item not found in PG (already deleted?): member=%s item=%s", memberID, req.ItemID)
	}

	pluginJSONPath := filepath.Join(memberID, "PLUGIN", pluginDir+".json")
	pluginRoot := filepath.Join(memberID, "plugins", pluginDir)

	if h.vaultFS.Exists(pluginJSONPath) {
		if err := h.vaultFS.Remove(pluginJSONPath); err != nil {
			log.Printf("[PluginHandler] delete plugin json failed: path=%s err=%v", pluginJSONPath, err)
			chatWriteError(w, http.StatusInternalServerError, "failed to delete plugin json")
			return
		}
		log.Printf("[PluginHandler] removed json: %s", pluginJSONPath)
	}
	if h.vaultFS.Exists(pluginRoot) {
		if err := h.vaultFS.RemoveAll(pluginRoot); err != nil {
			log.Printf("[PluginHandler] delete plugin dir failed: path=%s err=%v", pluginRoot, err)
			chatWriteError(w, http.StatusInternalServerError, "failed to delete plugin directory")
			return
		}
		log.Printf("[PluginHandler] removed dir: %s", pluginRoot)
	}

	if h.serviceHandler != nil {
		h.serviceHandler.RemoveFromRegistry(memberID, pluginDir)
	}

	if h.projector != nil {
		h.projector.InvalidateResolver(memberID)
		h.projector.InvalidateDocPathIndex(memberID)
		_ = h.projector.HandleEvent(r.Context(), vaultsync.SyncEvent{
			Collection: "item",
			UserID:     memberID,
			DocID:      req.ItemID,
			Action:     "delete",
			Timestamp:  time.Now().UnixMilli(),
			Version:    version,
		})
	}

	// Git commit 刪除結果
	if h.gitHandler != nil {
		if hash, err := h.gitHandler.Commit(memberID, "delete: "+pluginDir, "system"); err != nil {
			log.Printf("[PluginHandler] git commit failed: %v", err)
		} else if hash != "" {
			log.Printf("[PluginHandler] git commit: %s", hash)
		}
	}

	log.Printf("[PluginHandler] DELETE done: member=%s itemID=%s pluginDir=%s version=%d", memberID, req.ItemID, pluginDir, version)
	chatWriteJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"itemID":    req.ItemID,
		"pluginDir": pluginDir,
	})
}

// HandleBundleDownload serves a plugin's bundle.js file.
// GET /api/vault/plugin/bundle?memberID=xxx&plugin=pomodoro
func (h *PluginHandler) HandleBundleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	memberID := r.URL.Query().Get("memberID")
	pluginDir := r.URL.Query().Get("plugin")

	if memberID == "" || pluginDir == "" {
		http.Error(w, "missing memberID or plugin parameter", http.StatusBadRequest)
		return
	}

	// Sanitize plugin dir name
	pluginDir = filepath.Base(pluginDir)
	if pluginDir == "." || pluginDir == ".." || strings.Contains(pluginDir, "/") {
		http.Error(w, "invalid plugin name", http.StatusBadRequest)
		return
	}

	bundlePath := filepath.Join(memberID, "plugins", pluginDir, "frontend", "bundle.js")
	data, err := h.vaultFS.ReadFile(bundlePath)
	if err != nil {
		log.Printf("[PluginHandler] bundle not found: member=%s plugin=%s err=%v", memberID, pluginDir, err)
		http.Error(w, "bundle not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// HandleCSSDownload serves a plugin's bundle.css file (if exists).
// GET /api/vault/plugin/css?memberID=xxx&plugin=pomodoro
func (h *PluginHandler) HandleCSSDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	memberID := r.URL.Query().Get("memberID")
	pluginDir := r.URL.Query().Get("plugin")

	if memberID == "" || pluginDir == "" {
		http.Error(w, "missing memberID or plugin parameter", http.StatusBadRequest)
		return
	}

	pluginDir = filepath.Base(pluginDir)
	if pluginDir == "." || pluginDir == ".." || strings.Contains(pluginDir, "/") {
		http.Error(w, "invalid plugin name", http.StatusBadRequest)
		return
	}

	cssPath := filepath.Join(memberID, "plugins", pluginDir, "frontend", "bundle.css")
	data, err := h.vaultFS.ReadFile(cssPath)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

