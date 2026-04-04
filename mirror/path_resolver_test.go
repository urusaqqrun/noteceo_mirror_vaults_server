package mirror

import (
	"testing"
)

func strPtr(s string) *string { return &s }

func TestResolvePath_TopLevel(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	got, err := r.ResolvePath("f1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作" {
		t.Errorf("got %q, want %q", got, "NOTE/工作")
	}
}

func TestResolvePath_Nested(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f2", Name: "會議紀錄", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	got, err := r.ResolvePath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/會議紀錄" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/會議紀錄")
	}
}

func TestResolvePath_DeeplyNested(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f2", Name: "專案", ItemType: "NOTE", ParentID: strPtr("f1")},
		{ID: "f3", Name: "前端", ItemType: "NOTE", ParentID: strPtr("f2")},
	})
	got, err := r.ResolvePath("f3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/專案/前端" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/專案/前端")
	}
}

func TestResolvePath_DifferentTypes(t *testing.T) {
	tests := []struct {
		name     string
		node     TreeNode
		expected string
	}{
		{"CARD root", TreeNode{ID: "c1", Name: "讀書卡片", ItemType: "CARD_FOLDER", ParentID: nil}, "CARD/讀書卡片"},
		{"CHART root", TreeNode{ID: "ch1", Name: "月報圖表", ItemType: "CHART_FOLDER", ParentID: nil}, "CHART/月報圖表"},
		{"TODO root", TreeNode{ID: "t1", Name: "待辦清單", ItemType: "TODO", ParentID: nil}, "TODO/待辦清單"},
		{"NOTE root", TreeNode{ID: "n1", Name: "筆記", ItemType: "NOTE", ParentID: nil}, "NOTE/筆記"},
		{"empty type defaults to NOTE", TreeNode{ID: "e1", Name: "雜記", ItemType: "", ParentID: nil}, "NOTE/雜記"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewPathResolver([]TreeNode{tt.node})
			got, err := r.ResolvePath(tt.node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestResolvePath_OrphanParentID(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "孤兒", ItemType: "NOTE", ParentID: strPtr("nonexistent")},
	})
	_, err := r.ResolvePath("f1")
	if err == nil {
		t.Fatal("expected error for orphan parent, got nil")
	}
}

func TestResolvePath_CircularReference(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "A", ItemType: "NOTE", ParentID: strPtr("f2")},
		{ID: "f2", Name: "B", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	_, err := r.ResolvePath("f1")
	if err == nil {
		t.Error("expected error for circular reference, got nil")
	}
}

func TestResolvePath_SelfReference(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "Self", ItemType: "TODO", ParentID: strPtr("f1")},
	})
	_, err := r.ResolvePath("f1")
	if err == nil {
		t.Error("expected error for self-reference, got nil")
	}
}

func TestResolvePath_NotFound(t *testing.T) {
	r := NewPathResolver([]TreeNode{})
	_, err := r.ResolvePath("nonexistent")
	if err == nil {
		t.Fatal("expected error for not found, got nil")
	}
}

func TestResolvePath_EmptyID(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	_, err := r.ResolvePath("")
	if err == nil {
		t.Fatal("expected error for empty ID, got nil")
	}
}

func TestAddNode_UpdatesTree(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	r.AddNode(TreeNode{ID: "f2", Name: "新專案", ItemType: "NOTE", ParentID: strPtr("f1")})

	got, err := r.ResolvePath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/新專案" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/新專案")
	}
}

func TestRemoveNode(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f2", Name: "會議", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	r.RemoveNode("f2")

	_, err := r.ResolvePath("f2")
	if err == nil {
		t.Error("expected error after removal, got nil")
	}
}

func TestUpdateNode(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f2", Name: "舊名", ItemType: "NOTE", ParentID: strPtr("f1")},
	})
	r.UpdateNode(TreeNode{ID: "f2", Name: "新名", ItemType: "NOTE", ParentID: strPtr("f1")})

	got, err := r.ResolvePath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/新名" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/新名")
	}
}

func TestResolvePath_SpecialCharactersInName(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作/專案", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	got, err := r.ResolvePath("f1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作_專案" {
		t.Errorf("got %q, want %q", got, "NOTE/工作_專案")
	}
}

func TestResolvePath_EmptyName_UsesID(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "", ItemType: "NOTE_FOLDER", ParentID: nil},
	})
	got, err := r.ResolvePath("f1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/f1" {
		t.Errorf("got %q, want %q", got, "NOTE/f1")
	}
}

func TestResolvePath_CacheInvalidation(t *testing.T) {
	r := NewPathResolver([]TreeNode{
		{ID: "f1", Name: "工作", ItemType: "NOTE_FOLDER", ParentID: nil},
		{ID: "f2", Name: "子目錄", ItemType: "NOTE", ParentID: strPtr("f1")},
	})

	_, _ = r.ResolvePath("f2")

	r.UpdateNode(TreeNode{ID: "f1", Name: "生活", ItemType: "NOTE_FOLDER", ParentID: nil})

	got, err := r.ResolvePath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/生活/子目錄" {
		t.Errorf("cache not invalidated: got %q, want %q", got, "NOTE/生活/子目錄")
	}
}
