package mirror

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/urusaqqrun/vault-mirror-service/model"
	"golang.org/x/sync/errgroup"
)

var hexIDRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

// Exporter 負責資料庫資料 → Vault 檔案的匯出
type Exporter struct {
	fs       VaultFS
	resolver *PathResolver

	// docPathIndex 快取 docID -> 路徑，避免每次匯出都全量 walk EFS
	indexMu      sync.RWMutex
	indexBuild   sync.Mutex
	indexUserID  string
	docPathIndex map[string]string
}

func NewExporter(fs VaultFS, resolver *PathResolver) *Exporter {
	return &Exporter{fs: fs, resolver: resolver}
}

// ExportItemResult ExportItem 的回傳結果
type ExportItemResult struct {
	Path     string // 實際寫入的完整路徑
	IsFolder bool
}

// ExportItem 通用匯出：每個 item 都對應一個 .json 檔。
// 檔名規則：同層級無同名 → {sanitizeName(name)}.json；有同名 → {sanitizeName(name)}_{id}.json
func (e *Exporter) ExportItem(userId string, item *model.Item) (ExportItemResult, error) {
	mirrorData := ItemToMirrorData(item)
	parentDirPath := e.ResolveParentDir(userId, item.GetParentID(), item.Type)
	if err := e.fs.MkdirAll(parentDirPath); err != nil {
		return ExportItemResult{}, fmt.Errorf("mkdir parent: %w", err)
	}

	needsID := e.resolver.NeedsIDSuffix(mirrorData.ID)
	fileName := BuildFileName(mirrorData.Name, mirrorData.ID, needsID)
	fullPath := filepath.Join(parentDirPath, fileName)

	e.cleanupOldItemPath(userId, mirrorData.ID, fullPath)

	if needsID {
		e.ensureSiblingsHaveIDSuffix(userId, mirrorData.ID, parentDirPath)
	}

	jsonBytes, err := ItemToMirrorJSON(mirrorData)
	if err != nil {
		return ExportItemResult{}, fmt.Errorf("marshal item json: %w", err)
	}

	if err := e.fs.WriteFile(fullPath, jsonBytes); err != nil {
		return ExportItemResult{}, err
	}

	e.setIndexedPath(userId, mirrorData.ID, fullPath)
	return ExportItemResult{
		Path:     fullPath,
		IsFolder: model.IsFolder(item.Type),
	}, nil
}

// ensureSiblingsHaveIDSuffix 確保同名 sibling 的檔案也帶有 _id 後綴。
// 場景：新增 item 造成同名衝突時，已存在的 sibling 原本是 name.json，需改名為 name_id.json。
func (e *Exporter) ensureSiblingsHaveIDSuffix(userID, itemID, parentDirPath string) {
	siblings := e.resolver.GetConflictingSiblings(itemID)
	for _, sibID := range siblings {
		e.ensureDocPathIndex(userID)
		oldPath := e.getIndexedPath(userID, sibID)
		if oldPath == "" {
			continue
		}
		data, err := e.fs.ReadFile(oldPath)
		if err != nil {
			continue
		}
		sibItem, err := MirrorJSONToItem(data)
		if err != nil {
			continue
		}
		expectedName := BuildFileName(sibItem.Name, sibItem.ID, true)
		expectedPath := filepath.Join(parentDirPath, expectedName)
		if oldPath == expectedPath {
			continue
		}
		if !e.fs.Exists(oldPath) {
			continue
		}
		if err := e.fs.Rename(oldPath, expectedPath); err != nil {
			continue
		}
		e.setIndexedPath(userID, sibID, expectedPath)
		oldDir := strings.TrimSuffix(oldPath, ".json")
		newDir := strings.TrimSuffix(expectedPath, ".json")
		if oldDir != oldPath && e.fs.Exists(oldDir) {
			_ = e.fs.Rename(oldDir, newDir)
		}
	}
}

// ResolveParentDir returns the directory path where an item's .json should be written.
// FOLDER + 無 parent → {TYPE}/；非 FOLDER + 無 parent → {TYPE}/_unsorted/
// 孤兒 / circular ref → {TYPE}/_unsorted/
func (e *Exporter) ResolveParentDir(userID, parentID, itemType string) string {
	typeFallback := filepath.Join(userID, resolveTypeFromItemType(itemType), "_unsorted")
	if parentID == "" {
		if model.IsFolder(itemType) {
			return filepath.Join(userID, resolveTypeFromItemType(itemType))
		}
		return typeFallback
	}

	resolvedPath, err := e.resolver.ResolvePath(parentID)
	if err != nil || resolvedPath == "" {
		return typeFallback
	}
	return e.resolveIndexedParentDir(userID, parentID, filepath.Join(userID, resolvedPath))
}

func (e *Exporter) resolveIndexedParentDir(userID, parentID, fallbackPath string) string {
	if parentID == "" {
		return fallbackPath
	}
	e.ensureDocPathIndex(userID)
	if indexed := e.getIndexedPath(userID, parentID); indexed != "" {
		return strings.TrimSuffix(indexed, ".json")
	}
	return fallbackPath
}

// cleanupOldItemPath 清理同 ID 但舊位置的投影（改名/搬移情境）。
func (e *Exporter) cleanupOldItemPath(userID, docID, newPath string) {
	e.ensureDocPathIndex(userID)
	oldPath := e.getIndexedPath(userID, docID)
	if oldPath == "" || oldPath == newPath {
		return
	}

	_ = e.fs.Remove(oldPath)

	oldDir := strings.TrimSuffix(oldPath, ".json")
	newDir := strings.TrimSuffix(newPath, ".json")
	if oldDir == newDir || !e.fs.Exists(oldDir) {
		return
	}
	if !e.fs.Exists(newDir) {
		if err := e.fs.Rename(oldDir, newDir); err == nil {
			return
		}
	}
	_ = e.fs.RemoveAll(oldDir)
}

