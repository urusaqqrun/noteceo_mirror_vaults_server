package mirror

import (
	"testing"
)

func setupImporterFS() (*Importer, *MemoryVaultFS) {
	fs := NewMemoryVaultFS()
	return NewImporter(fs), fs
}

func TestProcessDiff_CreatedNote(t *testing.T) {
	imp, fs := setupImporterFS()

	mdContent := `---
id: n1
parentID: f1
title: 新筆記
usn: 1
htmlHash: abc123
createdAt: "1700000000000"
updatedAt: "1709000000000"
---

# 新筆記

這是新建的筆記內容
`
	fs.WriteFile("user1/NOTE/工作/新筆記.md", []byte(mdContent))

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/工作/新筆記.md"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Action != ImportActionCreate {
		t.Errorf("action: got %q, want %q", entries[0].Action, ImportActionCreate)
	}
	if entries[0].Collection != "item" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "item")
	}
	if entries[0].ItemType != "NOTE" {
		t.Errorf("itemType: got %q, want %q", entries[0].ItemType, "NOTE")
	}
	if entries[0].NoteMeta == nil {
		t.Fatal("NoteMeta should not be nil")
	}
	if entries[0].NoteMeta.ID != "n1" {
		t.Errorf("note ID: got %q, want %q", entries[0].NoteMeta.ID, "n1")
	}
}

