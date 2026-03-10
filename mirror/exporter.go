package mirror

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/urusaqqrun/vault-mirror-service/model"
	"golang.org/x/sync/errgroup"
)

// Exporter 負責 MongoDB 資料 → Vault 檔案的匯出
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

// ExportFolder 匯出 Folder 為目錄 + _folder.json
func (e *Exporter) ExportFolder(userId string, meta FolderMeta) error {
	folderPath, err := e.resolver.ResolveFolderPath(meta.ID)
	if err != nil {
		return fmt.Errorf("resolve folder path: %w", err)
	}

	fullPath := filepath.Join(userId, folderPath)
	e.cleanupOldFolderPath(userId, meta.ID, fullPath)
	if err := e.fs.MkdirAll(fullPath); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	data, err := FolderToJSON(meta)
	if err != nil {
		return fmt.Errorf("folder to json: %w", err)
	}

	jsonPath := filepath.Join(fullPath, "_folder.json")
	if err := e.fs.WriteFile(jsonPath, data); err != nil {
		return err
	}
	e.setIndexedPath(userId, meta.ID, fullPath)
	return nil
}

// ExportNote 匯出 Note 為 .md 檔案（含 frontmatter）
func (e *Exporter) ExportNote(userId string, meta NoteMeta, html string) error {
	notePath, err := e.resolver.ResolveNotePath(meta.Title, meta.ParentID)
	if err != nil {
		return fmt.Errorf("resolve note path: %w", err)
	}

	md, err := NoteToMarkdown(meta, html)
	if err != nil {
		return fmt.Errorf("note to markdown: %w", err)
	}

	fullPath := filepath.Join(userId, notePath)

	// 先刪掉同 ID 但不同標題的舊檔案（標題改了 → 路徑改了）
	e.cleanupOldNoteFile(userId, meta.ID, fullPath)

	if err := e.fs.WriteFile(fullPath, []byte(md)); err != nil {
		return err
	}
	e.setIndexedPath(userId, meta.ID, fullPath)
	return nil
}

// ExportCard 匯出 Card 為 .json 檔案
func (e *Exporter) ExportCard(userId string, meta CardMeta) error {
	cardPath, err := e.resolver.ResolveCardPath(meta.Name, meta.ParentID)
	if err != nil {
		return fmt.Errorf("resolve card path: %w", err)
	}

	data, err := CardToJSON(meta)
	if err != nil {
		return fmt.Errorf("card to json: %w", err)
	}

	fullPath := filepath.Join(userId, cardPath)
	e.cleanupOldCardPath(userId, meta.ID, fullPath)
	if err := e.fs.WriteFile(fullPath, data); err != nil {
		return err
	}
	e.setIndexedPath(userId, meta.ID, fullPath)
	return nil
}

// ExportChart 匯出 Chart 為 .json 檔案
func (e *Exporter) ExportChart(userId string, meta CardMeta) error {
	chartPath, err := e.resolver.ResolveChartPath(meta.Name, meta.ParentID)
	if err != nil {
		return fmt.Errorf("resolve chart path: %w", err)
	}

	data, err := CardToJSON(meta)
	if err != nil {
		return fmt.Errorf("chart to json: %w", err)
	}

	fullPath := filepath.Join(userId, chartPath)
	e.cleanupOldCardPath(userId, meta.ID, fullPath)
	if err := e.fs.WriteFile(fullPath, data); err != nil {
		return err
	}
	e.setIndexedPath(userId, meta.ID, fullPath)
	return nil
}

// ExportItem 通用匯出：將任意 Item 寫為 name.json（資料夾會先建目錄）
func (e *Exporter) ExportItem(userId string, item *model.Item) error {
	mirrorData := ItemToMirrorData(item)

	if model.IsFolder(item.Type) {
		return e.exportFolderItem(userId, mirrorData)
	}
	return e.exportLeafItem(userId, mirrorData, item)
}

// exportFolderItem 匯出資料夾類型：建目錄 + 寫 name.json
func (e *Exporter) exportFolderItem(userId string, data ItemMirrorData) error {
	folderPath, err := e.resolver.ResolveFolderPath(data.ID)
	if err != nil {
		return fmt.Errorf("resolve folder path: %w", err)
	}

	fullDirPath := filepath.Join(userId, folderPath)
	e.cleanupOldFolderPath(userId, data.ID, fullDirPath)
	if err := e.fs.MkdirAll(fullDirPath); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	jsonBytes, err := ItemToMirrorJSON(data)
	if err != nil {
		return fmt.Errorf("marshal item json: %w", err)
	}

	jsonPath := filepath.Join(fullDirPath, sanitizeName(data.Name)+".json")
	if err := e.fs.WriteFile(jsonPath, jsonBytes); err != nil {
		return err
	}
	e.setIndexedPath(userId, data.ID, fullDirPath)
	return nil
}

