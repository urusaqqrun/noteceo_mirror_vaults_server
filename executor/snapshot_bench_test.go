package executor

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	log.SetOutput(io.Discard)
}

// benchVaultFS 具有穩定 modtime 的記憶體 FS，用於 benchmark
type benchVaultFS struct {
	mu    sync.RWMutex
	files map[string][]byte
	mtime map[string]time.Time
}

func newBenchVaultFS() *benchVaultFS {
	return &benchVaultFS{
		files: make(map[string][]byte),
		mtime: make(map[string]time.Time),
	}
}

func (b *benchVaultFS) WriteFile(path string, content []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	dst := make([]byte, len(content))
	copy(dst, content)
	b.files[path] = dst
	b.mtime[path] = time.Now()
	return nil
}

func (b *benchVaultFS) ReadFile(path string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	data, ok := b.files[path]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	dst := make([]byte, len(data))
	copy(dst, data)
	return dst, nil
}

func (b *benchVaultFS) MkdirAll(_ string) error                            { return nil }
func (b *benchVaultFS) Remove(path string) error                           { delete(b.files, path); return nil }
func (b *benchVaultFS) RemoveAll(_ string) error                           { return nil }
func (b *benchVaultFS) Rename(_, _ string) error                           { return nil }
func (b *benchVaultFS) Exists(path string) bool                            { _, ok := b.files[path]; return ok }
func (b *benchVaultFS) ListDir(_ string) ([]fs.DirEntry, error)            { return nil, nil }
func (b *benchVaultFS) Stat(path string) (fs.FileInfo, error)              { return nil, nil }

type benchFileInfo struct {
	name  string
	size  int64
	mtime time.Time
}

func (fi *benchFileInfo) Name() string      { return fi.name }
func (fi *benchFileInfo) Size() int64       { return fi.size }
func (fi *benchFileInfo) Mode() fs.FileMode { return 0644 }
func (fi *benchFileInfo) ModTime() time.Time { return fi.mtime }
func (fi *benchFileInfo) IsDir() bool       { return false }
func (fi *benchFileInfo) Sys() interface{}  { return nil }

func (b *benchVaultFS) Walk(root string, fn filepath.WalkFunc) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	prefix := root + "/"
	var paths []string
	for k := range b.files {
		if strings.HasPrefix(k, prefix) {
			paths = append(paths, k)
		}
	}
	sort.Strings(paths)

	for _, p := range paths {
		info := &benchFileInfo{
			name:  filepath.Base(p),
			size:  int64(len(b.files[p])),
			mtime: b.mtime[p],
		}
		if err := fn(p, info, nil); err != nil {
			return err
		}
	}
	return nil
}

// populateBenchFS 填充指定數量的 JSON 檔案
func populateBenchFS(bfs *benchVaultFS, userID string, count int) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	types := []string{"NOTE", "TASK", "EVENT", "LINK"}
	for i := 0; i < count; i++ {
		typeName := types[i%len(types)]
		path := fmt.Sprintf("%s/%s/item_%05d.json", userID, typeName, i)
		doc := map[string]interface{}{
			"id":    fmt.Sprintf("id_%05d", i),
			"title": fmt.Sprintf("Item %d", i),
			"body":  strings.Repeat("x", 200),
		}
		data, _ := json.Marshal(doc)
		bfs.files[path] = data
		bfs.mtime[path] = baseTime.Add(time.Duration(i) * time.Second)
	}
}

// --- Benchmark: TakeSnapshotAndPathIDMap ---

func BenchmarkTakeSnapshot_100(b *testing.B)  { benchTakeSnapshot(b, 100) }
func BenchmarkTakeSnapshot_1k(b *testing.B)   { benchTakeSnapshot(b, 1000) }
func BenchmarkTakeSnapshot_5k(b *testing.B)   { benchTakeSnapshot(b, 5000) }
func BenchmarkTakeSnapshot_10k(b *testing.B)  { benchTakeSnapshot(b, 10000) }

func benchTakeSnapshot(b *testing.B, fileCount int) {
	bfs := newBenchVaultFS()
	populateBenchFS(bfs, "u1", fileCount)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, _, err := TakeSnapshotAndPathIDMap(bfs, "u1")
		if err != nil {
			b.Fatal(err)
		}
		if len(snap) != fileCount {
			b.Fatalf("expected %d files, got %d", fileCount, len(snap))
		}
	}
}

// --- Benchmark: TakeIncrementalSnapshot (no changes) ---

