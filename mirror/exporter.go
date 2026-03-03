package mirror

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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

	return e.fs.WriteFile(filepath.Join(userId, cardPath), data)
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

	return e.fs.WriteFile(filepath.Join(userId, chartPath), data)
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
