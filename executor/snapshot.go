package executor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// isSystemFile 判斷是否為系統檔案（不參與 diff/回寫）
func isSystemFile(relPath string) bool {
	if relPath == "CLAUDE.md" {
		return true
	}
	if strings.HasPrefix(relPath, ".CubeLV/") {
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

		// 統一從 JSON 讀取 ID
		if strings.HasSuffix(path, ".json") {
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

// TakeIncrementalSnapshot 增量快照：只對 mtime 比 since 更新的檔案重新讀取+hash，
// 其餘沿用 prev 的資料。同時偵測新增與刪除。
func TakeIncrementalSnapshot(
	vaultFS mirror.VaultFS,
	userID string,
	prev map[string]FileSnapshot,
	prevIDMap map[string]string,
	since time.Time,
) (map[string]FileSnapshot, map[string]string, error) {
	snap := make(map[string]FileSnapshot, len(prev))
	idMap := make(map[string]string, len(prevIDMap))
	var reusedCount, rehashedCount int

	err := vaultFS.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}

		relPath := path
		prefix := userID + "/"
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			relPath = path[len(prefix):]
		}
		if isSystemFile(relPath) {
			return nil
		}

		// mtime 沒變且 prev 有記錄 → 直接沿用
		if old, ok := prev[relPath]; ok && !info.ModTime().After(since) {
			snap[relPath] = old
			if id, ok2 := prevIDMap[relPath]; ok2 {
				idMap[relPath] = id
			}
			reusedCount++
			return nil
		}

		// mtime 有變或新檔案 → 讀檔 + hash
		data, rErr := vaultFS.ReadFile(path)
		if rErr != nil {
			return nil
		}
		h := sha256.Sum256(data)
		snap[relPath] = FileSnapshot{
			Path:    relPath,
			Hash:    fmt.Sprintf("%x", h),
			ModTime: info.ModTime(),
		}
		rehashedCount++

		if strings.HasSuffix(path, ".json") {
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
	log.Printf("[CacheProfile] IncrementalSnapshot: reused=%d, rehashed=%d, total=%d",
		reusedCount, rehashedCount, len(snap))
	return snap, idMap, nil
}
