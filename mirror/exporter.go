package mirror

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"

	"github.com/urusaqqrun/vault-mirror-service/model"
	"golang.org/x/sync/errgroup"
)

// Exporter 負責 MongoDB 資料 → Vault 檔案的匯出
type Exporter struct {
	fs       VaultFS
	resolver *PathResolver
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
	return e.fs.WriteFile(jsonPath, data)
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

	return e.fs.WriteFile(fullPath, []byte(md))
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
	return e.fs.WriteFile(fullPath, data)
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
	return e.fs.WriteFile(fullPath, data)
}

// DeleteFolder 刪除 Folder 對應的整個目錄
func (e *Exporter) DeleteFolder(userId string, folderID string) error {
	folderPath, err := e.resolver.ResolveFolderPath(folderID)
	if err != nil {
		return fmt.Errorf("resolve folder path: %w", err)
	}
	return e.fs.RemoveAll(filepath.Join(userId, folderPath))
}

// DeleteNote 刪除 Note 對應的 .md 檔案
func (e *Exporter) DeleteNote(userId string, noteID string, title string, parentFolderID string) error {
	notePath, err := e.resolver.ResolveNotePath(title, parentFolderID)
	if err != nil {
		return fmt.Errorf("resolve note path: %w", err)
	}
	return e.fs.Remove(filepath.Join(userId, notePath))
}

// cleanupOldNoteFile 清理同 ID 但路徑不同的舊 .md 檔案。
// 先掃描新路徑的父目錄（處理標題改名），
// 再 walk 整個用戶目錄（處理跨資料夾搬移）。
func (e *Exporter) cleanupOldNoteFile(userId string, noteID string, newFullPath string) {
	// 快速路徑：先只掃同一個目錄
	dir := filepath.Dir(newFullPath)
	if e.removeOldNoteInDir(dir, noteID, newFullPath) {
		return
	}

	// 慢路徑：walk 整個用戶根目錄處理跨資料夾搬移
	e.fs.Walk(userId, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") || path == newFullPath {
			return nil
		}
		data, rErr := e.fs.ReadFile(path)
		if rErr != nil {
			return nil
		}
		meta, _, pErr := MarkdownToNote(string(data))
		if pErr != nil {
			return nil
		}
		if meta.ID == noteID {
			e.fs.Remove(path)
			return filepath.SkipAll
		}
		return nil
	})
}

// ExportBatchEntry 批次匯出項目
type ExportBatchEntry struct {
	ItemType   string
	FolderMeta *FolderMeta
	NoteMeta   *NoteMeta
	NoteHTML   string
	CardMeta   *CardMeta
}

// ExportBatch 使用 errgroup 並行寫入多個檔案到 EFS（上限 8 goroutines）
func (e *Exporter) ExportBatch(userId string, entries []ExportBatchEntry) error {
	g := new(errgroup.Group)
	g.SetLimit(8)
	for _, entry := range entries {
		entry := entry
		g.Go(func() error {
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
	oldFolderPath := ""
	e.fs.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !strings.HasSuffix(path, "_folder.json") {
			return nil
		}
		data, rErr := e.fs.ReadFile(path)
		if rErr != nil {
			return nil
		}
		meta, pErr := JSONToFolder(data)
		if pErr != nil || meta.ID != folderID {
			return nil
		}
		if pathDir := filepath.Dir(path); pathDir != newFolderPath {
			oldFolderPath = pathDir
		}
		return filepath.SkipAll
	})
	if oldFolderPath != "" {
		_ = e.fs.RemoveAll(oldFolderPath)
	}
}

// cleanupOldCardPath 清理同 ID 但舊位置的 card/chart JSON 檔
func (e *Exporter) cleanupOldCardPath(userID string, docID string, newPath string) {
	oldPath := ""
	e.fs.Walk(userID, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".json") || strings.HasSuffix(path, "_folder.json") || path == newPath {
			return nil
		}
		data, rErr := e.fs.ReadFile(path)
		if rErr != nil {
			return nil
		}
		var payload map[string]interface{}
		if uErr := json.Unmarshal(data, &payload); uErr != nil {
			return nil
		}
		id, _ := payload["id"].(string)
		if id == docID {
			oldPath = path
			return filepath.SkipAll
		}
		return nil
	})
	if oldPath != "" {
		_ = e.fs.Remove(oldPath)
	}
}