// DeleteItem 通用刪除：刪除 item 的 .json 與同名子目錄。
// 刪除後若同目錄下同名檔案從多個變成一個，自動 rename 回不帶 _id 的名稱。
func (e *Exporter) DeleteItem(userId, itemID string) error {
	e.ensureDocPathIndex(userId)
	oldPath := e.getIndexedPath(userId, itemID)
	if oldPath == "" {
		return nil
	}

	// 刪除前取得 item name（用於後續 sibling rename 判斷）
	var itemName string
	if e.fs.Exists(oldPath) {
		if data, readErr := e.fs.ReadFile(oldPath); readErr == nil {
			var obj map[string]interface{}
			if json.Unmarshal(data, &obj) == nil {
				itemName, _ = obj["name"].(string)
			}
		}
		if err := e.fs.Remove(oldPath); err != nil {
			return err
		}
	}
	dirPath := strings.TrimSuffix(oldPath, ".json")
	if dirPath != oldPath && e.fs.Exists(dirPath) {
		if err := e.fs.RemoveAll(dirPath); err != nil {
			return err
		}
	}
	e.removeIndexedPath(userId, itemID)
	e.resolver.RemoveNode(itemID)

	if itemName != "" {
		e.renameSoleSiblingAfterDelete(userId, filepath.Dir(oldPath), itemName)
	}

	return nil
}

// renameSoleSiblingAfterDelete 在刪除後，若同目錄只剩一個同名帶 _id 的檔案，rename 回不帶 _id。
func (e *Exporter) renameSoleSiblingAfterDelete(userID, parentDir, itemName string) {
	sanitized := sanitizeName(itemName)
	prefix := sanitized + "_"

	var matches []string
	_ = e.fs.Walk(parentDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || filepath.Dir(path) != parentDir {
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}
		base := strings.TrimSuffix(filepath.Base(path), ".json")
		if strings.HasPrefix(base, prefix) {
			suffix := base[len(prefix):]
			if hexIDRe.MatchString(suffix) {
				matches = append(matches, path)
			}
		}
		return nil
	})

	if len(matches) != 1 {
		return
	}
	oldPath := matches[0]
	newPath := filepath.Join(parentDir, sanitized+".json")
	if oldPath == newPath || e.fs.Exists(newPath) {
		return
	}
	if err := e.fs.Rename(oldPath, newPath); err != nil {
		return
	}
	oldDir := strings.TrimSuffix(oldPath, ".json")
	newDir := strings.TrimSuffix(newPath, ".json")
	if oldDir != oldPath && e.fs.Exists(oldDir) {
		_ = e.fs.Rename(oldDir, newDir)
	}
	oldBase := strings.TrimSuffix(filepath.Base(oldPath), ".json")
	sibID := oldBase[len(prefix):]
	e.setIndexedPath(userID, sibID, newPath)
}

// ExportBatch 使用 errgroup 並行寫入多個檔案到 EFS（上限 8 goroutines）
// 所有 item 統一走 ExportItem。
func (e *Exporter) ExportBatch(userId string, items []*model.Item) error {
	g := new(errgroup.Group)
	g.SetLimit(8)
	for _, item := range items {
		item := item
		g.Go(func() error {
			if item == nil {
				return nil
			}
			_, err := e.ExportItem(userId, item)
			return err
		})
	}
	return g.Wait()
}

func (e *Exporter) ensureDocPathIndex(userID string) {
	e.indexMu.RLock()
	if e.docPathIndex != nil && e.indexUserID == userID {
		e.indexMu.RUnlock()
		return
	}
	e.indexMu.RUnlock()

	e.indexBuild.Lock()
	defer e.indexBuild.Unlock()

	e.indexMu.RLock()
	if e.docPathIndex != nil && e.indexUserID == userID {
		e.indexMu.RUnlock()
		return
	}
	e.indexMu.RUnlock()

	next := make(map[string]string)
	e.fs.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, rErr := e.fs.ReadFile(path)
		if rErr != nil {
			return nil
		}
		if strings.HasSuffix(path, ".json") {
			if mirrorItem, err := MirrorJSONToItem(data); err == nil {
				next[mirrorItem.ID] = path
				return nil
			}
			var payload map[string]interface{}
			if uErr := json.Unmarshal(data, &payload); uErr == nil {
				if id, ok := payload["id"].(string); ok && id != "" {
					next[id] = path
				}
			}
		}
		return nil
	})

	e.indexMu.Lock()
	e.docPathIndex = next
	e.indexUserID = userID
	e.indexMu.Unlock()
}

func (e *Exporter) getIndexedPath(userID, docID string) string {
	e.indexMu.RLock()
	defer e.indexMu.RUnlock()
	if e.indexUserID != userID || e.docPathIndex == nil {
		return ""
	}
	return e.docPathIndex[docID]
}

func (e *Exporter) setIndexedPath(userID, docID, path string) {
	e.ensureDocPathIndex(userID)
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	if e.indexUserID != userID || e.docPathIndex == nil {
		return
	}
	e.docPathIndex[docID] = path
}

func (e *Exporter) removeIndexedPath(userID, docID string) {
	e.ensureDocPathIndex(userID)
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	if e.indexUserID != userID || e.docPathIndex == nil {
		return
	}
	delete(e.docPathIndex, docID)
}
