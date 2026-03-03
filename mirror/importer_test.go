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

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/工作/新筆記.md"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Action != ImportActionCreate {
		t.Errorf("action: got %q, want %q", entries[0].Action, ImportActionCreate)
	}
	if entries[0].Collection != "note" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "note")
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

	entries, err := imp.ProcessDiff("user1", nil, []string{"NOTE/工作/修改的筆記.md"}, nil, nil)
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

	entries, err := imp.ProcessDiff("user1", nil, nil, []string{"NOTE/工作/刪除的筆記.md"}, nil)
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
	})
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

	folderJSON := `{"ID":"f-new","memberID":"user1","folderName":"新目錄","type":"NOTE"}`
	fs.WriteFile("user1/NOTE/新目錄/_folder.json", []byte(folderJSON))

	entries, err := imp.ProcessDiff("user1", []string{"NOTE/新目錄/_folder.json"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Collection != "folder" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "folder")
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

	entries, err := imp.ProcessDiff("user1", []string{"CARD/美食/鼎泰豐.json"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Collection != "card" {
		t.Errorf("collection: got %q, want %q", entries[0].Collection, "card")
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
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3", len(entries))
	}
}

func TestDetectCollection(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"NOTE/工作/_folder.json", "folder"},
		{"NOTE/工作/test.md", "note"},
		{"CARD/美食/鼎泰豐.json", "card"},
		{"CHART/月營收/Q4.json", "chart"},
		{"TODO/待辦/task.md", "note"},
	}
	for _, tt := range tests {
		got := detectCollection(tt.path)
		if got != tt.expected {
			t.Errorf("detectCollection(%q): got %q, want %q", tt.path, got, tt.expected)
		}
	}
}
