package mirror

import (
	"testing"
)

func strPtr(s string) *string { return &s }

func TestResolveFolderPath_TopLevel(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
	})
	got, err := r.ResolveFolderPath("f1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作" {
		t.Errorf("got %q, want %q", got, "NOTE/工作")
	}
}

func TestResolveFolderPath_Nested(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "會議紀錄", Type: "NOTE", ParentID: strPtr("f1")},
	})
	got, err := r.ResolveFolderPath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/會議紀錄" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/會議紀錄")
	}
}

func TestResolveFolderPath_DeeplyNested(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "專案", Type: "NOTE", ParentID: strPtr("f1")},
		{ID: "f3", FolderName: "前端", Type: "NOTE", ParentID: strPtr("f2")},
	})
	got, err := r.ResolveFolderPath("f3")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/專案/前端" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/專案/前端")
	}
}

func TestResolveFolderPath_DifferentTypes(t *testing.T) {
	tests := []struct {
		name     string
		folder   FolderNode
		expected string
	}{
		{"CARD", FolderNode{ID: "c1", FolderName: "讀書卡片", Type: "CARD", ParentID: nil}, "CARD/讀書卡片"},
		{"CHART", FolderNode{ID: "ch1", FolderName: "月報圖表", Type: "CHART", ParentID: nil}, "CHART/月報圖表"},
		{"TODO", FolderNode{ID: "t1", FolderName: "待辦清單", Type: "TODO", ParentID: nil}, "TODO/待辦清單"},
		{"NOTE", FolderNode{ID: "n1", FolderName: "筆記", Type: "NOTE", ParentID: nil}, "NOTE/筆記"},
		{"empty type defaults to NOTE", FolderNode{ID: "e1", FolderName: "雜記", Type: "", ParentID: nil}, "NOTE/雜記"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewPathResolver([]FolderNode{tt.folder})
			got, err := r.ResolveFolderPath(tt.folder.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestResolveNotePath(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "會議紀錄", Type: "NOTE", ParentID: strPtr("f1")},
	})
	got, err := r.ResolveNotePath("今日會議", "f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/會議紀錄/今日會議.md" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/會議紀錄/今日會議.md")
	}
}

func TestResolveCardPath(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "c1", FolderName: "美食清單", Type: "CARD", ParentID: nil},
	})
	got, err := r.ResolveCardPath("鼎泰豐", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "CARD/美食清單/鼎泰豐.json" {
		t.Errorf("got %q, want %q", got, "CARD/美食清單/鼎泰豐.json")
	}
}

func TestResolveChartPath(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "ch1", FolderName: "月營收圖表", Type: "CHART", ParentID: nil},
	})
	got, err := r.ResolveChartPath("2024年Q4", "ch1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "CHART/月營收圖表/2024年Q4.json" {
		t.Errorf("got %q, want %q", got, "CHART/月營收圖表/2024年Q4.json")
	}
}

func TestResolveFolderPath_OrphanParentID(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "孤兒", Type: "NOTE", ParentID: strPtr("nonexistent")},
	})
	_, err := r.ResolveFolderPath("f1")
	if err == nil {
		t.Error("expected error for orphan parentID, got nil")
	}
}

func TestResolveFolderPath_CircularReference(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "A", Type: "NOTE", ParentID: strPtr("f2")},
		{ID: "f2", FolderName: "B", Type: "NOTE", ParentID: strPtr("f1")},
	})
	_, err := r.ResolveFolderPath("f1")
	if err == nil {
		t.Error("expected error for circular reference, got nil")
	}
}

func TestResolveFolderPath_NotFound(t *testing.T) {
	r := NewPathResolver([]FolderNode{})
	got, err := r.ResolveFolderPath("nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "_unsorted" {
		t.Errorf("got %q, want %q", got, "_unsorted")
	}
}

func TestResolveFolderPath_EmptyID(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
	})
	got, err := r.ResolveFolderPath("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "_unsorted" {
		t.Errorf("got %q, want %q", got, "_unsorted")
	}
}

func TestAddFolder_UpdatesTree(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
	})
	r.AddFolder(FolderNode{ID: "f2", FolderName: "新專案", Type: "NOTE", ParentID: strPtr("f1")})

	got, err := r.ResolveFolderPath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/新專案" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/新專案")
	}
}

func TestRemoveFolder(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "會議", Type: "NOTE", ParentID: strPtr("f1")},
	})
	r.RemoveFolder("f2")

	got, err := r.ResolveFolderPath("f2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "_unsorted" {
		t.Errorf("got %q, want %q (should fallback after removal)", got, "_unsorted")
	}
}

func TestUpdateFolder(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "舊名", Type: "NOTE", ParentID: strPtr("f1")},
	})
	r.UpdateFolder(FolderNode{ID: "f2", FolderName: "新名", Type: "NOTE", ParentID: strPtr("f1")})

	got, err := r.ResolveFolderPath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/工作/新名" {
		t.Errorf("got %q, want %q", got, "NOTE/工作/新名")
	}
}

func TestResolveFolderPath_SpecialCharactersInName(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作/專案", Type: "NOTE", ParentID: nil},
	})
	got, err := r.ResolveFolderPath("f1")
	if err != nil {
		t.Fatal(err)
	}
	// 斜線應被替換，避免路徑衝突
	if got != "NOTE/工作_專案" {
		t.Errorf("got %q, want %q", got, "NOTE/工作_專案")
	}
}

func TestResolveFolderPath_CacheInvalidation(t *testing.T) {
	r := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "子目錄", Type: "NOTE", ParentID: strPtr("f1")},
	})

	// 先解析一次，快取結果
	_, _ = r.ResolveFolderPath("f2")

	// 更新父 Folder 名稱
	r.UpdateFolder(FolderNode{ID: "f1", FolderName: "生活", Type: "NOTE", ParentID: nil})

	got, err := r.ResolveFolderPath("f2")
	if err != nil {
		t.Fatal(err)
	}
	if got != "NOTE/生活/子目錄" {
		t.Errorf("cache not invalidated: got %q, want %q", got, "NOTE/生活/子目錄")
	}
}
