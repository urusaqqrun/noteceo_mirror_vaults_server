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
	if strings.HasPrefix(relPath, "plugins/") {
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
	var walkCount, hashCount, jsonParseCount int
	var totalBytes int64
	err := vaultFS.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		walkCount++
		data, rErr := vaultFS.ReadFile(path)
		if rErr != nil {
			return nil
		}
		totalBytes += int64(len(data))
		h := sha256.Sum256(data)
		relPath := path
		prefix := userID + "/"
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			relPath = path[len(prefix):]
		}
		if isSystemFile(relPath) {
			return nil
		}
		hashCount++
		snap[relPath] = FileSnapshot{
			Path:    relPath,
			Hash:    fmt.Sprintf("%x", h),
			ModTime: info.ModTime(),
		}

		if strings.HasSuffix(path, ".json") {
			var doc map[string]any
			if jErr := json.Unmarshal(data, &doc); jErr == nil {
				if id, ok := doc["id"].(string); ok && id != "" {
					idMap[relPath] = id
					jsonParseCount++
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	log.Printf("[CacheProfile] TakeSnapshotFull: walked=%d, hashed=%d, jsonParsed=%d, totalBytes=%d, files=%d",
		walkCount, hashCount, jsonParseCount, totalBytes, len(snap))
	return snap, idMap, nil
}

// TakeIncrementalSnapshot 增量快照：比對每個檔案的 mtime 與 prev 記錄，
// 只有 mtime 不同或新增的檔案才重新讀取+hash，其餘沿用 prev。
func TakeIncrementalSnapshot(
	vaultFS mirror.VaultFS,
	userID string,
	prev map[string]FileSnapshot,
	prevIDMap map[string]string,
) (map[string]FileSnapshot, map[string]string, error) {
	snap := make(map[string]FileSnapshot, len(prev))
	idMap := make(map[string]string, len(prevIDMap))
	var reusedCount, rehashedCount, walkCount, jsonParseCount int
	var rehashedBytes int64

	start := time.Now()
	err := vaultFS.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		walkCount++

		relPath := path
		prefix := userID + "/"
		if len(path) > len(prefix) && path[:len(prefix)] == prefix {
			relPath = path[len(prefix):]
		}
		if isSystemFile(relPath) {
			return nil
		}

		if old, ok := prev[relPath]; ok && old.ModTime.UnixMilli() == info.ModTime().UnixMilli() {
			snap[relPath] = old
			if id, ok2 := prevIDMap[relPath]; ok2 {
				idMap[relPath] = id
			}
			reusedCount++
			return nil
		}

		data, rErr := vaultFS.ReadFile(path)
		if rErr != nil {
			return nil
		}
		rehashedBytes += int64(len(data))
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
					jsonParseCount++
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	log.Printf("[CacheProfile] IncrementalSnapshot: walked=%d, reused=%d, rehashed=%d, rehashedBytes=%d, jsonParsed=%d, total=%d, elapsed=%dms",
		walkCount, reusedCount, rehashedCount, rehashedBytes, jsonParseCount, len(snap), time.Since(start).Milliseconds())
	return snap, idMap, nil
}
