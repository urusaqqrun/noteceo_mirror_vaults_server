package executor

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionMessage represents a chat message used for JSONL session rebuild.
type SessionMessage struct {
	ID            string          `json:"id"`
	Role          string          `json:"role"`
	Content       string          `json:"content"`
	Thinking      string          `json:"thinking,omitempty"`
	ToolCalls     json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID    string          `json:"tool_call_id,omitempty"`
	AttachedItems json.RawMessage `json:"attached_items,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

var (
	claudeVersionOnce sync.Once
	claudeVersionStr  string
)

func getClaudeVersion() string {
	claudeVersionOnce.Do(func() {
		out, err := exec.Command("claude", "--version").Output()
		if err == nil {
			claudeVersionStr = strings.TrimSpace(string(out))
		}
		if claudeVersionStr == "" {
			claudeVersionStr = "1.0.0"
		}
	})
	return claudeVersionStr
}

// RebuildSessionJSONL writes chat messages as .jsonl to the Claude CLI session path:
//
//	~/.claude/projects/<encoded-workDir>/sessions/<sessionID>.jsonl
//
// The output format matches the real Claude CLI JSONL schema so that
// `claude --resume <sessionID>` can correctly restore conversation context.
func RebuildSessionJSONL(sessionID, workDir, memberID string, messages []SessionMessage) error {
	if sessionID == "" || len(messages) == 0 {
		return nil
	}

	encodedWorkDir := encodeProjectPath(workDir)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	sessionsDir := filepath.Join(homeDir, ".claude", "projects", encodedWorkDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return fmt.Errorf("mkdir sessions dir: %w", err)
	}

	filePath := filepath.Join(sessionsDir, sessionID+".jsonl")
	cwd := workDir
	version := getClaudeVersion()

	var lines []string
	var prevUUID interface{} // null for the first entry

	// Track the last assistant UUID that contained a tool_use, for
	// sourceToolAssistantUUID on tool result entries.
	var lastToolAssistantUUID string

	for _, msg := range messages {
		ts := msg.CreatedAt.UTC().Format(time.RFC3339Nano)

		switch msg.Role {
		case "user":
			hasImages := false
			var imageBlocks []interface{}
			if len(msg.AttachedItems) > 0 && string(msg.AttachedItems) != "null" {
				var items []map[string]interface{}
				if json.Unmarshal(msg.AttachedItems, &items) == nil {
					for _, item := range items {
						if itemType, _ := item["type"].(string); itemType == "image" {
							if url, _ := item["url"].(string); url != "" {
								hasImages = true
								imageBlocks = append(imageBlocks, map[string]interface{}{
									"type": "image",
									"source": map[string]interface{}{
										"type": "url",
										"url":  url,
									},
								})
							}
						}
					}
				}
			}

			// Real CLI uses plain string for content; array only when multimodal
			var content interface{}
			if hasImages {
				blocks := make([]interface{}, 0, len(imageBlocks)+1)
				blocks = append(blocks, imageBlocks...)
				blocks = append(blocks, map[string]string{"type": "text", "text": msg.Content})
				content = blocks
			} else {
				content = msg.Content
			}

			entry := map[string]interface{}{
				"parentUuid":  prevUUID,
				"isSidechain": false,
				"type":        "user",
				"message": map[string]interface{}{
					"role":    "user",
					"content": content,
				},
				"uuid":       msg.ID,
				"timestamp":  ts,
				"userType":   "external",
				"entrypoint": "cli",
				"cwd":        cwd,
				"sessionId":  sessionID,
				"version":    version,
			}

			lineBytes, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			lines = append(lines, string(lineBytes))
			prevUUID = msg.ID

		case "assistant":
			var contentBlocks []interface{}

			if msg.Thinking != "" {
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type":     "thinking",
					"thinking": msg.Thinking,
				})
			}

			hasToolUse := false
			if len(msg.ToolCalls) > 0 && string(msg.ToolCalls) != "null" {
				var toolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if json.Unmarshal(msg.ToolCalls, &toolCalls) == nil {
					for _, tc := range toolCalls {
						hasToolUse = true
						var input interface{}
						if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
							input = map[string]interface{}{}
						}
						contentBlocks = append(contentBlocks, map[string]interface{}{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": input,
						})
					}
				}
			}

			if msg.Content != "" {
				contentBlocks = append(contentBlocks, map[string]string{
					"type": "text",
					"text": msg.Content,
				})
			}

			if len(contentBlocks) == 0 {
				contentBlocks = append(contentBlocks, map[string]string{
					"type": "text",
					"text": "",
				})
			}

			stopReason := "end_turn"
			if hasToolUse {
				stopReason = "tool_use"
				lastToolAssistantUUID = msg.ID
			}

			entry := map[string]interface{}{
				"parentUuid":  prevUUID,
				"isSidechain": false,
				"type":        "assistant",
				"message": map[string]interface{}{
					"role":          "assistant",
					"type":          "message",
					"content":       contentBlocks,
					"stop_reason":   stopReason,
					"stop_sequence": nil,
				},
				"uuid":       msg.ID,
				"timestamp":  ts,
				"userType":   "external",
				"entrypoint": "cli",
				"cwd":        cwd,
				"sessionId":  sessionID,
				"version":    version,
			}

			lineBytes, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			lines = append(lines, string(lineBytes))
			prevUUID = msg.ID

		case "tool":
			// Tool results are type "user" with content array containing tool_result blocks
			entry := map[string]interface{}{
				"parentUuid":  prevUUID,
				"isSidechain": false,
				"type":        "user",
				"message": map[string]interface{}{
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{
							"tool_use_id": msg.ToolCallID,
							"type":        "tool_result",
							"content":     msg.Content,
							"is_error":    false,
						},
					},
				},
				"uuid":      msg.ID,
				"timestamp": ts,
				"toolUseResult": map[string]interface{}{
					"stdout":      msg.Content,
					"stderr":      "",
					"interrupted": false,
				},
				"userType":   "external",
				"entrypoint": "cli",
				"cwd":        cwd,
				"sessionId":  sessionID,
				"version":    version,
			}
			if lastToolAssistantUUID != "" {
				entry["sourceToolAssistantUUID"] = lastToolAssistantUUID
			}

			lineBytes, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			lines = append(lines, string(lineBytes))
			prevUUID = msg.ID

		default:
			continue
		}
	}

	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return err
	}
	log.Printf("[SessionRebuild] wrote %s (%d entries, %d bytes)", filePath, len(lines), len(content))
	return nil
}

// CleanupSessionJSONL removes Claude CLI's session files so that
// --session-id won't conflict with an existing session on disk.
// Claude CLI stores sessions in the sessions/ subdirectory.
func CleanupSessionJSONL(sessionID, workDir string) {
	if sessionID == "" {
		return
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	encoded := encodeProjectPath(workDir)
	projectDir := filepath.Join(homeDir, ".claude", "projects", encoded)

	candidates := []string{
		filepath.Join(projectDir, "sessions", sessionID+".jsonl"),
		filepath.Join(projectDir, "sessions", sessionID+".lock"),
		filepath.Join(projectDir, sessionID+".jsonl"),
	}
	for _, path := range candidates {
		if err := os.Remove(path); err == nil {
			log.Printf("[SessionCleanup] removed: %s", path)
		}
	}
}

// encodeProjectPath produces the directory name Claude CLI uses for a project.
// CLI 的編碼方式：把絕對路徑的 "/" 替換成 "-"，開頭保留 "-"。
// 例如 /vaults/abc123 → -vaults-abc123
func encodeProjectPath(workDir string) string {
	absPath := filepath.Clean(workDir)
	return strings.ReplaceAll(absPath, "/", "-")
}
