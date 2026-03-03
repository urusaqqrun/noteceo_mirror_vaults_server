package mirror

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// VaultFS 檔案系統抽象介面，方便 mock 測試
type VaultFS interface {
	WriteFile(path string, content []byte) error
	ReadFile(path string) ([]byte, error)
	MkdirAll(path string) error
	Remove(path string) error
	RemoveAll(path string) error
	Rename(oldPath, newPath string) error
	Exists(path string) bool
	ListDir(path string) ([]fs.DirEntry, error)
	Stat(path string) (fs.FileInfo, error)
	Walk(root string, fn filepath.WalkFunc) error
}

// RealVaultFS EFS / 本地檔案系統實作
type RealVaultFS struct {
	Root string
}

func (r *RealVaultFS) abs(path string) string { return filepath.Join(r.Root, path) }

func (r *RealVaultFS) WriteFile(path string, content []byte) error {
	abs := r.abs(path)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	return os.WriteFile(abs, content, 0644)
}

func (r *RealVaultFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(r.abs(path))
}

func (r *RealVaultFS) MkdirAll(path string) error {
	return os.MkdirAll(r.abs(path), 0755)
}

func (r *RealVaultFS) Remove(path string) error {
	return os.Remove(r.abs(path))
}

func (r *RealVaultFS) RemoveAll(path string) error {
	return os.RemoveAll(r.abs(path))
}

func (r *RealVaultFS) Rename(oldPath, newPath string) error {
	newAbs := r.abs(newPath)
	if err := os.MkdirAll(filepath.Dir(newAbs), 0755); err != nil {
		return err
	}
	return os.Rename(r.abs(oldPath), newAbs)
}

func (r *RealVaultFS) Exists(path string) bool {
	_, err := os.Stat(r.abs(path))
	return err == nil
}

func (r *RealVaultFS) ListDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(r.abs(path))
}

func (r *RealVaultFS) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(r.abs(path))
}

func (r *RealVaultFS) Walk(root string, fn filepath.WalkFunc) error {
	return filepath.Walk(r.abs(root), func(path string, info fs.FileInfo, err error) error {
		rel, _ := filepath.Rel(r.Root, path)
		return fn(rel, info, err)
	})
}

// MemoryVaultFS 記憶體實作（用於測試）
type MemoryVaultFS struct {
	mu    sync.RWMutex
	files map[string][]byte
	dirs  map[string]bool
}

func NewMemoryVaultFS() *MemoryVaultFS {
	return &MemoryVaultFS{
		files: make(map[string][]byte),
		dirs:  make(map[string]bool),
	}
}

func (m *MemoryVaultFS) WriteFile(path string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureDirLocked(filepath.Dir(path))
	dst := make([]byte, len(content))
	copy(dst, content)
	m.files[path] = dst
	return nil
}

func (m *MemoryVaultFS) ReadFile(path string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	dst := make([]byte, len(data))
	copy(dst, data)
	return dst, nil
}

func (m *MemoryVaultFS) MkdirAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureDirLocked(path)
	return nil
}

func (m *MemoryVaultFS) Remove(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, path)
	delete(m.dirs, path)
	return nil
}

func (m *MemoryVaultFS) RemoveAll(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.files {
		if k == path || strings.HasPrefix(k, path+"/") {
			delete(m.files, k)
		}
	}
	for k := range m.dirs {
		if k == path || strings.HasPrefix(k, path+"/") {
			delete(m.dirs, k)
		}
	}
	return nil
}

func (m *MemoryVaultFS) Rename(oldPath, newPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 移動檔案
	if data, ok := m.files[oldPath]; ok {
		m.ensureDirLocked(filepath.Dir(newPath))
		m.files[newPath] = data
		delete(m.files, oldPath)
		return nil
	}

	// 移動目錄（包含所有子檔案/子目錄）
	if m.dirs[oldPath] {
		prefix := oldPath + "/"
		for k, v := range m.files {
			if strings.HasPrefix(k, prefix) {
				newKey := newPath + "/" + strings.TrimPrefix(k, prefix)
				m.files[newKey] = v
				delete(m.files, k)
			}
		}
		for k := range m.dirs {
			if k == oldPath || strings.HasPrefix(k, prefix) {
				newKey := newPath + strings.TrimPrefix(k, oldPath)
				m.dirs[newKey] = true
				delete(m.dirs, k)
			}
		}
		return nil
	}

	return fmt.Errorf("not found: %s", oldPath)
}

func (m *MemoryVaultFS) Exists(path string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.files[path]; ok {
		return true
	}
	return m.dirs[path]
}

func (m *MemoryVaultFS) ListDir(path string) ([]fs.DirEntry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	prefix := path + "/"
	if path == "" || path == "." {
		prefix = ""
	}

	seen := make(map[string]bool)
	var entries []fs.DirEntry

	for k := range m.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		if seen[name] {
			continue
		}
		seen[name] = true
		isDir := len(parts) > 1
		entries = append(entries, &memDirEntry{name: name, isDir: isDir})
	}
	for k := range m.dirs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		parts := strings.SplitN(rest, "/", 2)
		name := parts[0]
		if seen[name] || name == "" {
			continue
		}
		seen[name] = true
		entries = append(entries, &memDirEntry{name: name, isDir: true})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (m *MemoryVaultFS) Stat(path string) (fs.FileInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if data, ok := m.files[path]; ok {
		return &memFileInfo{name: filepath.Base(path), size: int64(len(data)), isDir: false}, nil
	}
	if m.dirs[path] {
		return &memFileInfo{name: filepath.Base(path), size: 0, isDir: true}, nil
	}
	return nil, fmt.Errorf("not found: %s", path)
}

func (m *MemoryVaultFS) Walk(root string, fn filepath.WalkFunc) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	prefix := root + "/"
	if root == "" || root == "." {
		prefix = ""
	}

	var paths []string
	for k := range m.files {
		if strings.HasPrefix(k, prefix) || root == "" || root == "." {
			paths = append(paths, k)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		info := &memFileInfo{name: filepath.Base(p), size: int64(len(m.files[p])), isDir: false}
		if err := fn(p, info, nil); err != nil {
			return err
		}
	}
	return nil
}

func (m *MemoryVaultFS) ensureDirLocked(path string) {
	parts := strings.Split(path, "/")
	for i := range parts {
		if parts[i] == "" || parts[i] == "." {
			continue
		}
		dir := strings.Join(parts[:i+1], "/")
		m.dirs[dir] = true
	}
}

// memDirEntry implements fs.DirEntry for testing
type memDirEntry struct {
	name  string
	isDir bool
}

func (e *memDirEntry) Name() string               { return e.name }
func (e *memDirEntry) IsDir() bool                 { return e.isDir }
func (e *memDirEntry) Type() fs.FileMode           { if e.isDir { return fs.ModeDir }; return 0 }
func (e *memDirEntry) Info() (fs.FileInfo, error)   { return &memFileInfo{name: e.name, isDir: e.isDir}, nil }

// memFileInfo implements fs.FileInfo for testing
type memFileInfo struct {
	name  string
	size  int64
	isDir bool
}

func (i *memFileInfo) Name() string      { return i.name }
func (i *memFileInfo) Size() int64       { return i.size }
func (i *memFileInfo) Mode() fs.FileMode { if i.isDir { return fs.ModeDir | 0755 }; return 0644 }
func (i *memFileInfo) ModTime() time.Time { return time.Now() }
func (i *memFileInfo) IsDir() bool       { return i.isDir }
func (i *memFileInfo) Sys() interface{}  { return nil }
