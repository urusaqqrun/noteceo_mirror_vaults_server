package mirror

import (
	"encoding/json"
	"fmt"
	"log"
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
	Collection string // "item"（統一寫入 Item collection）
	ItemType   string // FOLDER / NOTE / TODO / CARD / CHART（回寫 Item 時用）
	Path       string
	OldPath    string // 搬移時的舊路徑
	DocID      string // 刪除時從 beforeIDMap 取得

	// 解析後的資料（依 ItemType 填入對應欄位）
	FolderMeta *FolderMeta
	NoteMeta   *NoteMeta
	NoteBody   string // Markdown 原文（不含 frontmatter）
	CardMeta   *CardMeta
	HTMLHash   string // frontmatter 中的 htmlHash

	// 新格式：通用 Item JSON
	ItemData *ItemMirrorData
}

// Importer 負責 Vault 檔案 → MongoDB 資料的匯入
type Importer struct {
	fs VaultFS
}

func NewImporter(fs VaultFS) *Importer {
	return &Importer{fs: fs}
}

// ProcessDiff 根據 VaultDiff 產生匯入動作清單
// beforeIDMap: path→docID 映射，用於解析已刪除檔案的 ID（刪除的檔案無法讀取）
func (imp *Importer) ProcessDiff(userId string, created, modified, deleted []string, moved []MovedFileEntry, beforeIDMap map[string]string) ([]ImportEntry, error) {
	var entries []ImportEntry

	for _, path := range created {
		entry, err := imp.parseFile(userId, path, ImportActionCreate)
		if err != nil {
			log.Printf("[Importer] skip created %s: %v", path, err)
			continue
		}
		entries = append(entries, entry)
	}

	for _, path := range modified {
		entry, err := imp.parseFile(userId, path, ImportActionUpdate)
		if err != nil {
			log.Printf("[Importer] skip modified %s: %v", path, err)
			continue
		}
		entries = append(entries, entry)
	}

	for _, path := range deleted {
		entry := ImportEntry{
			Action:     ImportActionDelete,
			Collection: "item",
			ItemType:   detectItemType(path),
			Path:       path,
		}
		if beforeIDMap != nil {
			if id, ok := beforeIDMap[path]; ok {
				entry.DocID = id
			}
		}
		entries = append(entries, entry)
	}

	for _, m := range moved {
		entry, err := imp.parseFile(userId, m.NewPath, ImportActionMove)
		if err != nil {
			log.Printf("[Importer] skip moved %s -> %s: %v", m.OldPath, m.NewPath, err)
			continue
		}
		entry.OldPath = m.OldPath
		entries = append(entries, entry)
	}

	return entries, nil
}

// parseFile 解析 Vault 中的檔案內容，統一回寫到 Item collection
func (imp *Importer) parseFile(userId, path string, action ImportAction) (ImportEntry, error) {
	fullPath := filepath.Join(userId, path)
	data, err := imp.fs.ReadFile(fullPath)
	if err != nil {
		return ImportEntry{}, fmt.Errorf("read %s: %w", fullPath, err)
	}

	// 優先嘗試新格式（含 id + itemType 的 JSON）
	if strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, "_folder.json") {
		if mirrorItem, err := MirrorJSONToItem(data); err == nil {
			entry := ImportEntry{
				Action:     action,
				Collection: "item",
				ItemType:   mirrorItem.ItemType,
				Path:       path,
				DocID:      mirrorItem.ID,
				ItemData:   mirrorItem,
			}
			// vault fallback name 不回寫到 DB
			if IsVaultFallbackName(mirrorItem.Name, mirrorItem.ID) {
				mirrorItem.Name = ""
			}
			return entry, nil
		}
	}

	// 舊格式
	itemType := detectItemType(path)
	entry := ImportEntry{
		Action:     action,
		Collection: "item",
		ItemType:   itemType,
		Path:       path,
	}

	switch itemType {
	case "FOLDER":
		meta, err := JSONToFolder(data)
		if err != nil {
			return entry, fmt.Errorf("parse folder json: %w", err)
		}
		entry.FolderMeta = &meta

	case "NOTE", "TODO":
		meta, body, err := MarkdownToNote(string(data))
		if err != nil {
			return entry, fmt.Errorf("parse note md: %w", err)
		}
		entry.NoteMeta = &meta
		entry.NoteBody = body
		entry.HTMLHash = meta.HTMLHash

	case "CARD", "CHART":
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

// detectItemType 從路徑推斷 Item 的 itemType
func detectItemType(path string) string {
	if strings.HasSuffix(path, "_folder.json") {
		return "FOLDER"
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "NOTE"
	}

	rootDir := strings.ToUpper(parts[0])
	switch rootDir {
	case "CARD":
		return "CARD"
	case "CHART":
		return "CHART"
	case "TODO":
		return "TODO"
	case "NOTE":
		return "NOTE"
	default:
		// 新格式：根目錄名稱即為 itemType（KANBAN → KANBAN）
		if strings.HasSuffix(path, ".json") {
			return rootDir
		}
		return "NOTE"
	}
}

// detectCollection 從路徑推斷 collection 類型（向後相容，統一回傳 "item"）
func detectCollection(path string) string {
	return "item"
}
