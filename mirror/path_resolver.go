package mirror

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// TreeNode 精簡的 Item 結構，用於路徑解析。
type TreeNode struct {
	ID       string
	Name     string
	ItemType string
	ParentID *string
}

// PathResolver 根據完整 item parent 鏈條解析 Vault 路徑（並發安全）。
type PathResolver struct {
	mu    sync.RWMutex
	tree  map[string]*TreeNode
	cache map[string]string // itemID → 已解析的容器路徑
}

var errNodeNotFoundInTree = errors.New("node not found in tree")

func NewPathResolver(nodes []TreeNode) *PathResolver {
	r := &PathResolver{
		tree:  make(map[string]*TreeNode, len(nodes)),
		cache: make(map[string]string),
	}
	for i := range nodes {
		node := nodes[i]
		r.tree[node.ID] = &node
	}
	return r
}

// ResolvePath 解析 item 作為子項容器時的目錄路徑（不含 vaultRoot）。
// 成功回傳格式：`NOTE/工作`、`NOTE/工作/筆記A`。
// 找不到、circular ref 等異常一律回傳 error，由呼叫端決定 fallback 位置。
func (r *PathResolver) ResolvePath(itemID string) (string, error) {
	if itemID == "" {
		return "", fmt.Errorf("empty item ID")
	}

	r.mu.RLock()
	if cached, ok := r.cache[itemID]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	_, ok := r.tree[itemID]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %q", errNodeNotFoundInTree, itemID)
	}

	r.mu.RLock()
	parts, err := r.buildPathParts(itemID, make(map[string]bool))
	r.mu.RUnlock()
	if err != nil {
		return "", err
	}

	result := filepath.Join(parts...)

	r.mu.Lock()
	r.cache[itemID] = result
	r.mu.Unlock()
	return result, nil
}

func (r *PathResolver) AddNode(node TreeNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tree[node.ID] = &node
	r.invalidateCache()
}

func (r *PathResolver) RemoveNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tree, nodeID)
	r.invalidateCache()
}

func (r *PathResolver) UpdateNode(node TreeNode) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tree[node.ID] = &node
	r.invalidateCache()
}

// buildPathParts 遞迴向上取得 item 容器路徑片段。
// 命名規則：有 name → sanitizeName(name) + "_" + id；無 name → id
func (r *PathResolver) buildPathParts(nodeID string, visited map[string]bool) ([]string, error) {
	if visited[nodeID] {
		return nil, fmt.Errorf("circular reference detected at node %q", nodeID)
	}
	visited[nodeID] = true

	node, ok := r.tree[nodeID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", errNodeNotFoundInTree, nodeID)
	}

	name := buildNameWithID(node.Name, node.ID)

	if node.ParentID == nil || *node.ParentID == "" {
		typeName := resolveTypeFromItemType(node.ItemType)
		return []string{typeName, name}, nil
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

// resolveTypeFromItemType 將 itemType 對應到 Vault 根目錄。
func resolveTypeFromItemType(itemType string) string {
	if itemType == "" {
		return "NOTE"
	}
	if strings.HasSuffix(itemType, "_FOLDER") {
		return strings.TrimSuffix(itemType, "_FOLDER")
	}
	return itemType
}

// SanitizeItemName 將不安全的檔名字元替換為底線（exported 供其他 package 使用）
func SanitizeItemName(name string) string {
	return sanitizeName(name)
}

// buildNameWithID 產生目錄名或檔案 basename（不含 .json）。
// 有 name → sanitizeName(name) + "_" + id；無 name → id
func buildNameWithID(name, id string) string {
	if name == "" || name == id {
		return id
	}
	return sanitizeName(name) + "_" + id
}

// BuildFileNameWithID 產生 vault 檔案名稱（含 .json）。exported 供外部使用。
func BuildFileNameWithID(name, id string) string {
	return buildNameWithID(name, id) + ".json"
}

func sanitizeName(name string) string {
	if name == "" {
		return "_unnamed"
	}
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	// 移除控制字元（U+0000 ~ U+001F, U+007F）
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r != 0x7F {
			b.WriteRune(r)
		}
	}
	name = b.String()
	if name == "" {
		return "_unnamed"
	}
	// 防止路徑穿越
	if name == "." || name == ".." {
		return "_" + name
	}
	return name
}
