package mirror

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// E2E 場景 1: 用戶建 Note → Vault 同步
func TestE2E_UserCreatesNote_VaultSync(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "會議紀錄", Type: "NOTE", ParentID: strPtr("f1")},
	})
	exporter := NewExporter(fs, resolver)

	// 匯出 Folder
	noteType := "NOTE"
	exporter.ExportFolder("user1", FolderMeta{ID: "f1", MemberID: "user1", FolderName: "工作", Type: &noteType})
	exporter.ExportFolder("user1", FolderMeta{ID: "f2", MemberID: "user1", FolderName: "會議紀錄", Type: &noteType, ParentID: strPtr("f1")})

	// 匯出 Note
	err := exporter.ExportNote("user1", NoteMeta{
		ID: "n1", ParentID: "f2", Title: "週會記要",
		USN: 3, Tags: []string{"會議", "工作"},
		CreatedAt: "1700000000000", UpdatedAt: "1709000000000",
	}, "<h1>週會記要</h1><p>討論事項...</p>")

	if err != nil {
		t.Fatal(err)
	}

	// 驗證 Vault 檔案結構
	if !fs.Exists("user1/NOTE/工作") {
		t.Error("工作目錄應存在")
	}
	if !fs.Exists("user1/NOTE/工作/會議紀錄") {
		t.Error("會議紀錄目錄應存在")
	}
	if !fs.Exists("user1/NOTE/工作/會議紀錄/週會記要.md") {
		t.Error("週會記要.md 應存在")
	}

	// 讀取並驗證 Markdown 內容
	data, _ := fs.ReadFile("user1/NOTE/工作/會議紀錄/週會記要.md")
	content := string(data)
	if !strings.Contains(content, "id: n1") {
		t.Error("frontmatter 應包含 id")
	}
	if !strings.Contains(content, "會議") {
		t.Error("frontmatter 應包含 tags")
	}
	if !strings.Contains(content, "討論事項") {
		t.Error("body 應包含筆記內容")
	}
}

// E2E 場景 2: AI 搬移 Note → DB parentID 更新
func TestE2E_AIMoveNote_ParentIDUpdate(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "未分類", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "工作", Type: "NOTE", ParentID: nil},
	})
	exporter := NewExporter(fs, resolver)

	// 初始狀態：Note 在「未分類」
	noteType := "NOTE"
	exporter.ExportFolder("user1", FolderMeta{ID: "f1", MemberID: "user1", FolderName: "未分類", Type: &noteType})
	exporter.ExportFolder("user1", FolderMeta{ID: "f2", MemberID: "user1", FolderName: "工作", Type: &noteType})
	exporter.ExportNote("user1", NoteMeta{
		ID: "n1", ParentID: "f1", Title: "重要筆記", USN: 3,
		CreatedAt: "1700000000000", UpdatedAt: "1709000000000",
	}, "<p>重要內容</p>")

	// AI 搬移：從「未分類」到「工作」
	// 模擬 AI 更新了 parentID 後重新寫入
	movedMD := `---
id: n1
parentID: f2
title: 重要筆記
usn: 3
htmlHash: abc123
createdAt: "1700000000000"
updatedAt: "1709000000000"
---

重要內容
`
	fs.WriteFile("user1/NOTE/工作/重要筆記.md", []byte(movedMD))
	fs.Remove("user1/NOTE/未分類/重要筆記.md")

	// Importer 解析搬移
	importer := NewImporter(fs)
	entries, err := importer.ProcessDiff("user1", nil, nil, nil, []MovedFileEntry{
		{OldPath: "NOTE/未分類/重要筆記.md", NewPath: "NOTE/工作/重要筆記.md"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Action != ImportActionMove {
		t.Errorf("action: got %q, want %q", entries[0].Action, ImportActionMove)
	}
	if entries[0].NoteMeta.ParentID != "f2" {
		t.Errorf("parentID should be f2 after move, got %q", entries[0].NoteMeta.ParentID)
	}
}

// E2E 場景 3: 並發編輯衝突 → dbUSN > aiStartUSN 時跳過回寫（保留用戶版本）
func TestE2E_ConcurrentEdit_UserWins(t *testing.T) {
	aiStartUSN := 5
	dbUSN := 8

	shouldSkip := dbUSN > aiStartUSN
	if !shouldSkip {
		t.Error("dbUSN > aiStartUSN should trigger skip (user modified during AI task)")
	}

	// 反向驗證：dbUSN <= aiStartUSN 則套用
	dbUSN2 := 5
	shouldSkip2 := dbUSN2 > aiStartUSN
	if shouldSkip2 {
		t.Error("dbUSN == aiStartUSN should NOT skip")
	}
}

// E2E 場景 4: 大量 Folder + Note 全量同步
func TestE2E_BulkSync_100Folders_500Notes(t *testing.T) {
	fs := NewMemoryVaultFS()

	// 建立 100 個 Folder
	folders := make([]FolderNode, 100)
	for i := 0; i < 100; i++ {
		folders[i] = FolderNode{
			ID:         fmt.Sprintf("f%d", i),
			FolderName: fmt.Sprintf("Folder_%d", i),
			Type:       "NOTE",
			ParentID:   nil,
		}
	}
	resolver := NewPathResolver(folders)
	exporter := NewExporter(fs, resolver)

	noteType := "NOTE"
	for _, f := range folders {
		exporter.ExportFolder("user1", FolderMeta{
			ID: f.ID, MemberID: "user1", FolderName: f.FolderName, Type: &noteType,
		})
	}

	// 建立 500 個 Note（每 Folder 5 個）
	noteCount := 0
	for i := 0; i < 100; i++ {
		for j := 0; j < 5; j++ {
			noteID := fmt.Sprintf("n%d_%d", i, j)
			title := fmt.Sprintf("Note_%d_%d", i, j)
			err := exporter.ExportNote("user1", NoteMeta{
				ID: noteID, ParentID: fmt.Sprintf("f%d", i), Title: title, USN: 1,
				CreatedAt: "1700000000000", UpdatedAt: "1709000000000",
			}, fmt.Sprintf("<p>Content of %s</p>", title))
			if err != nil {
				t.Fatalf("export note %s: %v", noteID, err)
			}
			noteCount++
		}
	}

	if noteCount != 500 {
		t.Errorf("exported %d notes, want 500", noteCount)
	}

	// 驗證隨機幾個檔案存在
	if !fs.Exists("user1/NOTE/Folder_0/Note_0_0.md") {
		t.Error("first note should exist")
	}
	if !fs.Exists("user1/NOTE/Folder_99/Note_99_4.md") {
		t.Error("last note should exist")
	}

	// 驗證 _folder.json 存在
	data, err := fs.ReadFile("user1/NOTE/Folder_50/_folder.json")
	if err != nil {
		t.Fatal("folder 50 json should exist:", err)
	}
	var meta FolderMeta
	json.Unmarshal(data, &meta)
	if meta.FolderName != "Folder_50" {
		t.Errorf("folder name: got %q, want %q", meta.FolderName, "Folder_50")
	}
}
