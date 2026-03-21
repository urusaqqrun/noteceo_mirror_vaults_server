package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
)

var (
	ErrVaultLocked = errors.New("user vault is locked by active AI task")
)

// VaultLocker 查詢 Vault 鎖定狀態（避免循環依賴 executor 套件）
type VaultLocker interface {
	IsLocked(userId string) bool
}

// DataReader 事件處理所需的資料讀取能力
// 由 database 層提供實作，測試可用 mock。
type DataReader interface {
	ListFolders(ctx context.Context, userID string) ([]*model.Folder, error)
	GetFolder(ctx context.Context, userID, folderID string) (*model.Folder, error)
	GetNote(ctx context.Context, userID, noteID string) (*model.Note, error)
	GetCard(ctx context.Context, userID, cardID string) (*model.Card, error)
	GetChart(ctx context.Context, userID, chartID string) (*model.Chart, error)
	GetItem(ctx context.Context, userID, itemID string) (*model.Item, error)
	ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error)
}

// resolverEntry PathResolver 快取項目
type resolverEntry struct {
	resolver  *mirror.PathResolver
	expiresAt time.Time
}

type docPathEntry struct {
	path     string
	isFolder bool
}

type docPathIndexEntry struct {
	entries   map[string]docPathEntry
	expiresAt time.Time
}

const resolverCacheTTL = 30 * time.Second
const docPathIndexTTL = 30 * time.Second

// SyncEventHandler 將同步事件轉為 Vault 匯出動作
type SyncEventHandler struct {
	fs     mirror.VaultFS
	reader DataReader
	locker VaultLocker // nil 時不檢查鎖定

	mu            sync.Mutex
	resolverCache map[string]*resolverEntry
	docPathIndex  map[string]*docPathIndexEntry
}

func NewSyncEventHandler(fs mirror.VaultFS, reader DataReader) *SyncEventHandler {
	return &SyncEventHandler{
		fs:            fs,
		reader:        reader,
		resolverCache: make(map[string]*resolverEntry),
		docPathIndex:  make(map[string]*docPathIndexEntry),
	}
}

// StartCacheEvictor 啟動定期清理過期快取的背景 goroutine，ctx 結束時自動停止
func (h *SyncEventHandler) StartCacheEvictor(ctx context.Context) {
	go h.evictExpiredCaches(ctx)
}

// SetLocker 設定 VaultLocker（在 main.go 組裝時呼叫）
func (h *SyncEventHandler) SetLocker(locker VaultLocker) {
	h.locker = locker
}