// exportLeafItem 匯出非資料夾類型：寫 name.json 到所屬資料夾
func (e *Exporter) exportLeafItem(userId string, data ItemMirrorData, item *model.Item) error {
	folderID := item.GetFolderID()
	folderPath, err := e.resolver.ResolveFolderPath(folderID)
	if err != nil {
		return fmt.Errorf("resolve parent folder: %w", err)
	}

	fileName := sanitizeName(data.Name) + ".json"
	fullPath := filepath.Join(userId, folderPath, fileName)

	// 檔名衝突處理：同路徑已存在且 ID 不同 → 加 ID 後綴
	fullPath = e.resolveCollision(fullPath, data.ID)

	e.cleanupOldItemPath(userId, data.ID, fullPath)

	jsonBytes, err := ItemToMirrorJSON(data)
	if err != nil {
		return fmt.Errorf("marshal item json: %w", err)
	}

	if err := e.fs.WriteFile(fullPath, jsonBytes); err != nil {
		return err
	}
	e.setIndexedPath(userId, data.ID, fullPath)
	return nil
}

// resolveCollision 若目標路徑已被不同 ID 佔用，加 _{id後8碼} 後綴
func (e *Exporter) resolveCollision(targetPath, itemID string) string {
	if !e.fs.Exists(targetPath) {
		return targetPath
	}
	existing, err := e.fs.ReadFile(targetPath)
	if err != nil {
		return targetPath
	}
	parsed, err := MirrorJSONToItem(existing)
	if err != nil || parsed.ID == itemID {
		return targetPath
	}
	ext := filepath.Ext(targetPath)
	base := strings.TrimSuffix(targetPath, ext)
	suffix := itemID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	return base + "_" + suffix + ext
}

// cleanupOldItemPath 清理同 ID 但舊位置的檔案（改名/搬移情境）
func (e *Exporter) cleanupOldItemPath(userID, docID, newPath string) {
	e.ensureDocPathIndex(userID)
	oldPath := e.getIndexedPath(userID, docID)
	if oldPath != "" && oldPath != newPath {
		_ = e.fs.Remove(oldPath)
	}
}

// DeleteItem 通用刪除：根據 docPathIndex 找到路徑後刪除
func (e *Exporter) DeleteItem(userId, itemID string) error {
	e.ensureDocPathIndex(userId)
	oldPath := e.getIndexedPath(userId, itemID)
	if oldPath == "" {
		return nil
	}
	// 判斷是檔案還是目錄
	info, err := e.fs.Stat(oldPath)
	if err != nil {
		e.removeIndexedPath(userId, itemID)
		return nil
	}
	if info.IsDir() {
		if err := e.fs.RemoveAll(oldPath); err != nil {
			return err
		}
	} else {
		if err := e.fs.Remove(oldPath); err != nil {
			return err
		}
	}
	e.removeIndexedPath(userId, itemID)
	return nil
}

// Deprecated: DeleteFolder 舊版方法，請改用 DeleteItem
func (e *Exporter) DeleteFolder(userId string, folderID string) error {
	folderPath, err := e.resolver.ResolveFolderPath(folderID)
	if err != nil {
		return fmt.Errorf("resolve folder path: %w", err)
	}
	if err := e.fs.RemoveAll(filepath.Join(userId, folderPath)); err != nil {
		return err
	}
	e.removeIndexedPath(userId, folderID)
	return nil
}

// DeleteNote 刪除 Note 對應的 .md 檔案
func (e *Exporter) DeleteNote(userId string, noteID string, title string, parentFolderID string) error {
	notePath, err := e.resolver.ResolveNotePath(title, parentFolderID)
	if err != nil {
		return fmt.Errorf("resolve note path: %w", err)
	}
	if err := e.fs.Remove(filepath.Join(userId, notePath)); err != nil {
		return err
	}
	e.removeIndexedPath(userId, noteID)
	return nil
}