func BenchmarkIncrementalSnapshot_NoChanges_100(b *testing.B)  { benchIncrNoChange(b, 100) }
func BenchmarkIncrementalSnapshot_NoChanges_1k(b *testing.B)   { benchIncrNoChange(b, 1000) }
func BenchmarkIncrementalSnapshot_NoChanges_5k(b *testing.B)   { benchIncrNoChange(b, 5000) }
func BenchmarkIncrementalSnapshot_NoChanges_10k(b *testing.B)  { benchIncrNoChange(b, 10000) }

func benchIncrNoChange(b *testing.B, fileCount int) {
	bfs := newBenchVaultFS()
	populateBenchFS(bfs, "u1", fileCount)
	prevSnap, prevIDMap, err := TakeSnapshotAndPathIDMap(bfs, "u1")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, _, err := TakeIncrementalSnapshot(bfs, "u1", prevSnap, prevIDMap)
		if err != nil {
			b.Fatal(err)
		}
		if len(snap) != fileCount {
			b.Fatalf("expected %d files, got %d", fileCount, len(snap))
		}
	}
}

// --- Benchmark: TakeIncrementalSnapshot (with changes) ---

func BenchmarkIncrementalSnapshot_1Change_1k(b *testing.B)     { benchIncrChanges(b, 1000, 1) }
func BenchmarkIncrementalSnapshot_10Changes_1k(b *testing.B)   { benchIncrChanges(b, 1000, 10) }
func BenchmarkIncrementalSnapshot_100Changes_1k(b *testing.B)  { benchIncrChanges(b, 1000, 100) }
func BenchmarkIncrementalSnapshot_1Change_10k(b *testing.B)    { benchIncrChanges(b, 10000, 1) }
func BenchmarkIncrementalSnapshot_10Changes_10k(b *testing.B)  { benchIncrChanges(b, 10000, 10) }
func BenchmarkIncrementalSnapshot_100Changes_10k(b *testing.B) { benchIncrChanges(b, 10000, 100) }

func benchIncrChanges(b *testing.B, fileCount, changeCount int) {
	bfs := newBenchVaultFS()
	populateBenchFS(bfs, "u1", fileCount)
	prevSnap, prevIDMap, err := TakeSnapshotAndPathIDMap(bfs, "u1")
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// 模擬變更：更新前 changeCount 個檔案的 mtime
		for j := 0; j < changeCount && j < fileCount; j++ {
			typeName := []string{"NOTE", "TASK", "EVENT", "LINK"}[j%4]
			path := fmt.Sprintf("u1/%s/item_%05d.json", typeName, j)
			bfs.mu.Lock()
			bfs.mtime[path] = time.Now()
			bfs.mu.Unlock()
		}
		b.StartTimer()

		snap, _, err := TakeIncrementalSnapshot(bfs, "u1", prevSnap, prevIDMap)
		if err != nil {
			b.Fatal(err)
		}
		if len(snap) != fileCount {
			b.Fatalf("expected %d files, got %d", fileCount, len(snap))
		}
	}
}

// --- Benchmark: ComputeDiff ---

func BenchmarkComputeDiff_1k(b *testing.B)  { benchComputeDiff(b, 1000) }
func BenchmarkComputeDiff_5k(b *testing.B)  { benchComputeDiff(b, 5000) }
func BenchmarkComputeDiff_10k(b *testing.B) { benchComputeDiff(b, 10000) }

func benchComputeDiff(b *testing.B, fileCount int) {
	now := time.Now()
	before := make(map[string]FileSnapshot, fileCount)
	after := make(map[string]FileSnapshot, fileCount)

	for i := 0; i < fileCount; i++ {
		path := fmt.Sprintf("NOTE/item_%05d.json", i)
		before[path] = FileSnapshot{Path: path, Hash: fmt.Sprintf("h%d", i), ModTime: now}

		switch {
		case i < 10:
			// 修改前 10 個
			after[path] = FileSnapshot{Path: path, Hash: fmt.Sprintf("h%d_mod", i), ModTime: now}
		case i >= fileCount-5:
			// 刪除最後 5 個（不加入 after）
		default:
			after[path] = before[path]
		}
	}
	// 新增 5 個
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("NOTE/new_%05d.json", i)
		after[path] = FileSnapshot{Path: path, Hash: fmt.Sprintf("hn%d", i), ModTime: now}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeDiff(before, after)
	}
}
