package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
)

// SnapshotUpdater DB 快照更新介面，由 database.PgStore 實作。
// 定義在 mirror 避免循環依賴。
type SnapshotUpdater interface {
	UpsertSnapshotFile(ctx context.Context, memberID, filePath, hash, docID string, mtime int64) error
	DeleteSnapshotFile(ctx context.Context, memberID, filePath string) error
	DeleteSnapshotByPrefix(ctx context.Context, memberID, prefix string) error
	RenameSnapshotPrefix(ctx context.Context, memberID, oldPrefix, newPrefix string) error
}

// SnapshotAwareVaultFS 包裝 VaultFS，在每次寫入/刪除/改名後自動更新 DB 快照。
// 讀取類方法直接委派給 inner，不做任何快照操作。
type SnapshotAwareVaultFS struct {
	inner VaultFS
	db    SnapshotUpdater
}

func NewSnapshotAwareVaultFS(inner VaultFS, db SnapshotUpdater) *SnapshotAwareVaultFS {
	return &SnapshotAwareVaultFS{inner: inner, db: db}
}

// --- 寫入類方法：委派 + 自動更新快照 ---

func (s *SnapshotAwareVaultFS) WriteFile(path string, content []byte) error {
	if err := s.inner.WriteFile(path, content); err != nil {
		return err
	}
	memberID, relPath := splitMemberPath(path)
	if memberID == "" || relPath == "" || isSnapshotIgnored(relPath) {
		return nil
	}

	h := sha256.Sum256(content)
	hash := fmt.Sprintf("%x", h)
	docID := extractDocID(content, relPath)

	var mtime int64
	if info, err := s.inner.Stat(path); err == nil {
		mtime = info.ModTime().UnixMilli()
	}

	if err := s.db.UpsertSnapshotFile(context.Background(), memberID, relPath, hash, docID, mtime); err != nil {
		log.Printf("[SnapshotFS] upsert failed: member=%s path=%s err=%v", memberID, relPath, err)
	}
	return nil
}

func (s *SnapshotAwareVaultFS) Remove(path string) error {
	if err := s.inner.Remove(path); err != nil {
		return err
	}
	memberID, relPath := splitMemberPath(path)
	if memberID == "" || relPath == "" || isSnapshotIgnored(relPath) {
		return nil
	}
	if err := s.db.DeleteSnapshotFile(context.Background(), memberID, relPath); err != nil {
		log.Printf("[SnapshotFS] delete failed: member=%s path=%s err=%v", memberID, relPath, err)
	}
	return nil
}

func (s *SnapshotAwareVaultFS) RemoveAll(path string) error {
	if err := s.inner.RemoveAll(path); err != nil {
		return err
	}
	memberID, relPath := splitMemberPath(path)
	if memberID == "" {
		return nil
	}
	if err := s.db.DeleteSnapshotByPrefix(context.Background(), memberID, relPath); err != nil {
		log.Printf("[SnapshotFS] deletePrefix failed: member=%s prefix=%s err=%v", memberID, relPath, err)
	}
	return nil
}

func (s *SnapshotAwareVaultFS) Rename(oldPath, newPath string) error {
	if err := s.inner.Rename(oldPath, newPath); err != nil {
		return err
	}
	oldMember, oldRel := splitMemberPath(oldPath)
	newMember, newRel := splitMemberPath(newPath)
	if oldMember == "" || oldRel == "" || newMember == "" || newRel == "" {
		return nil
	}
	if oldMember == newMember {
		if err := s.db.RenameSnapshotPrefix(context.Background(), oldMember, oldRel, newRel); err != nil {
			log.Printf("[SnapshotFS] rename failed: member=%s old=%s new=%s err=%v", oldMember, oldRel, newRel, err)
		}
	}
	return nil
}

// --- 讀取類方法：直接委派 ---

func (s *SnapshotAwareVaultFS) ReadFile(path string) ([]byte, error) { return s.inner.ReadFile(path) }
func (s *SnapshotAwareVaultFS) MkdirAll(path string) error           { return s.inner.MkdirAll(path) }
func (s *SnapshotAwareVaultFS) Exists(path string) bool              { return s.inner.Exists(path) }
func (s *SnapshotAwareVaultFS) ListDir(path string) ([]fs.DirEntry, error) {
	return s.inner.ListDir(path)
}
func (s *SnapshotAwareVaultFS) Stat(path string) (fs.FileInfo, error) { return s.inner.Stat(path) }
func (s *SnapshotAwareVaultFS) Walk(root string, fn filepath.WalkFunc) error {
	return s.inner.Walk(root, fn)
}

// --- helpers ---

func splitMemberPath(path string) (memberID, relPath string) {
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}

func isSnapshotIgnored(relPath string) bool {
	if relPath == "CLAUDE.md" || relPath == ".vault_initialized" {
		return true
	}
	if strings.HasPrefix(relPath, ".CubeLV/") {
		return true
	}
	// plugins/ 由 Git 管理，不追蹤 snapshot
	if strings.HasPrefix(relPath, "plugins/") {
		return true
	}
	return false
}

func extractDocID(content []byte, filePath string) string {
	if !strings.HasSuffix(filePath, ".json") {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(content, &doc) == nil {
		if id, ok := doc["id"].(string); ok {
			return id
		}
	}
	return ""
}