// evictExpiredCaches 定期清理過期的 resolverCache 和 docPathIndex，防止記憶體洩漏
func (h *SyncEventHandler) evictExpiredCaches(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			h.mu.Lock()
			for uid, entry := range h.resolverCache {
				if now.After(entry.expiresAt) {
					delete(h.resolverCache, uid)
				}
			}
			for uid, entry := range h.docPathIndex {
				if now.After(entry.expiresAt) {
					delete(h.docPathIndex, uid)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *SyncEventHandler) HandleEvent(ctx context.Context, event SyncEvent) error {
	if h.locker != nil && h.locker.IsLocked(event.UserID) {
		return ErrVaultLocked
	}

	col := strings.ToLower(event.Collection)

	// item 事件：需先讀取 item 判斷 itemType 再決定是否清除 PathResolver
	if col == "item" {
		return h.handleItemEvent(ctx, event)
	}

	if col == "folder" {
		h.InvalidateResolver(event.UserID)
		h.InvalidateDocPathIndex(event.UserID)
	}

	switch strings.ToLower(event.Action) {
	case "delete":
		return h.deleteByDocID(ctx, event.UserID, event.Collection, event.DocID)
	case "create", "update":
		return h.exportByDocID(ctx, event.UserID, event.Collection, event.DocID)
	default:
		return nil
	}
}

// handleItemEvent 處理 collection="item" 的統一事件
func (h *SyncEventHandler) handleItemEvent(ctx context.Context, event SyncEvent) error {
	action := strings.ToLower(event.Action)

	if action == "delete" {
		return h.deleteItemByDocID(ctx, event.UserID, event.DocID)
	}

	item, err := h.reader.GetItem(ctx, event.UserID, event.DocID)
	if err != nil {
		return err
	}
	if item == nil {
		return nil
	}

	// 資料夾類型變更時清除 PathResolver 快取
	if model.IsFolder(item.Type) {
		h.InvalidateResolver(event.UserID)
		h.InvalidateDocPathIndex(event.UserID)
	}

	resolver, err := h.getResolver(ctx, event.UserID)
	if err != nil {
		return err
	}
	exporter := mirror.NewExporter(h.fs, resolver)

	result, err := exporter.ExportItem(event.UserID, item)
	if err != nil {
		return err
	}

	// 使用 ExportItem 回傳的實際路徑更新 docPathIndex（含衝突後綴）
	h.setDocPath(event.UserID, item.ID, result.Path, result.IsFolder)
	return nil
}

// deleteItemByDocID 刪除 item 對應的 EFS 檔案（先查索引，找不到才全量掃描）
func (h *SyncEventHandler) deleteItemByDocID(ctx context.Context, userID, docID string) error {
	target, isFolder, err := h.findPathByDocID(ctx, userID, docID)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
	}
	if isFolder {
		h.InvalidateResolver(userID)
		h.removeDocPath(userID, docID)
		return h.fs.RemoveAll(target)
	}
	h.removeDocPath(userID, docID)
	return h.fs.Remove(target)
}

func (h *SyncEventHandler) exportByDocID(ctx context.Context, userID, collection, docID string) error {
	resolver, err := h.getResolver(ctx, userID)
	if err != nil {
		return err
	}
	exporter := mirror.NewExporter(h.fs, resolver)

	switch strings.ToLower(collection) {
	case "folder":
		f, err := h.reader.GetFolder(ctx, userID, docID)
		if err != nil || f == nil {
			return err
		}
		meta := toFolderMeta(f)
		if err := exporter.ExportFolder(userID, meta); err != nil {
			return err
		}
		if p, err := resolver.ResolveFolderPath(meta.ID); err == nil {
			h.setDocPath(userID, meta.ID, filepath.Join(userID, p), true)
		}
		return nil
	case "note":
		n, err := h.reader.GetNote(ctx, userID, docID)
		if err != nil || n == nil {
			return err
		}
		meta := toNoteMeta(n)
		if err := exporter.ExportNote(userID, meta, n.GetContent()); err != nil {
			return err
		}
		if p, err := resolver.ResolveNotePath(meta.Title, meta.ParentID); err == nil {
			h.setDocPath(userID, meta.ID, filepath.Join(userID, p), false)
		}
		return nil
	case "card":
		c, err := h.reader.GetCard(ctx, userID, docID)
		if err != nil || c == nil {
			return err
		}
		meta := toCardMeta(c)
		if err := exporter.ExportCard(userID, meta); err != nil {
			return err
		}
		if p, err := resolver.ResolveCardPath(meta.Name, meta.ParentID); err == nil {
			h.setDocPath(userID, meta.ID, filepath.Join(userID, p), false)
		}
		return nil
	case "chart":
		c, err := h.reader.GetChart(ctx, userID, docID)
		if err != nil || c == nil {
			return err
		}
		meta := toChartMeta(c)
		if err := exporter.ExportChart(userID, meta); err != nil {
			return err
		}
		if p, err := resolver.ResolveChartPath(meta.Name, meta.ParentID); err == nil {
			h.setDocPath(userID, meta.ID, filepath.Join(userID, p), false)
		}
		return nil
	default:
		return nil
	}
}

func (h *SyncEventHandler) deleteByDocID(ctx context.Context, userID, collection, docID string) error {
	target, isFolder, err := h.findPathByDocID(ctx, userID, docID)
	if err != nil {
		return err
	}
	if target == "" {
		return nil
	}
	if isFolder {
		h.removeDocPath(userID, docID)
		return h.fs.RemoveAll(target)
	}
	h.removeDocPath(userID, docID)
	return h.fs.Remove(target)
}

// getResolver 取得或建立用戶的 PathResolver（帶 TTL 快取）
func (h *SyncEventHandler) getResolver(ctx context.Context, userID string) (*mirror.PathResolver, error) {
	h.mu.Lock()
	if entry, ok := h.resolverCache[userID]; ok && time.Now().Before(entry.expiresAt) {
		r := entry.resolver
		h.mu.Unlock()
		return r, nil
	}
	h.mu.Unlock()

	items, err := h.reader.ListItemFolders(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list item folders: %w", err)
	}
	resolver := buildPathResolverFromItems(items)

	h.mu.Lock()
	h.resolverCache[userID] = &resolverEntry{resolver: resolver, expiresAt: time.Now().Add(resolverCacheTTL)}
	h.mu.Unlock()

	return resolver, nil
}

// InvalidateResolver 強制清除指定用戶的 PathResolver 快取（folder 變更時呼叫）
func (h *SyncEventHandler) InvalidateResolver(userID string) {
	h.mu.Lock()
	delete(h.resolverCache, userID)
	h.mu.Unlock()
}

// InvalidateDocPathIndex 清除指定用戶 docID->path 索引。
func (h *SyncEventHandler) InvalidateDocPathIndex(userID string) {
	h.mu.Lock()
	delete(h.docPathIndex, userID)
	h.mu.Unlock()
}

func (h *SyncEventHandler) setDocPath(userID, docID, path string, isFolder bool) {
	if userID == "" || docID == "" || path == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.docPathIndex[userID]
	if !ok || time.Now().After(entry.expiresAt) {
		entry = &docPathIndexEntry{
			entries:   make(map[string]docPathEntry),
			expiresAt: time.Now().Add(docPathIndexTTL),
		}
		h.docPathIndex[userID] = entry
	}
	entry.entries[docID] = docPathEntry{path: path, isFolder: isFolder}
}

func (h *SyncEventHandler) removeDocPath(userID, docID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry, ok := h.docPathIndex[userID]; ok {
		delete(entry.entries, docID)
	}
}

func (h *SyncEventHandler) lookupDocPath(userID, docID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.docPathIndex[userID]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		delete(h.docPathIndex, userID)
		return "", false
	}
	if p, ok := entry.entries[docID]; ok {
		return p.path, p.isFolder
	}
	return "", false
}

