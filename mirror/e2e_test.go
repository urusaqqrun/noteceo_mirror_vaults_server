package mirror

import (
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

func TestE2E_ExportItem_ThenImport_Roundtrip(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "筆記", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	exporter := NewExporter(fs, resolver)

	item := &model.Item{
		ID:   "n-new",
		Name: "新格式筆記",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "f1",
			"content":  "<p>Hello World</p>",
			"usn":      float64(3),
		},
	}

	result, err := exporter.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "user1/NOTE/筆記/新格式筆記.json" {
		t.Fatalf("unexpected path: %q", result.Path)
	}

	importer := NewImporter(fs)
	entries, err := importer.ProcessDiff("user1",
		[]string{"NOTE/筆記/新格式筆記.json"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.ItemData == nil {
		t.Fatal("should parse as item json")
	}
	if e.ItemData.ID != "n-new" || e.ItemData.Name != "新格式筆記" {
		t.Fatalf("imported item mismatch: %+v", e.ItemData)
	}
}

func TestE2E_ExportItem_EmptyName_UsesID(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "筆記", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	exporter := NewExporter(fs, resolver)

	item := &model.Item{
		ID:   "empty-name-id",
		Name: "",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"parentID": "f1",
		},
	}

	result, err := exporter.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}

	expectedPath := "NOTE/筆記/empty-name-id.json"
	if !fs.Exists("user1/" + expectedPath) {
		t.Fatalf("file should exist at %q, got path %q", expectedPath, result.Path)
	}

	importer := NewImporter(fs)
	entries, err := importer.ProcessDiff("user1", []string{expectedPath}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].ItemData.Name != "" {
		t.Errorf("empty name should stay empty on import, got %q", entries[0].ItemData.Name)
	}
}

func TestE2E_ChildNoteUsesParentContainer(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "筆記A", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exporter := NewExporter(fs, resolver)

	if _, err := exporter.ExportItem("user1", &model.Item{
		ID:     "f1",
		Name:   "工作",
		Type:   "NOTE_FOLDER",
		Fields: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.ExportItem("user1", &model.Item{
		ID:     "n1",
		Name:   "筆記A",
		Type:   "NOTE",
		Fields: map[string]interface{}{"parentID": "f1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.ExportItem("user1", &model.Item{
		ID:     "n2",
		Name:   "A評論",
		Type:   "NOTE",
		Fields: map[string]interface{}{"parentID": "n1"},
	}); err != nil {
		t.Fatal(err)
	}

	if !fs.Exists("user1/NOTE/工作.json") {
		t.Fatal("folder json should exist")
	}
	if !fs.Exists("user1/NOTE/工作/筆記A.json") {
		t.Fatal("parent note json should exist")
	}
	if !fs.Exists("user1/NOTE/工作/筆記A/A評論.json") {
		t.Fatal("child note should exist under parent container")
	}
}

func TestE2E_RenameParentMovesChildContainer(t *testing.T) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "n1", Name: "筆記A", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	exporter := NewExporter(fs, resolver)

	parent := &model.Item{ID: "n1", Name: "筆記A", Type: "NOTE", Fields: map[string]interface{}{"parentID": "f1"}}
	child := &model.Item{ID: "n2", Name: "A評論", Type: "NOTE", Fields: map[string]interface{}{"parentID": "n1"}}

	if _, err := exporter.ExportItem("user1", &model.Item{ID: "f1", Name: "工作", Type: "NOTE_FOLDER", Fields: map[string]interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.ExportItem("user1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.ExportItem("user1", child); err != nil {
		t.Fatal(err)
	}

	resolver.UpdateNode(TreeNode{ID: "n1", Name: "筆記B", ItemType: "NOTE", ParentID: strPtr("f1")})
	parent.Name = "筆記B"
	if _, err := exporter.ExportItem("user1", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.ExportItem("user1", child); err != nil {
		t.Fatal(err)
	}

	if fs.Exists("user1/NOTE/工作/筆記A") || fs.Exists("user1/NOTE/工作/筆記A.json") {
		t.Fatal("old parent projection should be removed")
	}
	if !fs.Exists("user1/NOTE/工作/筆記B/A評論.json") {
		t.Fatal("child container should move with parent rename")
	}
}
