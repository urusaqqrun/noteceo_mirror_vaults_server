package executor

import (
	"testing"
	"time"
)

func TestComputeDiff_NoChanges(t *testing.T) {
	now := time.Now()
	snap := map[string]FileSnapshot{
		"NOTE/工作/a.md": {Path: "NOTE/工作/a.md", Hash: "hash1", ModTime: now},
	}
	diff := ComputeDiff(snap, snap)

	if len(diff.Created) != 0 {
		t.Errorf("Created: got %d, want 0", len(diff.Created))
	}
	if len(diff.Modified) != 0 {
		t.Errorf("Modified: got %d, want 0", len(diff.Modified))
	}
	if len(diff.Deleted) != 0 {
		t.Errorf("Deleted: got %d, want 0", len(diff.Deleted))
	}
	if len(diff.Moved) != 0 {
		t.Errorf("Moved: got %d, want 0", len(diff.Moved))
	}
}

func TestComputeDiff_NewFile(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{}
	after := map[string]FileSnapshot{
		"NOTE/工作/new.md": {Path: "NOTE/工作/new.md", Hash: "hash1", ModTime: now},
	}
	diff := ComputeDiff(before, after)

	if len(diff.Created) != 1 || diff.Created[0] != "NOTE/工作/new.md" {
		t.Errorf("Created: got %v, want [NOTE/工作/new.md]", diff.Created)
	}
}

func TestComputeDiff_ModifiedFile(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{
		"NOTE/工作/a.md": {Path: "NOTE/工作/a.md", Hash: "hash1", ModTime: now},
	}
	after := map[string]FileSnapshot{
		"NOTE/工作/a.md": {Path: "NOTE/工作/a.md", Hash: "hash2", ModTime: now},
	}
	diff := ComputeDiff(before, after)

	if len(diff.Modified) != 1 || diff.Modified[0] != "NOTE/工作/a.md" {
		t.Errorf("Modified: got %v, want [NOTE/工作/a.md]", diff.Modified)
	}
}

func TestComputeDiff_DeletedFile(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{
		"NOTE/工作/a.md": {Path: "NOTE/工作/a.md", Hash: "hash1", ModTime: now},
	}
	after := map[string]FileSnapshot{}
	diff := ComputeDiff(before, after)

	if len(diff.Deleted) != 1 || diff.Deleted[0] != "NOTE/工作/a.md" {
		t.Errorf("Deleted: got %v, want [NOTE/工作/a.md]", diff.Deleted)
	}
}

func TestComputeDiff_MovedFile(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{
		"NOTE/未分類/x.md": {Path: "NOTE/未分類/x.md", Hash: "hash1", ModTime: now},
	}
	after := map[string]FileSnapshot{
		"NOTE/工作/x.md": {Path: "NOTE/工作/x.md", Hash: "hash1", ModTime: now},
	}
	diff := ComputeDiff(before, after)

	if len(diff.Moved) != 1 {
		t.Fatalf("Moved: got %d, want 1", len(diff.Moved))
	}
	if diff.Moved[0].OldPath != "NOTE/未分類/x.md" || diff.Moved[0].NewPath != "NOTE/工作/x.md" {
		t.Errorf("Moved: got %v", diff.Moved[0])
	}
	if len(diff.Created) != 0 || len(diff.Deleted) != 0 {
		t.Error("搬移偵測後不應再出現在 Created/Deleted")
	}
}

func TestComputeDiff_MovedAndModified(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{
		"NOTE/未分類/x.md": {Path: "NOTE/未分類/x.md", Hash: "hash1", ModTime: now},
	}
	after := map[string]FileSnapshot{
		"NOTE/工作/x.md": {Path: "NOTE/工作/x.md", Hash: "hash2", ModTime: now},
	}
	diff := ComputeDiff(before, after)

	// 搬移且修改 → 無法判定為 move，視為 Deleted + Created
	if len(diff.Moved) != 0 {
		t.Errorf("MovedAndModified 不應被偵測為 move, got %d", len(diff.Moved))
	}
	if len(diff.Deleted) != 1 {
		t.Errorf("Deleted: got %d, want 1", len(diff.Deleted))
	}
	if len(diff.Created) != 1 {
		t.Errorf("Created: got %d, want 1", len(diff.Created))
	}
}

func TestComputeDiff_MultipleChanges(t *testing.T) {
	now := time.Now()
	before := map[string]FileSnapshot{
		"NOTE/a.md": {Path: "NOTE/a.md", Hash: "h1", ModTime: now},
		"NOTE/b.md": {Path: "NOTE/b.md", Hash: "h2", ModTime: now},
		"NOTE/c.md": {Path: "NOTE/c.md", Hash: "h3", ModTime: now},
	}
	after := map[string]FileSnapshot{
		"NOTE/a.md": {Path: "NOTE/a.md", Hash: "h1_changed", ModTime: now},
		// b.md 刪除
		"NOTE/c.md": {Path: "NOTE/c.md", Hash: "h3", ModTime: now}, // 不變
		"NOTE/d.md": {Path: "NOTE/d.md", Hash: "h4", ModTime: now}, // 新增
	}
	diff := ComputeDiff(before, after)

	if len(diff.Modified) != 1 {
		t.Errorf("Modified: got %d, want 1", len(diff.Modified))
	}
	if len(diff.Deleted) != 1 {
		t.Errorf("Deleted: got %d, want 1", len(diff.Deleted))
	}
	if len(diff.Created) != 1 {
		t.Errorf("Created: got %d, want 1", len(diff.Created))
	}
}
