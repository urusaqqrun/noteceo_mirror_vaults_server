package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
)

var (
	errFound      = errors.New("found")
	ErrVaultLocked = errors.New("user vault is locked by active AI task")
)

// VaultLocker 查詢 Vault 鎖定狀態（避免循環依賴 executor 套件）
type VaultLocker interface {
	IsLocked(userId string) bool
}

// DataReader 事件處理所需的 Mongo 讀取能力
// 由 database 層提供實作，測試可用 mock。
type DataReader interface {
	ListFolders(ctx context.Context, userID string) ([]*model.Folder, error)
	GetFolder(ctx context.Context, userID, folderID string) (*model.Folder, error)
	GetNote(ctx context.Context, userID, noteID string) (*model.Note, error)
	GetCard(ctx context.Context, userID, cardID string) (*model.Card, error)
	GetChart(ctx context.Context, userID, chartID string) (*model.Chart, error)
}

// resolverEntry PathResolver 快取項目
type resolverEntry struct {
	resolver  *mirror.PathResolver
	expiresAt time.Time
}

const resolverCacheTTL = 30 * time.Second

// SyncEventHandler 將同步事件轉為 Vault 匯出動作
type SyncEventHandler struct {
	fs     mirror.VaultFS
	reader DataReader
	locker VaultLocker // nil 時不檢查鎖定

	mu             sync.Mutex
	resolverCache  map[string]*resolverEntry
}

func NewSyncEventHandler(fs mirror.VaultFS, reader DataReader) *SyncEventHandler {
	return &SyncEventHandler{fs: fs, reader: reader, resolverCache: make(map[string]*resolverEntry)}
}

// SetLocker 設定 VaultLocker（在 main.go 組裝時呼叫）
func (h *SyncEventHandler) SetLocker(locker VaultLocker) {
	h.locker = locker
}

func (h *SyncEventHandler) HandleEvent(ctx context.Context, event SyncEvent) error {
	// AI 任務執行中，拒絕寫入該用戶的 Vault，讓訊息留在 PEL 稍後重試
	if h.locker != nil && h.locker.IsLocked(event.UserID) {
		return ErrVaultLocked
	}

	// folder 變更時清除 PathResolver 快取
	if strings.ToLower(event.Collection) == "folder" {
		h.InvalidateResolver(event.UserID)
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
		return exporter.ExportFolder(userID, toFolderMeta(f))
	case "note":
		n, err := h.reader.GetNote(ctx, userID, docID)
		if err != nil || n == nil {
			return err
		}
		return exporter.ExportNote(userID, toNoteMeta(n), n.GetContent())
	case "card":
		c, err := h.reader.GetCard(ctx, userID, docID)
		if err != nil || c == nil {
			return err
		}
		return exporter.ExportCard(userID, toCardMeta(c))
	case "chart":
		c, err := h.reader.GetChart(ctx, userID, docID)
		if err != nil || c == nil {
			return err
		}
		return exporter.ExportChart(userID, toChartMeta(c))
	default:
		return nil
	}
}

func (h *SyncEventHandler) deleteByDocID(ctx context.Context, userID, collection, docID string) error {
	root := userID
	var target string
	var isFolder bool

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

		switch strings.ToLower(collection) {
		case "note":
			if !strings.HasSuffix(path, ".md") {
				return nil
			}
			meta, _, parseErr := mirror.MarkdownToNote(string(data))
			if parseErr != nil {
				log.Printf("[deleteByDocID] parse md error (%s): %v", path, parseErr)
				return nil
			}
			if meta.ID == docID {
				target = path
				return errFound
			}
		case "folder":
			if !strings.HasSuffix(path, "_folder.json") {
				return nil
			}
			meta, jErr := mirror.JSONToFolder(data)
			if jErr != nil {
				log.Printf("[deleteByDocID] parse folder json error (%s): %v", path, jErr)
				return nil
			}
			if meta.ID == docID {
				target = filepath.Dir(path)
				isFolder = true
				return errFound
			}
		case "card", "chart":
			if !strings.HasSuffix(path, ".json") || strings.HasSuffix(path, "_folder.json") {
				return nil
			}
			var card map[string]any
			if uErr := json.Unmarshal(data, &card); uErr != nil {
				log.Printf("[deleteByDocID] parse json error (%s): %v", path, uErr)
				return nil
			}
			if id, ok := card["id"].(string); ok && id == docID {
				target = path
				return errFound
			}
		}
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, errFound) {
		return walkErr
	}

	if target == "" {
		return nil
	}
	if isFolder {
		return h.fs.RemoveAll(target)
	}
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

	folders, err := h.reader.ListFolders(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	resolver := buildPathResolver(folders)

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

func buildPathResolver(folders []*model.Folder) *mirror.PathResolver {
	nodes := make([]mirror.FolderNode, 0, len(folders))
	for _, f := range folders {
		if f == nil {
			continue
		}
		nodes = append(nodes, mirror.FolderNode{ID: f.ID, FolderName: f.FolderName, Type: f.GetType(), ParentID: f.ParentID})
	}
	return mirror.NewPathResolver(nodes)
}

func toFolderMeta(f *model.Folder) mirror.FolderMeta {
	return mirror.FolderMeta{
		ID:         f.ID,
		MemberID:   f.MemberID,
		FolderName: f.FolderName,
		Type:       f.Type,
		ParentID:   f.ParentID,
		OrderAt:    f.OrderAt,
		Icon:       f.Icon,
		CreatedAt:  f.CreatedAt,
		UpdatedAt:  f.UpdatedAt,
		USN:        f.Usn,
		NoteNum:    f.NoteNum,
	}
}

func toNoteMeta(n *model.Note) mirror.NoteMeta {
	parent := ""
	if n.ParentID != nil {
		parent = *n.ParentID
	} else {
		parent = n.FolderID
	}
	title := n.GetTitle()
	return mirror.NoteMeta{
		ID:        n.ID,
		ParentID:  parent,
		Title:     title,
		USN:       n.Usn,
		Tags:      n.Tags,
		CreatedAt: fmt.Sprintf("%d", n.CreateAt),
		UpdatedAt: fmt.Sprintf("%d", n.UpdateAt),
	}
}

func toCardMeta(c *model.Card) mirror.CardMeta {
	return mirror.CardMeta{
		ID:        c.ID,
		MemberID:  c.MemberID,
		ParentID:  c.ParentID,
		Name:      c.Name,
		Fields:    c.Fields,
		Reviews:   c.Reviews,
		OrderAt:   c.OrderAt,
		IsDeleted: c.IsDeleted,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
		USN:       c.Usn,
	}
}

func toChartMeta(c *model.Chart) mirror.CardMeta {
	return mirror.CardMeta{
		ID:        c.ID,
		MemberID:  c.MemberID,
		ParentID:  c.ParentID,
		Name:      c.Name,
		Fields:    c.Data,
		IsDeleted: c.IsDeleted,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
		USN:       c.Usn,
	}
}