func TestProcessDiff_ModifiedNote(t *testing.T) {
	imp, fs := setupImporterFS()

	mdContent := `---
id: n1
parentID: f1
title: 修改的筆記
usn: 2
htmlHash: newHash
createdAt: "1700000000000"
updatedAt: "1709000000000"
---

# 修改的筆記

更新後的內容
`
	fs.WriteFile("user1/NOTE/工作/修改的筆記.md", []byte(mdContent))

	entries, err := imp.ProcessDiff("user1", nil, []string{"NOTE/工作/修改的筆記.md"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Action != ImportActionUpdate {
		t.Errorf("action: got %q, want %q", entries[0].Action, ImportActionUpdate)
	}
	if entries[0].HTMLHash != "newHash" {
		t.Errorf("htmlHash: got %q, want %q", entries[0].HTMLHash, "newHash")
	}
}

func TestProcessDiff_DeletedNote(t *testing.T) {
	imp, _ := setupImporterFS()

	entries, err := imp.ProcessDiff("user1", nil, nil, []string{"NOTE/工作/刪除的筆記.md"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Action != ImportActionDelete {
		t.Errorf("action: got %q, want %q", entries[0].Action, ImportActionDelete)
	}
}

func TestProcessDiff_MovedNote(t *testing.T) {
	imp, fs := setupImporterFS()

	mdContent := `---
id: n1
parentID: f2
title: 搬移的筆記
usn: 3
htmlHash: sameHash
createdAt: "1700000000000"
updatedAt: "1709000000000"
---

搬移後的筆記
`
	fs.WriteFile("user1/NOTE/生活/搬移的筆記.md", []byte(mdContent))

	entries, err := imp.ProcessDiff("user1", nil, nil, nil, []MovedFileEntry{
		{OldPath: "NOTE/工作/搬移的筆記.md", NewPath: "NOTE/生活/搬移的筆記.md"},
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
	if entries[0].OldPath != "NOTE/工作/搬移的筆記.md" {
		t.Errorf("oldPath: got %q", entries[0].OldPath)
	}
}

func TestProcessDiff_NewFolder(t *testing.T) {
	imp, fs := setupImporterFS()

	folderJSON := `{"ID":"f-new","folderName":"新目錄","type":"NOTE"}`
	fs.WriteFile("user1/NOTE/新目錄/_folder.json", []byte(folderJSON))

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/新目錄/_folder.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Collection != "item" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "item")
	}
	if entries[0].ItemType != "FOLDER" {
		t.Errorf("itemType: got %q, want %q", entries[0].ItemType, "FOLDER")
	}
	if entries[0].FolderMeta == nil {
		t.Fatal("FolderMeta should not be nil")
	}
	if entries[0].FolderMeta.ID != "f-new" {
		t.Errorf("folder ID: got %q, want %q", entries[0].FolderMeta.ID, "f-new")
	}
}

func TestProcessDiff_CardCreated(t *testing.T) {
	imp, fs := setupImporterFS()

	cardJSON := `{"id":"card1","parentID":"c1","name":"鼎泰豐","fields":"{\"店名\":\"鼎泰豐\"}"}`
	fs.WriteFile("user1/CARD/美食/鼎泰豐.json", []byte(cardJSON))

	entries, err := imp.ProcessDiff("user1", []string{"CARD/美食/鼎泰豐.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Collection != "item" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "item")
	}
	if entries[0].ItemType != "CARD" {
		t.Errorf("itemType: got %q, want %q", entries[0].ItemType, "CARD")
	}
	if entries[0].CardMeta.Name != "鼎泰豐" {
		t.Errorf("card name: got %q", entries[0].CardMeta.Name)
	}
}

func TestProcessDiff_MixedChanges(t *testing.T) {
	imp, fs := setupImporterFS()

	fs.WriteFile("user1/NOTE/工作/新.md", []byte("---\nid: n1\nparentID: f1\ntitle: 新\nusn: 1\nhtmlHash: h1\ncreatedAt: \"0\"\nupdatedAt: \"0\"\n---\ncontent"))
	fs.WriteFile("user1/NOTE/工作/改.md", []byte("---\nid: n2\nparentID: f1\ntitle: 改\nusn: 2\nhtmlHash: h2\ncreatedAt: \"0\"\nupdatedAt: \"0\"\n---\ncontent"))

	entries, err := imp.ProcessDiff("user1",
		[]string{"NOTE/工作/新.md"},
		[]string{"NOTE/工作/改.md"},
		[]string{"NOTE/工作/刪.md"},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3", len(entries))
	}
}

// --- 新格式 JSON 解析 ---

func TestProcessDiff_NewFormatJSON_Created(t *testing.T) {
	imp, fs := setupImporterFS()

	itemJSON := `{"id":"item1","name":"看板1","itemType":"KANBAN","fields":{"color":"red"}}`
	fs.WriteFile("user1/KANBAN/看板/看板1.json", []byte(itemJSON))

	entries, err := imp.ProcessDiff("user1", []string{"KANBAN/看板/看板1.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.ItemData == nil {
		t.Fatal("ItemData should not be nil for new format")
	}
	if e.ItemData.ID != "item1" {
		t.Errorf("ID: got %q, want %q", e.ItemData.ID, "item1")
	}
	if e.ItemData.ItemType != "KANBAN" {
		t.Errorf("ItemType: got %q, want %q", e.ItemData.ItemType, "KANBAN")
	}
	if e.ItemType != "KANBAN" {
		t.Errorf("entry.ItemType: got %q, want %q", e.ItemType, "KANBAN")
	}
}

func TestProcessDiff_NewFormat_FallbackNameCleared(t *testing.T) {
	imp, fs := setupImporterFS()

	itemJSON := `{"id":"abc123","name":"untitled_abc123","itemType":"NOTE","fields":{}}`
	fs.WriteFile("user1/NOTE/工作/untitled_abc123.json", []byte(itemJSON))

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/工作/untitled_abc123.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].ItemData.Name != "" {
		t.Errorf("fallback name should be cleared, got %q", entries[0].ItemData.Name)
	}
}

func TestProcessDiff_OldFolderJSON_StillWorks(t *testing.T) {
	imp, fs := setupImporterFS()

	folderJSON := `{"ID":"f1","folderName":"舊資料夾","type":"NOTE"}`
	fs.WriteFile("user1/NOTE/舊資料夾/_folder.json", []byte(folderJSON))

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/舊資料夾/_folder.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].FolderMeta == nil {
		t.Fatal("old format _folder.json should still parse as FolderMeta")
	}
	if entries[0].ItemData != nil {
		t.Error("old format should NOT produce ItemData")
	}
}

func TestDetectItemType_GenericRootDir(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"KANBAN/board/task.json", "KANBAN"},
		{"WHITEBOARD/stuff/drawing.json", "WHITEBOARD"},
		{"NOTE/folder/note.md", "NOTE"},
		{"CARD/list/card.json", "CARD"},
	}
	for _, tt := range tests {
		got := detectItemType(tt.path)
		if got != tt.expected {
			t.Errorf("detectItemType(%q): got %q, want %q", tt.path, got, tt.expected)
		}
	}
}

func TestDetectItemType(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"NOTE/工作/_folder.json", "FOLDER"},
		{"NOTE/工作/test.md", "NOTE"},
		{"CARD/美食/鼎泰豐.json", "CARD"},
		{"CHART/月營收/Q4.json", "CHART"},
		{"TODO/待辦/task.md", "TODO"},
	}
	for _, tt := range tests {
		got := detectItemType(tt.path)
		if got != tt.expected {
			t.Errorf("detectItemType(%q): got %q, want %q", tt.path, got, tt.expected)
		}
	}
}

func TestDetectCollection_AlwaysReturnsItem(t *testing.T) {
	paths := []string{"NOTE/test.md", "CARD/test.json", "_folder.json"}
	for _, p := range paths {
		got := detectCollection(p)
		if got != "item" {
			t.Errorf("detectCollection(%q): got %q, want %q", p, got, "item")
		}
	}
}
