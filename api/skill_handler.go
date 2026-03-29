package api

import (
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// SkillHandler handles AI skill instruction sync from plugins.
type SkillHandler struct {
	vaultFS mirror.VaultFS
}

func NewSkillHandler(fs mirror.VaultFS) *SkillHandler {
	return &SkillHandler{vaultFS: fs}
}

func (h *SkillHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/vault/skills", h.HandleSkills)
}

// HandleSkills receives skill instructions from plugins and writes/deletes
// .skills/{name}.md files in the user's vault directory.
// Body: { "skills": { "name": "instruction content", "other": "" } }
// Empty string value → delete the skill file; non-empty → write/overwrite.
func (h *SkillHandler) HandleSkills(w http.ResponseWriter, r *http.Request) {
	memberID := r.Header.Get("X-User-ID")
	if memberID == "" {
		http.Error(w, `{"error":"unauthorized"}`, 401)
		return
	}

	var req struct {
		Skills map[string]string `json:"skills"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, 400)
		return
	}

	if len(req.Skills) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"written":0,"deleted":0}`))
		return
	}

	skillsDir := filepath.Join(memberID, ".skills")
	h.vaultFS.MkdirAll(skillsDir)

	var written, deleted int
	for name, instruction := range req.Skills {
		path := filepath.Join(skillsDir, name+".md")
		if instruction == "" {
			// Delete the skill file
			if err := h.vaultFS.Remove(path); err != nil {
				log.Printf("[SkillHandler] delete %s error: %v", path, err)
			} else {
				deleted++
			}
		} else {
			// Write/overwrite the skill file
			if err := h.vaultFS.WriteFile(path, []byte(instruction)); err != nil {
				log.Printf("[SkillHandler] write %s error: %v", path, err)
			} else {
				written++
			}
		}
	}

	log.Printf("[SkillHandler] synced skills for %s: %d written, %d deleted", memberID, written, deleted)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"written": written,
		"deleted": deleted,
	})
}
