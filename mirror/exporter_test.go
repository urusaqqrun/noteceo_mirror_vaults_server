package mirror

import (
	"strings"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

func newTestExporter() (*Exporter, *MemoryVaultFS) {
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "筆記A", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: "c1", Name: "美食清單", ItemType: "CARD_FOLDER", ParentID: nil},
	})
	return NewExporter(mfs, resolver), mfs
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

	exp.resolver.UpdateNode(TreeNode{ID: "n1", Name: "筆記B", ItemType: "NOTE", ParentID: strPtr("f1")})
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
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "c1", Name: "看板", ItemType: "CARD_FOLDER", ParentID: nil},
	})
	return NewExporter(mfs, resolver), mfs
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

func TestExportItem_SameNameDifferentID_UsesIDSuffix(t *testing.T) {
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f-a", Name: "inbox", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: "f-b", Name: "inbox", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exp := NewExporter(mfs, resolver)

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

	if firstResult.Path != "user1/NOTE/工作/inbox_f-a.json" {
		t.Fatalf("unexpected first item path: %q", firstResult.Path)
	}
	if secondResult.Path != "user1/NOTE/工作/inbox_f-b.json" {
		t.Fatalf("unexpected second item path: %q", secondResult.Path)
	}
	if !mfs.Exists(firstResult.Path) || !mfs.Exists(secondResult.Path) {
		t.Fatal("both items should exist with unique names")
	}
}

func TestExportItem_UniqueName_NoIDSuffix(t *testing.T) {
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "inbox", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: "n2", Name: "outbox", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exp := NewExporter(mfs, resolver)

	item := &model.Item{ID: "n1", Name: "inbox", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/工作/inbox.json" {
		t.Errorf("unique name should not have ID suffix, got %q", result.Path)
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

func TestExportItem_SameID_Overwrites(t *testing.T) {
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
		t.Errorf("same ID should overwrite, got: %q", result.Path)
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

func TestDeleteItem_RenamesSoleSiblingBack(t *testing.T) {
	id1 := "aaaaaaaaaaaaaaaaaaaaaaaa"
	id2 := "bbbbbbbbbbbbbbbbbbbbbbbb"
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: id1, Name: "同名", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: id2, Name: "同名", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exp := NewExporter(mfs, resolver)
	fs := mfs
	a := &model.Item{ID: id1, Name: "同名", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	b := &model.Item{ID: id2, Name: "同名", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	if _, err := exp.ExportItem("user1", a); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportItem("user1", b); err != nil {
		t.Fatal(err)
	}
	pathA := "user1/NOTE/工作/同名_" + id1 + ".json"
	pathB := "user1/NOTE/工作/同名_" + id2 + ".json"
	if !fs.Exists(pathA) {
		t.Fatalf("id1 should have _id suffix, files: %v", fs.ListAllFiles())
	}
	if !fs.Exists(pathB) {
		t.Fatal("id2 should have _id suffix")
	}
	if err := exp.DeleteItem("user1", id1); err != nil {
		t.Fatal(err)
	}
	if fs.Exists(pathA) {
		t.Error("id1 should be deleted")
	}
	if fs.Exists(pathB) {
		t.Error("sole remaining sibling should be renamed back (no _id)")
	}
	if !fs.Exists("user1/NOTE/工作/同名.json") {
		t.Errorf("sole remaining sibling should be 同名.json, files: %v", fs.ListAllFiles())
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

func TestExportItem_NonFolderNoParent_GoesToUnsorted(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{})
	exp := NewExporter(fs, resolver)

	item := &model.Item{
		ID:     "orphan1",
		Name:   "孤兒筆記",
		Type:   "TODO",
		Fields: map[string]interface{}{},
	}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/TODO/_unsorted/孤兒筆記.json" {
		t.Errorf("non-folder without parent should go to {TYPE}/_unsorted, got %q", result.Path)
	}
}

func TestExportItem_OrphanParent_GoesToUnsorted(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{})
	exp := NewExporter(fs, resolver)

	item := &model.Item{
		ID:   "orphan2",
		Name: "斷鏈筆記",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "nonexistent-parent",
		},
	}
	result, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/_unsorted/斷鏈筆記.json" {
		t.Errorf("orphan parent should go to {TYPE}/_unsorted, got %q", result.Path)
	}
}

// --- 增量匯出時同名衝突自動重命名 sibling ---

func TestExportItem_NewConflict_RenamesSibling(t *testing.T) {
	mfs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "inbox", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exp := NewExporter(mfs, resolver)

	// 先匯出 n1（唯一）→ inbox.json
	first := &model.Item{ID: "n1", Name: "inbox", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	r1, err := exp.ExportItem("user1", first)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Path != "user1/NOTE/工作/inbox.json" {
		t.Fatalf("first export should be simple name, got %q", r1.Path)
	}

	// 新增同名 n2 到 resolver
	resolver.AddNode(TreeNode{ID: "n2", Name: "inbox", ItemType: "NOTE", ParentID: strPtr("f1")})

	// 匯出 n2 → inbox_n2.json，且 n1 應被重命名為 inbox_n1.json
	second := &model.Item{ID: "n2", Name: "inbox", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	r2, err := exp.ExportItem("user1", second)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Path != "user1/NOTE/工作/inbox_n2.json" {
		t.Fatalf("second export should have ID suffix, got %q", r2.Path)
	}
	if !mfs.Exists("user1/NOTE/工作/inbox_n1.json") {
		t.Fatal("sibling n1 should be renamed to inbox_n1.json")
	}
	if mfs.Exists("user1/NOTE/工作/inbox.json") {
		t.Fatal("old inbox.json should no longer exist")
	}
}
