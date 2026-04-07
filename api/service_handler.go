package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ServiceHandler struct {
	vaultRoot     string
	workerBaseURL string
	workerSecret  string

	mu       sync.RWMutex
	registry map[string]*serviceEntry // "memberID:serviceDir" → entry
}

type serviceEntry struct {
	MemberID     string
	ServiceDir   string
	Port         int
	Status       string
	LastEnsured  time.Time
}

type serviceManifest struct {
	Name      string             `json:"name"`
	Version   string             `json:"version"`
	Endpoints []manifestEndpoint `json:"endpoints"`
}

type manifestEndpoint struct {
	Path                string `json:"path"`
	Method              string `json:"method"`
	Public              bool   `json:"public"`
	WebhookSecretHeader string `json:"webhook_secret_header,omitempty"`
	WebhookSecretValue  string `json:"webhook_secret_value,omitempty"`
}

func NewServiceHandler(vaultRoot, serviceWorkerURL, workerSecret string) *ServiceHandler {
	h := &ServiceHandler{
		vaultRoot:     vaultRoot,
		workerBaseURL: strings.TrimRight(serviceWorkerURL, "/"),
		workerSecret:  workerSecret,
		registry:      make(map[string]*serviceEntry),
	}
	go h.registryCleaner()
	return h
}

func (h *ServiceHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/svc/", h.HandleSvcRequest)
}

func (h *ServiceHandler) HandleSvcRequest(w http.ResponseWriter, r *http.Request) {
	// CORS for Electron desktop (localhost:3000) and web
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-User-ID")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/svc/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		http.Error(w, "invalid service path", http.StatusBadRequest)
		return
	}
	memberID := filepath.Base(parts[0])
	serviceDir := filepath.Base(parts[1])
	endpoint := "/"
	if len(parts) == 3 {
		endpoint = "/" + parts[2]
	}

	manifest, err := h.loadManifest(memberID, serviceDir)
	if err != nil {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}

	ep := findEndpoint(manifest, endpoint, r.Method)

	if ep != nil && ep.Public {
		if ep.WebhookSecretHeader == "" || ep.WebhookSecretValue == "" {
			http.Error(w, "service misconfigured: public endpoint requires webhook secret", http.StatusForbidden)
			return
		}
		actual := r.Header.Get(ep.WebhookSecretHeader)
		if subtle.ConstantTimeCompare([]byte(actual), []byte(ep.WebhookSecretValue)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else {
		userID := r.Header.Get("X-User-ID")
		if userID == "" || userID != memberID {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	err = h.ensureService(r.Context(), memberID, serviceDir)
	if err != nil {
		log.Printf("[ServiceHandler] ensure failed: %s/%s: %v", memberID, serviceDir, err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	h.proxyToWorker(w, r, memberID, serviceDir, endpoint)
}

func (h *ServiceHandler) loadManifest(memberID, serviceDir string) (*serviceManifest, error) {
	manifestPath := filepath.Join(h.vaultRoot, memberID, "plugins", serviceDir, "backend", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var m serviceManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func findEndpoint(m *serviceManifest, path, method string) *manifestEndpoint {
	for i := range m.Endpoints {
		ep := &m.Endpoints[i]
		if matchesMethod(ep.Method, method) && ep.Path == path {
			return ep
		}
	}
	for i := range m.Endpoints {
		ep := &m.Endpoints[i]
		if matchesMethod(ep.Method, method) && strings.HasPrefix(path, ep.Path+"/") {
			return ep
		}
	}
	return nil
}

func matchesMethod(epMethod, reqMethod string) bool {
	if epMethod == "" {
		return true
	}
	return strings.EqualFold(epMethod, reqMethod)
}

func (h *ServiceHandler) ensureService(ctx context.Context, memberID, serviceDir string) error {
	key := memberID + ":" + serviceDir

	h.mu.RLock()
	entry, exists := h.registry[key]
	h.mu.RUnlock()

	if exists && entry.Status == "running" && time.Since(entry.LastEnsured) < 4*time.Minute {
		return nil
	}

	reqBody, _ := json.Marshal(map[string]string{
		"memberID":   memberID,
		"serviceDir": serviceDir,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", h.workerBaseURL+"/internal/service/ensure", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.workerSecret != "" {
		req.Header.Set("X-Internal-Secret", h.workerSecret)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("worker ensure → %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Status string `json:"status"`
		Port   int    `json:"port"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	if result.Status == "starting" {
		if err := h.waitForServiceReady(ctx, memberID, serviceDir, 45*time.Second); err != nil {
			return err
		}
	}

	h.mu.Lock()
	h.registry[key] = &serviceEntry{
		MemberID:    memberID,
		ServiceDir:  serviceDir,
		Port:        result.Port,
		Status:      "running",
		LastEnsured: time.Now(),
	}
	h.mu.Unlock()

	return nil
}

func (h *ServiceHandler) waitForServiceReady(ctx context.Context, memberID, serviceDir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		reqBody, _ := json.Marshal(map[string]string{
			"memberID":   memberID,
			"serviceDir": serviceDir,
		})
		req, err := http.NewRequestWithContext(ctx, "POST", h.workerBaseURL+"/internal/service/ensure", bytes.NewReader(reqBody))
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if h.workerSecret != "" {
			req.Header.Set("X-Internal-Secret", h.workerSecret)
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}

		var result struct {
			Status string `json:"status"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		if result.Status == "running" {
			return nil
		}
		if result.Status == "error" {
			return fmt.Errorf("service failed to start")
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("service start timeout")
}

func (h *ServiceHandler) proxyToWorker(w http.ResponseWriter, r *http.Request, memberID, serviceDir, endpoint string) {
	targetBase, err := url.Parse(h.workerBaseURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetBase)
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		log.Printf("[ServiceHandler] proxy error: %s/%s: %v", memberID, serviceDir, err)
		key := memberID + ":" + serviceDir
		h.mu.Lock()
		delete(h.registry, key)
		h.mu.Unlock()
		http.Error(rw, "bad gateway", http.StatusBadGateway)
	}

	originalQuery := r.URL.RawQuery
	r.URL.Path = fmt.Sprintf("/svc-proxy/%s/%s%s", memberID, serviceDir, endpoint)
	r.URL.RawQuery = originalQuery
	r.Host = targetBase.Host
	if h.workerSecret != "" {
		r.Header.Set("X-Internal-Secret", h.workerSecret)
	}
	proxy.ServeHTTP(w, r)
}

// StopAndRemove 呼叫 plugin_runtime_server 的 stop API 停掉後端 process，並從 registry 移除
func (h *ServiceHandler) StopAndRemove(memberID, serviceDir string) {
	key := memberID + ":" + serviceDir
	h.mu.Lock()
	delete(h.registry, key)
	h.mu.Unlock()
	log.Printf("[ServiceHandler] removed from registry: %s", key)

	reqBody, _ := json.Marshal(map[string]string{
		"memberID":   memberID,
		"serviceDir": serviceDir,
	})
	req, err := http.NewRequest("POST", h.workerBaseURL+"/internal/service/stop", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[ServiceHandler] stop request build failed: %s: %v", key, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if h.workerSecret != "" {
		req.Header.Set("X-Internal-Secret", h.workerSecret)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[ServiceHandler] stop request failed: %s: %v", key, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	log.Printf("[ServiceHandler] stop response: %s: %d %s", key, resp.StatusCode, string(body))
}

func (h *ServiceHandler) registryCleaner() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		for key, entry := range h.registry {
			if time.Since(entry.LastEnsured) > 6*time.Minute {
				delete(h.registry, key)
			}
		}
		h.mu.Unlock()
	}
}
