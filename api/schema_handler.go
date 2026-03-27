package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// SchemaHandler handles item schema sync from frontend decorators.
type SchemaHandler struct {
	fs mirror.VaultFS
}

func NewSchemaHandler(fs mirror.VaultFS) *SchemaHandler {
	return &SchemaHandler{fs: fs}
}

func (h *SchemaHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/vault/schemas", h.SyncSchemas)
}

// SyncSchemas receives item type schemas from the frontend @itemType decorator
// and writes them to .schemas/ in the user's vault directory.
func (h *SchemaHandler) SyncSchemas(w http.ResponseWriter, r *http.Request) {
	// Extract memberID: X-User-ID (nginx 注入) → JWT payload → query param
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		memberID = extractMemberIDFromAuth(r.Header.Get("Authorization"))
	}
	if memberID == "" {
		memberID = r.URL.Query().Get("memberID")
	}
	if memberID == "" {
		http.Error(w, `{"error":"missing memberID"}`, 401)
		return
	}

	var req struct {
		Schemas map[string]interface{} `json:"schemas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}

	if len(req.Schemas) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"count":0}`))
		return
	}

	// Write each schema as a separate .json file
	schemasDir := filepath.Join(memberID, ".schemas")
	h.fs.MkdirAll(schemasDir)

	for typeName, schema := range req.Schemas {
		data, err := json.MarshalIndent(schema, "", "  ")
		if err != nil {
			continue
		}
		path := filepath.Join(schemasDir, typeName+".json")
		if err := h.fs.WriteFile(path, data); err != nil {
			log.Printf("[SchemaHandler] write %s error: %v", path, err)
		}
	}

	// Update user-level CLAUDE.md with aiHints from schemas
	updateUserClaudeMD(h.fs, memberID, req.Schemas)

	log.Printf("[SchemaHandler] synced %d schemas for %s", len(req.Schemas), memberID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(req.Schemas),
	})
}

// updateUserClaudeMD updates the user-level CLAUDE.md in the vault with aiHints
// extracted from the pushed schemas. It replaces content between AIHINTS markers,
// or creates the file with markers if it doesn't exist.
func updateUserClaudeMD(vaultFS mirror.VaultFS, memberID string, schemas map[string]interface{}) {
	// Build aiHints text from all schemas that have aiHints
	var hintsBuilder strings.Builder
	for typeName, schema := range schemas {
		schemaMap, ok := schema.(map[string]interface{})
		if !ok {
			continue
		}
		aiHints, ok := schemaMap["aiHints"]
		if !ok {
			continue
		}
		// aiHints can be a string or []interface{}
		switch v := aiHints.(type) {
		case string:
			if v != "" {
				hintsBuilder.WriteString(fmt.Sprintf("### %s\n%s\n\n", typeName, v))
			}
		case []interface{}:
			if len(v) > 0 {
				hintsBuilder.WriteString(fmt.Sprintf("### %s\n", typeName))
				for _, hint := range v {
					if s, ok := hint.(string); ok {
						hintsBuilder.WriteString(fmt.Sprintf("- %s\n", s))
					}
				}
				hintsBuilder.WriteString("\n")
			}
		}
	}

	hintsContent := hintsBuilder.String()

	// Build the aiHints block with markers
	aiHintsBlock := "<!-- AIHINTS:START -->\n"
	if hintsContent != "" {
		aiHintsBlock += "## 啟用的插件 aiHints\n\n" + hintsContent
	}
	aiHintsBlock += "<!-- AIHINTS:END -->"

	claudeMDPath := filepath.Join(memberID, "CLAUDE.md")

	// Try to read existing file
	existing, err := vaultFS.ReadFile(claudeMDPath)
	if err != nil {
		// File doesn't exist — create new with markers
		newContent := fmt.Sprintf("# 用戶個人化設定\n\n%s\n\n<!-- AI_MEMORY:START -->\n<!-- AI_MEMORY:END -->\n", aiHintsBlock)
		if writeErr := vaultFS.WriteFile(claudeMDPath, []byte(newContent)); writeErr != nil {
			log.Printf("[SchemaHandler] write user CLAUDE.md error: %v", writeErr)
		}
		return
	}

	content := string(existing)

	// Replace content between AIHINTS markers
	startMarker := "<!-- AIHINTS:START -->"
	endMarker := "<!-- AIHINTS:END -->"
	startIdx := strings.Index(content, startMarker)
	endIdx := strings.Index(content, endMarker)

	if startIdx >= 0 && endIdx >= 0 && endIdx > startIdx {
		// Replace the block between markers (inclusive)
		newContent := content[:startIdx] + aiHintsBlock + content[endIdx+len(endMarker):]
		if writeErr := vaultFS.WriteFile(claudeMDPath, []byte(newContent)); writeErr != nil {
			log.Printf("[SchemaHandler] update user CLAUDE.md error: %v", writeErr)
		}
	} else {
		// Markers don't exist — append before AI_MEMORY or at end
		memoryMarker := "<!-- AI_MEMORY:START -->"
		memIdx := strings.Index(content, memoryMarker)
		if memIdx >= 0 {
			newContent := content[:memIdx] + aiHintsBlock + "\n\n" + content[memIdx:]
			if writeErr := vaultFS.WriteFile(claudeMDPath, []byte(newContent)); writeErr != nil {
				log.Printf("[SchemaHandler] update user CLAUDE.md error: %v", writeErr)
			}
		} else {
			// No markers at all — append aiHints block and memory markers at end
			newContent := content + "\n\n" + aiHintsBlock + "\n\n<!-- AI_MEMORY:START -->\n<!-- AI_MEMORY:END -->\n"
			if writeErr := vaultFS.WriteFile(claudeMDPath, []byte(newContent)); writeErr != nil {
				log.Printf("[SchemaHandler] update user CLAUDE.md error: %v", writeErr)
			}
		}
	}
}

// extractMemberIDFromAuth 從 Authorization header 的 JWT payload 取 user_id
func extractMemberIDFromAuth(auth string) string {
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == auth || token == "" {
		return ""
	}
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
		UserID string `json:"user_id"`
		Sub    string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.UserID != "" {
		return claims.UserID
	}
	return claims.Sub
}