func (h *SyncEventHandler) rebuildDocPathIndex(ctx context.Context, userID string) error {
	next := make(map[string]docPathEntry)
	root := userID
	walkErr := h.fs.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, rErr := h.fs.ReadFile(path)
		if rErr != nil {
			return nil
		}
		if strings.HasSuffix(path, "_folder.json") {
			meta, jErr := mirror.JSONToFolder(data)
			if jErr == nil && meta.ID != "" {
				next[meta.ID] = docPathEntry{path: filepath.Dir(path), isFolder: true}
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			meta, _, pErr := mirror.MarkdownToNote(string(data))
			if pErr == nil && meta.ID != "" {
				next[meta.ID] = docPathEntry{path: path, isFolder: false}
			}
			return nil
		}
		if strings.HasSuffix(path, ".json") {
			// 優先嘗試新格式（含 itemType）
			if mirrorItem, err := mirror.MirrorJSONToItem(data); err == nil {
				if model.IsFolder(mirrorItem.ItemType) {
					next[mirrorItem.ID] = docPathEntry{path: filepath.Dir(path), isFolder: true}
				} else {
					next[mirrorItem.ID] = docPathEntry{path: path, isFolder: false}
				}
				return nil
			}
			// 退回舊格式
			var card map[string]any
			if jErr := json.Unmarshal(data, &card); jErr == nil {
				if id, ok := card["id"].(string); ok && id != "" {
					next[id] = docPathEntry{path: path, isFolder: false}
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	h.mu.Lock()
	h.docPathIndex[userID] = &docPathIndexEntry{
		entries:   next,
		expiresAt: time.Now().Add(docPathIndexTTL),
	}
	h.mu.Unlock()
	return nil
}

func (h *SyncEventHandler) findPathByDocID(ctx context.Context, userID, docID string) (string, bool, error) {
	if p, isFolder := h.lookupDocPath(userID, docID); p != "" {
		return p, isFolder, nil
	}
	if err := h.rebuildDocPathIndex(ctx, userID); err != nil {
		return "", false, err
	}
	p, isFolder := h.lookupDocPath(userID, docID)
	return p, isFolder, nil
}

func buildPathResolverFromItems(items []*model.Item) *mirror.PathResolver {
	nodes := make([]mirror.FolderNode, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		nodes = append(nodes, mirror.FolderNode{
			ID:         item.ID,
			FolderName: item.GetName(),
			Type:       mirror.ItemFolderType(item),
			ParentID:   model.StrPtrField(item.Fields, "parentID"),
		})
	}
	return mirror.NewPathResolver(nodes)
}

func toFolderMeta(f *model.Folder) mirror.FolderMeta {
	meta := mirror.FolderMeta{
		ID:                  f.ID,
		FolderName:          f.FolderName,
		Type:                f.Type,
		ParentID:            f.ParentID,
		OrderAt:             f.OrderAt,
		Icon:                f.Icon,
		CreatedAt:           f.CreatedAt,
		UpdatedAt:           f.UpdatedAt,
		USN:                 f.Usn,
		NoteNum:             f.NoteNum,
		IsTemp:              f.IsTemp,
		FolderSummary:       f.FolderSummary,
		AiFolderName:        f.AiFolderName,
		AiFolderSummary:     f.AiFolderSummary,
		AiInstruction:       f.AiInstruction,
		AutoUpdateSummary:   f.AutoUpdateSummary,
		IsSummarizedNoteIds: f.IsSummarizedNoteIds,
		TemplateHTML:        f.TemplateHTML,
		TemplateCSS:         f.TemplateCSS,
		UIPrompt:            f.UIPrompt,
		IsShared:            f.IsShared,
		Searchable:          f.Searchable,
		AllowContribute:     f.AllowContribute,
		ChartKind:           f.ChartKind,
	}
	for _, idx := range f.Indexes {
		if idx == nil {
			continue
		}
		meta.Indexes = append(meta.Indexes, mirror.IndexMeta{
			Name: idx.Name, Notes: idx.Notes, IsReserved: idx.IsReserved,
		})
	}
	for _, fd := range f.Fields {
		if fd == nil {
			continue
		}
		meta.Fields = append(meta.Fields, mirror.CardFieldMeta{
			Name: fd.Name, Type: fd.Type, Options: fd.Options,
		})
	}
	for _, th := range f.TemplateHistory {
		if th == nil {
			continue
		}
		meta.TemplateHistory = append(meta.TemplateHistory, mirror.TemplateHistoryMeta{
			HTML: th.HTML, CSS: th.CSS, Timestamp: th.Timestamp,
		})
	}
	for _, s := range f.Sharers {
		if s == nil {
			continue
		}
		meta.Sharers = append(meta.Sharers, mirror.SharerMeta{
			UserID: s.UserID, Role: s.Role,
		})
	}
	return meta
}

func toNoteMeta(n *model.Note) mirror.NoteMeta {
	parent := ""
	if n.ParentID != nil {
		parent = *n.ParentID
	} else {
		parent = n.FolderID
	}
	meta := mirror.NoteMeta{
		ID:        n.ID,
		ParentID:  parent,
		FolderID:  n.FolderID,
		Title:     n.GetTitle(),
		Type:      n.Type,
		USN:       n.Usn,
		Tags:      n.Tags,
		CreatedAt: fmt.Sprintf("%d", n.CreateAt),
		UpdatedAt: fmt.Sprintf("%d", n.UpdateAt),
		IsNew:     n.IsNew,
		AiTags:    n.AiTags,
		ImgURLs:   n.ImgURLs,
	}
	if n.OrderAt != nil {
		meta.OrderAt = *n.OrderAt
	}
	if n.Status != nil {
		meta.Status = *n.Status
	}
	if n.AiTitle != nil {
		meta.AiTitle = *n.AiTitle
	}
	return meta
}

func toCardMeta(c *model.Card) mirror.CardMeta {
	return mirror.CardMeta{
		ID:            c.ID,
		ContributorID: c.ContributorID,
		ParentID:      c.ParentID,
		Name:          c.Name,
		Fields:        c.Fields,
		Reviews:       c.Reviews,
		Coordinates:   c.Coordinates,
		OrderAt:       c.OrderAt,
		IsDeleted:     c.IsDeleted,
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
		USN:           c.Usn,
	}
}

func toChartMeta(c *model.Chart) mirror.CardMeta {
	return mirror.CardMeta{
		ID:       c.ID,
		ParentID: c.ParentID,
		Name:      c.Name,
		Fields:    c.Data,
		IsDeleted: c.IsDeleted,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
		USN:       c.Usn,
	}
}
