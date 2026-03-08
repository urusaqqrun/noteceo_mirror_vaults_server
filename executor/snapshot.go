package executor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// isSystemFile 判斷是否為系統檔案（不參與 diff/回寫）
func isSystemFile(relPath string) bool {
	if relPath == "CLAUDE.md" {
		return true
	}
	if strings.HasPrefix(relPath, ".NoteCEO/") {
		return true
	}
	return false
}

// TakeSnapshot 掃描用戶 Vault 目錄產生檔案快照（用於 AI 任務前後 diff 比對）
func TakeSnapshot(vaultFS mirror.VaultFS, userID string) (map[string]FileSnapshot, error) {
	snap, _, err := TakeSnapshotAndPathIDMap(vaultFS, userID)
	return snap, err
}

// TakeSnapshotAndPathIDMap 單次 walk 同時建立 snapshot 與 path→docID。
func TakeSnapshotAndPathIDMap(vaultFS mirror.VaultFS, userID string) (map[string]FileSnapshot, map[string]string, error) {
	snap := make(map[string]FileSnapshot)
	idMap := make(map[string]string)
	err := vaultFS.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, rErr := vaultFS.ReadFile(path)
		if rErr != nil {
			return nil
		}
		h := sha256.Sum256(data)
		// path 已是相對於 VaultFS root 的路徑（含 userID 前綴），
		// 需去掉 userID/ 前綴讓 diff 結果可直接對應 Vault 內部路徑
		relPath := path
		prefix := userID + "/"
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			relPath = path[len(prefix):]
		}
		if isSystemFile(relPath) {
			return nil
		}
		snap[relPath] = FileSnapshot{
			Path:    relPath,
			Hash:    fmt.Sprintf("%x", h),
			ModTime: info.ModTime(),
		}

		if strings.HasSuffix(path, ".md") {
			meta, _, pErr := mirror.MarkdownToNote(string(data))
			if pErr == nil && meta.ID != "" {
				idMap[relPath] = meta.ID
			}
		} else if strings.HasSuffix(path, "_folder.json") {
			meta, jErr := mirror.JSONToFolder(data)
			if jErr == nil && meta.ID != "" {
				idMap[relPath] = meta.ID
			}
		} else if strings.HasSuffix(path, ".json") {
			var doc map[string]any
			if jErr := json.Unmarshal(data, &doc); jErr == nil {
				if id, ok := doc["id"].(string); ok && id != "" {
					idMap[relPath] = id
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return snap, idMap, nil
}
