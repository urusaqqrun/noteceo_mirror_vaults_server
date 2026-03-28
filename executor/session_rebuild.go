package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// jsonlEntry is the per-line structure written to the .jsonl session file.
type jsonlEntry struct {
	Type       string                 `json:"type"`
	Message    jsonlMessage           `json:"message"`
	UUID       string                 `json:"uuid"`
	ParentUUID interface{}            `json:"parentUuid"` // string or null
	Timestamp  string                 `json:"timestamp"`
	SessionID  string                 `json:"sessionId"`
	Cwd        string                 `json:"cwd"`
}

type jsonlMessage struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

// RebuildSessionJSONL writes chat messages as .jsonl to the Claude CLI session path:
//
//	~/.claude/projects/<encoded-workDir>/sessions/<sessionID>.jsonl
//
// This enables --resume to restore conversation context when the CLI restarts.
func RebuildSessionJSONL(sessionID, workDir, memberID string, messages []SessionMessage) error {
	if sessionID == "" || len(messages) == 0 {
		return nil
	}

	// Compute the encoded project path used by Claude CLI
	encodedWorkDir := encodeProjectPath(workDir)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}

	projectDir := filepath.Join(homeDir, ".claude", "projects", encodedWorkDir)
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return fmt.Errorf("mkdir project dir: %w", err)
	}

	filePath := filepath.Join(projectDir, sessionID+".jsonl")
	cwd := filepath.Join("/vaults", memberID)

	var lines []string
	var prevUUID interface{} // null for the first entry

	for _, msg := range messages {
		entry := jsonlEntry{
			UUID:       msg.ID,
			ParentUUID: prevUUID,
			Timestamp:  msg.CreatedAt.UTC().Format(time.RFC3339Nano),
			SessionID:  sessionID,
			Cwd:        cwd,
		}

		switch msg.Role {
		case "user":
			entry.Type = "user"

			// 檢查 attachedItems 中是否有圖片
			var imageBlocks []interface{}
			if len(msg.AttachedItems) > 0 && string(msg.AttachedItems) != "null" {
				var items []map[string]interface{}
				if json.Unmarshal(msg.AttachedItems, &items) == nil {
					for _, item := range items {
						if itemType, _ := item["type"].(string); itemType == "image" {
							if url, _ := item["url"].(string); url != "" {
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

			var content []interface{}
			content = append(content, imageBlocks...)
			content = append(content, map[string]string{"type": "text", "text": msg.Content})

			entry.Message = jsonlMessage{
				Role:    "user",
				Content: content,
			}

		case "assistant":
			entry.Type = "assistant"
			var content []interface{}

			if msg.Thinking != "" {
				content = append(content, map[string]string{
					"type":     "thinking",
					"thinking": msg.Thinking,
				})
			}

			// Parse tool_calls if present
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
						var input interface{}
						if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
							input = map[string]interface{}{}
						}
						content = append(content, map[string]interface{}{
							"type":  "tool_use",
							"id":    tc.ID,
							"name":  tc.Function.Name,
							"input": input,
						})
					}
				}
			}

			if msg.Content != "" {
				content = append(content, map[string]string{
					"type": "text",
					"text": msg.Content,
				})
			}

			if len(content) == 0 {
				content = append(content, map[string]string{
					"type": "text",
					"text": "",
				})
			}

			entry.Message = jsonlMessage{
				Role:    "assistant",
				Content: content,
			}

		case "tool":
			entry.Type = "toolUseResult"
			entry.Message = jsonlMessage{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{
						"type":        "tool_result",
						"tool_use_id": msg.ToolCallID,
						"content":     msg.Content,
					},
				},
			}

		default:
			continue
		}

		lineBytes, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		lines = append(lines, string(lineBytes))
		prevUUID = msg.ID
	}

	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(filePath, []byte(content), 0644)
}

// encodeProjectPath produces the directory name Claude CLI uses for a project.
// CLI 的編碼方式：把絕對路徑的 "/" 替換成 "-"，開頭保留 "-"。
// 例如 /vaults/abc123 → -vaults-abc123
func encodeProjectPath(workDir string) string {
	absPath := filepath.Clean(workDir)
	return strings.ReplaceAll(absPath, "/", "-")
}
