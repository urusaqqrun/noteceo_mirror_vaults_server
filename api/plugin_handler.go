package api

import (
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// PluginHandler serves plugin bundle downloads.
type PluginHandler struct {
	vaultFS mirror.VaultFS
}

func NewPluginHandler(vaultFS mirror.VaultFS) *PluginHandler {
	return &PluginHandler{vaultFS: vaultFS}
}

func (h *PluginHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/vault/plugin/bundle", h.HandleBundleDownload)
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

	bundlePath := filepath.Join(memberID, "plugins", pluginDir, "bundle.js")
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
