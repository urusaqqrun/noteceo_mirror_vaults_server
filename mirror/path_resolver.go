package mirror

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// FolderNode 精簡的 Folder 結構，用於路徑解析
type FolderNode struct {
	ID         string
	FolderName string
	Type       string
	ParentID   *string
}

// PathResolver 根據 parentID 鏈條解析 Vault 檔案路徑（並發安全）
type PathResolver struct {
	mu    sync.RWMutex
	tree  map[string]*FolderNode
	cache map[string]string // folderID → 已解析的路徑
}

func NewPathResolver(folders []FolderNode) *PathResolver {
	r := &PathResolver{
		tree:  make(map[string]*FolderNode, len(folders)),
		cache: make(map[string]string),
	}
	for i := range folders {
		f := folders[i]
		r.tree[f.ID] = &f
	}
	return r
}

// ResolveFolderPath 解析 Folder 在 Vault 中的路徑（不含 vaultRoot）
// 回傳格式: "NOTE/工作/會議紀錄"
// folderID 為空時回傳 "_unsorted"（容許 parentID 缺失的線上資料）
func (r *PathResolver) ResolveFolderPath(folderID string) (string, error) {
	if folderID == "" {
		return "_unsorted", nil
	}

	r.mu.RLock()
	if cached, ok := r.cache[folderID]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	node, ok := r.tree[folderID]
	r.mu.RUnlock()

	if !ok {
		return "_unsorted", nil
	}

	r.mu.RLock()
	parts, err := r.buildPathParts(folderID, make(map[string]bool))
	r.mu.RUnlock()
	if err != nil {
		return "", err
	}

	typeName := resolveType(node.Type)
	result := filepath.Join(append([]string{typeName}, parts...)...)

	r.mu.Lock()
	r.cache[folderID] = result
	r.mu.Unlock()
	return result, nil
}

// ResolveNotePath 解析 Note 在 Vault 中的路徑
func (r *PathResolver) ResolveNotePath(noteTitle string, parentFolderID string) (string, error) {
	folderPath, err := r.ResolveFolderPath(parentFolderID)
	if err != nil {
		return "", fmt.Errorf("resolve note parent: %w", err)
	}
	return filepath.Join(folderPath, sanitizeName(noteTitle)+".md"), nil
}

// ResolveCardPath 解析 Card 在 Vault 中的路徑
func (r *PathResolver) ResolveCardPath(cardName string, parentFolderID string) (string, error) {
	folderPath, err := r.ResolveFolderPath(parentFolderID)
	if err != nil {
		return "", fmt.Errorf("resolve card parent: %w", err)
	}
	return filepath.Join(folderPath, sanitizeName(cardName)+".json"), nil
}

// ResolveChartPath 解析 Chart 在 Vault 中的路徑
func (r *PathResolver) ResolveChartPath(chartName string, parentFolderID string) (string, error) {
	folderPath, err := r.ResolveFolderPath(parentFolderID)
	if err != nil {
		return "", fmt.Errorf("resolve chart parent: %w", err)
	}
	return filepath.Join(folderPath, sanitizeName(chartName)+".json"), nil
}

func (r *PathResolver) AddFolder(folder FolderNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tree[folder.ID] = &folder
	r.invalidateCache()
}

func (r *PathResolver) RemoveFolder(folderID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tree, folderID)
	r.invalidateCache()
}

func (r *PathResolver) UpdateFolder(folder FolderNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tree[folder.ID] = &folder
	r.invalidateCache()
}

// buildPathParts 遞迴向上取得 folderName 路徑片段（不含 type 前綴）
func (r *PathResolver) buildPathParts(folderID string, visited map[string]bool) ([]string, error) {
	if visited[folderID] {
		return nil, fmt.Errorf("circular reference detected at folder %q", folderID)
	}
	visited[folderID] = true

	node, ok := r.tree[folderID]
	if !ok {
		return nil, fmt.Errorf("folder %q not found in tree", folderID)
	}

	name := sanitizeName(node.FolderName)

	if node.ParentID == nil || *node.ParentID == "" {
		return []string{name}, nil
	}

	parentParts, err := r.buildPathParts(*node.ParentID, visited)
	if err != nil {
		return nil, err
	}

	return append(parentParts, name), nil
}

func (r *PathResolver) invalidateCache() {
	r.cache = make(map[string]string)
}

// resolveType 回傳 Folder 的 type 目錄名，空字串預設為 NOTE
func resolveType(t string) string {
	switch t {
	case "CARD":
		return "CARD"
	case "CHART":
		return "CHART"
	case "TODO":
		return "TODO"
	default:
		return "NOTE"
	}
}

// sanitizeName 將不安全的檔名字元替換為底線
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	return name
}
