package mirror

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ImportAction 回寫動作類型
type ImportAction string

const (
	ImportActionCreate ImportAction = "create"
	ImportActionUpdate ImportAction = "update"
	ImportActionDelete ImportAction = "delete"
	ImportActionMove   ImportAction = "move"
	ImportActionSkip   ImportAction = "skip"
)

// ImportEntry 單筆回寫項目
type ImportEntry struct {
	Action     ImportAction
	Collection string // folder / note / card / chart
	Path       string
	OldPath    string // 搬移時的舊路徑

	// 解析後的資料（依 Collection 填入對應欄位）
	FolderMeta *FolderMeta
	NoteMeta   *NoteMeta
	NoteBody   string // Markdown 原文（不含 frontmatter）
	CardMeta   *CardMeta
	HTMLHash   string // frontmatter 中的 htmlHash
}

// Importer 負責 Vault 檔案 → MongoDB 資料的匯入
type Importer struct {
	fs VaultFS
}

func NewImporter(fs VaultFS) *Importer {
	return &Importer{fs: fs}
}

// ProcessDiff 根據 VaultDiff 產生匯入動作清單
func (imp *Importer) ProcessDiff(userId string, created, modified, deleted []string, moved []MovedFileEntry) ([]ImportEntry, error) {
	var entries []ImportEntry

	for _, path := range created {
		entry, err := imp.parseFile(userId, path, ImportActionCreate)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	for _, path := range modified {
		entry, err := imp.parseFile(userId, path, ImportActionUpdate)
		if err != nil {
			continue
		}

		// htmlHash 機制：hash 未變 → 跳過（AI 沒改內容）
		if entry.Collection == "note" && entry.NoteMeta != nil && entry.HTMLHash != "" {
			entry.Action = ImportActionUpdate
		}

		entries = append(entries, entry)
	}

	for _, path := range deleted {
		entry := ImportEntry{
			Action:     ImportActionDelete,
			Collection: detectCollection(path),
			Path:       path,
		}
		entries = append(entries, entry)
	}

	for _, m := range moved {
		entry, err := imp.parseFile(userId, m.NewPath, ImportActionMove)
		if err != nil {
			continue
		}
		entry.OldPath = m.OldPath
		entries = append(entries, entry)
	}

	return entries, nil
}

// parseFile 解析 Vault 中的檔案內容
func (imp *Importer) parseFile(userId, path string, action ImportAction) (ImportEntry, error) {
	fullPath := filepath.Join(userId, path)
	data, err := imp.fs.ReadFile(fullPath)
	if err != nil {
		return ImportEntry{}, fmt.Errorf("read %s: %w", fullPath, err)
	}

	collection := detectCollection(path)
	entry := ImportEntry{
		Action:     action,
		Collection: collection,
		Path:       path,
	}

	switch collection {
	case "folder":
		var meta FolderMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return entry, fmt.Errorf("parse folder json: %w", err)
		}
		entry.FolderMeta = &meta

	case "note":
		meta, body, err := MarkdownToNote(string(data))
		if err != nil {
			return entry, fmt.Errorf("parse note md: %w", err)
		}
		entry.NoteMeta = &meta
		entry.NoteBody = body
		entry.HTMLHash = meta.HTMLHash

	case "card", "chart":
		var meta CardMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return entry, fmt.Errorf("parse card/chart json: %w", err)
		}
		entry.CardMeta = &meta
	}

	return entry, nil
}

// MovedFileEntry 搬移的檔案
type MovedFileEntry struct {
	OldPath string
	NewPath string
}

// detectCollection 從路徑推斷 collection 類型
func detectCollection(path string) string {
	if strings.HasSuffix(path, "_folder.json") {
		return "folder"
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "note"
	}

	switch strings.ToUpper(parts[0]) {
	case "CARD":
		return "card"
	case "CHART":
		return "chart"
	case "TODO":
		return "note"
	default:
		if strings.HasSuffix(path, ".json") {
			return "card"
		}
		return "note"
	}
}
