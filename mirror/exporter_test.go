package mirror

import (
	"strings"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

func newTestExporter() (*Exporter, *MemoryVaultFS) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "筆記A", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: "c1", Name: "美食清單", ItemType: "CARD_FOLDER", ParentID: nil},
	})
	return NewExporter(fs, resolver), fs
}

func TestExportItem_FolderWritesSiblingJSON(t *testing.T) {
	exp, fs := newTestExporter()
	item := &model.Item{
		ID:   "f1",
		Name: "工作",
		Type: "NOTE_FOLDER",
		Fields: map[string]interface{}{
			"usn": 1,
		},
	}
	_, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/NOTE/工作.json") {
		t.Fatal("folder metadata json should exist")
	}
	if fs.Exists("user1/NOTE/工作/工作.json") {
		t.Fatal("old embedded folder json should not exist")
	}
}

func TestExportItem_UsesParentIDPath(t *testing.T) {
	exp, fs := newTestExporter()
	_, err := exp.ExportItem("user1", &model.Item{
		ID:   "n-child",
		Name: "A評論",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "n1",
			"content":  "hello",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/NOTE/工作/筆記A/A評論.json") {
		t.Fatal("child note should be placed under parent note container")
	}
}

func TestExportItem_NoteWritesJSONFile(t *testing.T) {
	exp, fs := newTestExporter()
	item := &model.Item{
		ID:   "n2",
		Name: "今日會議",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID":  "f1",
			"content":   "<p>會議內容</p>",
			"usn":       3,
			"createdAt": "1700000000000",
			"updatedAt": "1709000000000",
		},
	}
	_, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}

	data, err := fs.ReadFile("user1/NOTE/工作/今日會議.json")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, `"id": "n2"`) {
		t.Error("should write mirror item json")
	}
	if !strings.Contains(content, "會議內容") {
		t.Error("should keep note content in json fields")
	}
}

func TestExportItem_RenameMovesChildContainer(t *testing.T) {
	exp, fs := newTestExporter()
	parent := &model.Item{ID: "n1", Name: "筆記A", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	child := &model.Item{ID: "n-child", Name: "A評論", Type: "NOTE", Fields: map[string]interface{}{"parentID": "n1"}}
	if _, err := exp.ExportItem("user1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportItem("user1", child); err != nil {
		t.Fatal(err)
	}

	parent.Name = "筆記B"
	if _, err := exp.ExportItem("user1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportItem("user1", child); err != nil {
		t.Fatal(err)
	}

	if fs.Exists("user1/NOTE/工作/筆記A.json") || fs.Exists("user1/NOTE/工作/筆記A") {
		t.Fatal("old parent projection should be removed")
	}
	if !fs.Exists("user1/NOTE/工作/筆記B.json") {
		t.Fatal("renamed parent json should exist")
	}
	if !fs.Exists("user1/NOTE/工作/筆記B/A評論.json") {
		t.Fatal("child container should move with parent rename")
	}
}

func TestExportItem_CardWritesJSONFile(t *testing.T) {
	exp, fs := newTestExporter()
	item := &model.Item{
		ID:   "card1",
		Name: "鼎泰豐",
		Type: "CARD",
		Fields: map[string]interface{}{
			"parentID": "c1",
			"fields":   `{"店名":"鼎泰豐"}`,
		},
	}
	_, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/CARD/美食清單/鼎泰豐.json") {
		t.Fatal("card json should exist")
	}
}

func TestDeleteItem_RemovesJSONAndChildDir(t *testing.T) {
	exp, fs := newTestExporter()
	parent := &model.Item{ID: "n1", Name: "筆記A", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	child := &model.Item{ID: "n-child", Name: "A評論", Type: "NOTE", Fields: map[string]interface{}{"parentID": "n1"}}
	if _, err := exp.ExportItem("user1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportItem("user1", child); err != nil {
		t.Fatal(err)
	}
	if err := exp.DeleteItem("user1", "n1"); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("user1/NOTE/工作/筆記A.json") || fs.Exists("user1/NOTE/工作/筆記A") {
		t.Fatal("delete should remove both json and child container")
	}
}

func newItemTestExporter() (*Exporter, *MemoryVaultFS) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "c1", Name: "看板", ItemType: "CARD_FOLDER", ParentID: nil},
	})
	return NewExporter(fs, resolver), fs
}

func TestExportItem_LeafWritesJSON(t *testing.T) {
	exp, fs := newItemTestExporter()
	item := &model.Item{
		ID:   "i1",
		Name: "會議記錄",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "f1",
			"content":  "hello",
		},
	}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsFolder {
		t.Error("NOTE item should not be folder")
	}
	if !fs.Exists("user1/NOTE/工作/會議記錄.json") {
		t.Error("JSON file should exist at expected path")
	}
	if result.Path != "user1/NOTE/工作/會議記錄.json" {
		t.Errorf("returned path: got %q", result.Path)
	}
}