// cleanupOldNoteFile 清理同 ID 但路徑不同的舊 .md 檔案。
// 先掃描新路徑的父目錄（處理標題改名），
// 再 walk 整個用戶目錄（處理跨資料夾搬移）。
func (e *Exporter) cleanupOldNoteFile(userId string, noteID string, newFullPath string) {
	e.ensureDocPathIndex(userId)
	oldPath := e.getIndexedPath(userId, noteID)
	if oldPath != "" && oldPath != newFullPath {
		_ = e.fs.Remove(oldPath)
	}
}

// ExportBatchEntry 批次匯出項目
type ExportBatchEntry struct {
	ItemType   string
	FolderMeta *FolderMeta
	NoteMeta   *NoteMeta
	NoteHTML   string
	CardMeta   *CardMeta
	Item       *model.Item // 新格式：通用 Item 匯出
}

// ExportBatch 使用 errgroup 並行寫入多個檔案到 EFS（上限 8 goroutines）
func (e *Exporter) ExportBatch(userId string, entries []ExportBatchEntry) error {
	g := new(errgroup.Group)
	g.SetLimit(8)
	for _, entry := range entries {
		entry := entry
		g.Go(func() error {
			// 新格式優先
			if entry.Item != nil {
				return e.ExportItem(userId, entry.Item)
			}
			// 舊格式相容
			switch {
			case model.IsFolder(entry.ItemType):
				if entry.FolderMeta == nil {
					return nil
				}
				return e.ExportFolder(userId, *entry.FolderMeta)
			case entry.ItemType == "NOTE" || entry.ItemType == "TODO":
				if entry.NoteMeta == nil {
					return nil
				}
				return e.ExportNote(userId, *entry.NoteMeta, entry.NoteHTML)
			case entry.ItemType == "CARD":
				if entry.CardMeta == nil {
					return nil
				}
				return e.ExportCard(userId, *entry.CardMeta)
			case entry.ItemType == "CHART":
				if entry.CardMeta == nil {
					return nil
				}
				return e.ExportChart(userId, *entry.CardMeta)
			default:
				log.Printf("[ExportBatch] unknown itemType: %s", entry.ItemType)
				return nil
			}
		})
	}
	return g.Wait()
}

// removeOldNoteInDir 掃描單一目錄，找到同 ID 舊檔就刪除並回傳 true。
func (e *Exporter) removeOldNoteInDir(dir string, noteID string, excludePath string) bool {
	entries, err := e.fs.ListDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if path == excludePath {
			continue
		}
		data, err := e.fs.ReadFile(path)
		if err != nil {
			continue
		}
		meta, _, err := MarkdownToNote(string(data))
		if err != nil {
			continue
		}
		if meta.ID == noteID {
			e.fs.Remove(path)
			return true
		}
	}
	return false
}

// cleanupOldFolderPath 清理同 ID 但舊位置的資料夾（搬移/改名情境）
func (e *Exporter) cleanupOldFolderPath(userID string, folderID string, newFolderPath string) {
	e.ensureDocPathIndex(userID)
	oldFolderPath := e.getIndexedPath(userID, folderID)
	if oldFolderPath != "" && oldFolderPath != newFolderPath {
		_ = e.fs.RemoveAll(oldFolderPath)
	}
}

// cleanupOldCardPath 清理同 ID 但舊位置的 card/chart JSON 檔
func (e *Exporter) cleanupOldCardPath(userID string, docID string, newPath string) {
	e.ensureDocPathIndex(userID)
	oldPath := e.getIndexedPath(userID, docID)
	if oldPath != "" && oldPath != newPath {
		_ = e.fs.Remove(oldPath)
	}
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
		if strings.HasSuffix(path, "_folder.json") {
			meta, pErr := JSONToFolder(data)
			if pErr == nil && meta.ID != "" {
				next[meta.ID] = filepath.Dir(path)
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			meta, _, pErr := MarkdownToNote(string(data))
			if pErr == nil && meta.ID != "" {
				next[meta.ID] = path
			}
			return nil
		}
		if strings.HasSuffix(path, ".json") {
			// 優先嘗試新格式（含 itemType）
			if mirrorItem, err := MirrorJSONToItem(data); err == nil {
				if model.IsFolder(mirrorItem.ItemType) {
					next[mirrorItem.ID] = filepath.Dir(path)
				} else {
					next[mirrorItem.ID] = path
				}
				return nil
			}
			// 退回舊格式
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