package executor

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

// TestIntegration_SnapshotRealFS 使用真實磁碟（模擬 EFS）量測 snapshot 各場景的耗時。
// 執行方式：go test -run TestIntegration_SnapshotRealFS -v -count=1 ./executor/
func TestIntegration_SnapshotRealFS(t *testing.T) {
	if testing.Short() {
		t.Skip("skip integration test in short mode")
	}

	fileCounts := []int{100, 1000, 5000, 10000}
	changeCounts := []int{0, 1, 10, 100}
	sampleCount := 5

	type result struct {
		Scenario string
		Files    int
		Changes  int
		Samples  []float64
		Avg      float64
		P95      float64
	}
	var results []result

	for _, fc := range fileCounts {
		tmpDir := t.TempDir()
		realFS := &mirror.RealVaultFS{Root: tmpDir}
		userID := "bench_user"

		populateRealFS(t, tmpDir, userID, fc)

		// 場景 A：Full Snapshot
		var samples []float64
		for s := 0; s < sampleCount; s++ {
			start := time.Now()
			snap, _, err := TakeSnapshotAndPathIDMap(realFS, userID)
			elapsed := float64(time.Since(start).Milliseconds())
			if err != nil {
				t.Fatalf("full snapshot error: %v", err)
			}
			if len(snap) != fc {
				t.Fatalf("expected %d, got %d", fc, len(snap))
			}
			samples = append(samples, elapsed)
		}
		avg, p95 := stats(samples)
		results = append(results, result{"FullSnapshot", fc, 0, samples, avg, p95})

		// 取一份基線快照
		baseSnap, baseIDMap, _ := TakeSnapshotAndPathIDMap(realFS, userID)

		// 場景 B：Incremental Snapshot（不同變更量）
		for _, cc := range changeCounts {
			var samples []float64
			for s := 0; s < sampleCount; s++ {
				// 模擬變更：touch 指定數量的檔案
				touchRealFiles(t, tmpDir, userID, fc, cc)

				start := time.Now()
				snap, _, err := TakeIncrementalSnapshot(realFS, userID, baseSnap, baseIDMap)
				elapsed := float64(time.Since(start).Milliseconds())
				if err != nil {
					t.Fatalf("incremental snapshot error: %v", err)
				}
				if len(snap) != fc {
					t.Fatalf("expected %d, got %d", fc, len(snap))
				}
				samples = append(samples, elapsed)
			}
			avg, p95 := stats(samples)
			results = append(results, result{"IncrSnapshot", fc, cc, samples, avg, p95})
		}

		// 場景 C：ComputeDiff
		{
			afterSnap := make(map[string]FileSnapshot, len(baseSnap))
			for k, v := range baseSnap {
				afterSnap[k] = v
			}
			// 模擬 10 個修改 + 5 新增 + 5 刪除
			i := 0
			for k := range afterSnap {
				if i < 10 {
					fs := afterSnap[k]
					fs.Hash = "modified_" + fs.Hash
					afterSnap[k] = fs
				}
				if i >= fc-5 {
					delete(afterSnap, k)
				}
				i++
			}
			for j := 0; j < 5; j++ {
				p := fmt.Sprintf("NOTE/new_test_%d.json", j)
				afterSnap[p] = FileSnapshot{Path: p, Hash: fmt.Sprintf("newh%d", j), ModTime: time.Now()}
			}

			var samples []float64
			for s := 0; s < sampleCount; s++ {
				start := time.Now()
				ComputeDiff(baseSnap, afterSnap)
				elapsed := float64(time.Since(start).Microseconds()) / 1000.0
				samples = append(samples, elapsed)
			}
			avg, p95 := stats(samples)
			results = append(results, result{"ComputeDiff", fc, 20, samples, avg, p95})
		}
	}

	// 輸出結果表格
	t.Log("\n=== Snapshot Performance Report (Real FS) ===")
	t.Logf("%-20s %8s %8s %10s %10s %s", "Scenario", "Files", "Changes", "Avg(ms)", "P95(ms)", "Samples(ms)")
	t.Log(strings.Repeat("-", 90))
	for _, r := range results {
		sampleStr := make([]string, len(r.Samples))
		for i, s := range r.Samples {
			sampleStr[i] = fmt.Sprintf("%.1f", s)
		}
		t.Logf("%-20s %8d %8d %10.1f %10.1f [%s]",
			r.Scenario, r.Files, r.Changes, r.Avg, r.P95, strings.Join(sampleStr, ", "))
	}
}

func populateRealFS(t *testing.T, root, userID string, count int) {
	t.Helper()
	types := []string{"NOTE", "TASK", "EVENT", "LINK"}
	for i := 0; i < count; i++ {
		typeName := types[i%len(types)]
		dir := filepath.Join(root, userID, typeName)
		os.MkdirAll(dir, 0755)
		path := filepath.Join(dir, fmt.Sprintf("item_%05d.json", i))
		doc := map[string]interface{}{
			"id":    fmt.Sprintf("id_%05d", i),
			"title": fmt.Sprintf("Item %d", i),
			"body":  strings.Repeat("x", 200),
		}
		data, _ := json.Marshal(doc)
		os.WriteFile(path, data, 0644)
	}
}

func touchRealFiles(t *testing.T, root, userID string, fileCount, changeCount int) {
	t.Helper()
	types := []string{"NOTE", "TASK", "EVENT", "LINK"}
	now := time.Now()
	for j := 0; j < changeCount && j < fileCount; j++ {
		typeName := types[j%len(types)]
		path := filepath.Join(root, userID, typeName, fmt.Sprintf("item_%05d.json", j))
		os.Chtimes(path, now, now)
	}
}

func stats(samples []float64) (avg, p95 float64) {
	n := len(samples)
	if n == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, v := range samples {
		sum += v
	}
	avg = sum / float64(n)

	sorted := make([]float64, n)
	copy(sorted, samples)
	sort.Float64s(sorted)
	idx := int(math.Ceil(0.95*float64(n))) - 1
	if idx >= n {
		idx = n - 1
	}
	p95 = sorted[idx]
	return
}