func TestExportItem_FolderCreatesDir(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f-new", Name: "新目錄", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	exp := NewExporter(fs, resolver)

	item := &model.Item{
		ID:     "f-new",
		Name:   "新目錄",
		Type:   "NOTE_FOLDER",
		Fields: map[string]interface{}{},
	}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsFolder {
		t.Error("FOLDER item should be folder")
	}
	if !fs.Exists("user1/NOTE/新目錄.json") {
		t.Error("folder metadata JSON should exist")
	}
	if fs.Exists("user1/NOTE/新目錄") {
		t.Error("folder without children should not create container dir")
	}
}

func TestExportItem_Collision_UsesIDSuffix(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	exp := NewExporter(fs, resolver)

	first := &model.Item{ID: "f-a", Name: "inbox", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	second := &model.Item{ID: "f-b", Name: "inbox", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}

	firstResult, err := exp.ExportItem("user1", first)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := exp.ExportItem("user1", second)
	if err != nil {
		t.Fatal(err)
	}

	if firstResult.Path != "user1/NOTE/工作/inbox.json" {
		t.Fatalf("unexpected first item path: %q", firstResult.Path)
	}
	if secondResult.Path != "user1/NOTE/工作/inbox_f-b.json" {
		t.Fatalf("expected full ID suffix, got %q", secondResult.Path)
	}
	if !fs.Exists(firstResult.Path) || !fs.Exists(secondResult.Path) {
		t.Fatal("both colliding items should exist")
	}
}

func TestExportItem_EmptyName_UsesID(t *testing.T) {
	exp, fs := newItemTestExporter()
	item := &model.Item{
		ID:   "abc12345",
		Name: "",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "f1",
		},
	}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/工作/abc12345.json" {
		t.Errorf("empty name should use ID as filename, got %q", result.Path)
	}
	if !fs.Exists("user1/NOTE/工作/abc12345.json") {
		t.Error("file should exist")
	}
}

func TestExportItem_ResolveCollision_DifferentID(t *testing.T) {
	exp, fs := newItemTestExporter()
	item1 := &model.Item{
		ID: "id_aaaabbbb", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}
	item2 := &model.Item{
		ID: "id_ccccdddd", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}
	if _, err := exp.ExportItem("user1", item1); err != nil {
		t.Fatal(err)
	}
	result, err := exp.ExportItem("user1", item2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/工作/同名_id_ccccdddd.json" {
		t.Errorf("collision should add full id suffix, got path: %q", result.Path)
	}
	if !fs.Exists(result.Path) {
		t.Error("collision-resolved file should exist")
	}
}

func TestExportItem_ResolveCollision_SameID_Overwrites(t *testing.T) {
	exp, _ := newItemTestExporter()
	item := &model.Item{
		ID: "same-id", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}
	if _, err := exp.ExportItem("user1", item); err != nil {
		t.Fatal(err)
	}
	item.Fields["content"] = "updated"
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/工作/同名.json" {
		t.Errorf("same ID should overwrite without suffix, got: %q", result.Path)
	}
}

func TestDeleteItem_RemovesLeaf(t *testing.T) {
	exp, fs := newItemTestExporter()
	item := &model.Item{
		ID: "del1", Name: "要刪除", Type: "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}
	if _, err := exp.ExportItem("user1", item); err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/NOTE/工作/要刪除.json") {
		t.Fatal("file should exist before delete")
	}
	if err := exp.DeleteItem("user1", "del1"); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("user1/NOTE/工作/要刪除.json") {
		t.Error("file should be removed after DeleteItem")
	}
}

func TestExportItem_RenameCleanupOldPath(t *testing.T) {
	exp, fs := newItemTestExporter()
	item := &model.Item{
		ID: "ren1", Name: "舊名", Type: "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}
	if _, err := exp.ExportItem("user1", item); err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/NOTE/工作/舊名.json") {
		t.Fatal("old file should exist")
	}
	item.Name = "新名"
	if _, err := exp.ExportItem("user1", item); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("user1/NOTE/工作/舊名.json") {
		t.Error("old file should be removed after rename")
	}
	if !fs.Exists("user1/NOTE/工作/新名.json") {
		t.Error("new file should exist after rename")
	}
}

func TestExportBatch_AllItems(t *testing.T) {
	exp, fs := newItemTestExporter()
	items := []*model.Item{
		{ID: "b1", Name: "note1", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}},
		{ID: "b2", Name: "note2", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}},
	}
	if err := exp.ExportBatch("user1", items); err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("user1/NOTE/工作/note1.json") || !fs.Exists("user1/NOTE/工作/note2.json") {
		t.Fatal("batch export should create all items")
	}
}
