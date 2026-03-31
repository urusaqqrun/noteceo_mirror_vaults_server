package api

import (
	"encoding/json"
	"io/fs"
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
	mux.HandleFunc("/api/vault/plugin/css", h.HandleCSSDownload)
	mux.HandleFunc("/api/vault/plugin/source", h.HandleSourceDownload)
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

	cssPath := filepath.Join(memberID, "plugins", pluginDir, "bundle.css")
	data, err := h.vaultFS.ReadFile(cssPath)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// HandleSourceDownload returns all source files of a plugin (excluding bundle.js and node_modules).
// GET /api/vault/plugin/source?memberID=xxx&plugin=pomodoro
func (h *PluginHandler) HandleSourceDownload(w http.ResponseWriter, r *http.Request) {
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

	pluginRoot := filepath.Join(memberID, "plugins", pluginDir)
	if !h.vaultFS.Exists(pluginRoot) {
		http.Error(w, "plugin not found", http.StatusNotFound)
		return
	}

	files := make(map[string]string)

	_ = h.vaultFS.Walk(pluginRoot, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(pluginRoot, path)
		if relErr != nil {
			return nil
		}
		if rel == "bundle.js" || strings.HasPrefix(rel, "node_modules"+string(filepath.Separator)) || strings.HasPrefix(rel, ".") {
			return nil
		}
		data, readErr := h.vaultFS.ReadFile(path)
		if readErr != nil {
			log.Printf("[PluginHandler] skip unreadable file: %s err=%v", path, readErr)
			return nil
		}
		files[rel] = string(data)
		return nil
	})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pluginDir": pluginDir,
		"files":     files,
	})
}
