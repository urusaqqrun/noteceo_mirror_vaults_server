package executor

import "time"

// FileSnapshot 檔案快照，用於 diff 比對
type FileSnapshot struct {
	Path    string
	Hash    string
	ModTime time.Time
}

// VaultDiff 快照比對結果
type VaultDiff struct {
	Created  []string
	Modified []string
	Deleted  []string
	Moved    []MovedFile
}

// MovedFile 搬移偵測結果（hash 相同但路徑不同）
type MovedFile struct {
	OldPath string
	NewPath string
}

// ComputeDiff 比對兩個快照，回傳差異
// 搬移偵測：before 中刪除的檔案，如果 hash 完全相同出現在 after 的新增中，視為搬移
func ComputeDiff(before, after map[string]FileSnapshot) VaultDiff {
	var diff VaultDiff

	var rawDeleted []string
	var rawCreated []string

	// 找出修改和刪除
	for path, bSnap := range before {
		aSnap, exists := after[path]
		if !exists {
			rawDeleted = append(rawDeleted, path)
		} else if aSnap.Hash != bSnap.Hash {
			diff.Modified = append(diff.Modified, path)
		}
	}

	// 找出新增
	for path := range after {
		if _, exists := before[path]; !exists {
			rawCreated = append(rawCreated, path)
		}
	}

	// 搬移偵測：deleted + created 中 hash 相同的配對
	deletedByHash := make(map[string]string) // hash → path
	for _, path := range rawDeleted {
		h := before[path].Hash
		deletedByHash[h] = path
	}

	movedOldPaths := make(map[string]bool)
	movedNewPaths := make(map[string]bool)

	for _, path := range rawCreated {
		h := after[path].Hash
		if oldPath, ok := deletedByHash[h]; ok {
			diff.Moved = append(diff.Moved, MovedFile{OldPath: oldPath, NewPath: path})
			movedOldPaths[oldPath] = true
			movedNewPaths[path] = true
			delete(deletedByHash, h)
		}
	}

	// 排除已配對的搬移
	for _, path := range rawDeleted {
		if !movedOldPaths[path] {
			diff.Deleted = append(diff.Deleted, path)
		}
	}
	for _, path := range rawCreated {
		if !movedNewPaths[path] {
			diff.Created = append(diff.Created, path)
		}
	}

	return diff
}
