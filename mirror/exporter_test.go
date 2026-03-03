package mirror

import (
	"encoding/json"
	"strings"
	"testing"
)

func newTestExporter() (*Exporter, *MemoryVaultFS) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "f2", FolderName: "會議紀錄", Type: "NOTE", ParentID: strPtr("f1")},
		{ID: "c1", FolderName: "美食清單", Type: "CARD", ParentID: nil},
		{ID: "ch1", FolderName: "月營收", Type: "CHART", ParentID: nil},
	})
	return NewExporter(fs, resolver), fs
}

func TestExportFolder_CreatesDirectoryAndJSON(t *testing.T) {
	exp, fs := newTestExporter()
	noteType := "NOTE"
	err := exp.ExportFolder("user1", FolderMeta{
		ID:         "f1",
		MemberID:   "user1",
		FolderName: "工作",
		Type:       &noteType,
		OrderAt:    strPtr("1709000000"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fs.Exists("user1/NOTE/工作") {
		t.Error("目錄應該存在")
	}
	data, err := fs.ReadFile("user1/NOTE/工作/_folder.json")
	if err != nil {
		t.Fatal("_folder.json 應該存在:", err)
	}
	if !strings.Contains(string(data), "工作") {
		t.Error("_folder.json 應包含 folderName")
	}
}

func TestExportFolder_NestedCreatesParentDirs(t *testing.T) {
	exp, fs := newTestExporter()
	noteType := "NOTE"
	// 先匯出父 Folder
	exp.ExportFolder("user1", FolderMeta{ID: "f1", MemberID: "user1", FolderName: "工作", Type: &noteType})
	// 匯出子 Folder
	err := exp.ExportFolder("user1", FolderMeta{
		ID: "f2", MemberID: "user1", FolderName: "會議紀錄", Type: &noteType, ParentID: strPtr("f1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fs.Exists("user1/NOTE/工作/會議紀錄") {
		t.Error("巢狀目錄應該存在")
	}
	if !fs.Exists("user1/NOTE/工作/會議紀錄/_folder.json") {
		t.Error("巢狀 _folder.json 應該存在")
	}
}

func TestExportNote_WritesMDFile(t *testing.T) {
	exp, fs := newTestExporter()
	err := exp.ExportNote("user1", NoteMeta{
		ID: "n1", ParentID: "f2", Title: "今日會議", USN: 3,
		CreatedAt: "1700000000000", UpdatedAt: "1709000000000",
	}, "<p>會議內容</p>")
	if err != nil {
		t.Fatal(err)
	}

	data, err := fs.ReadFile("user1/NOTE/工作/會議紀錄/今日會議.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "id: n1") {
		t.Error("應包含 frontmatter id")
	}
	if !strings.Contains(content, "會議內容") {
		t.Error("應包含筆記內容")
	}
}

func TestExportNote_UpdateExisting(t *testing.T) {
	exp, fs := newTestExporter()
	meta := NoteMeta{ID: "n1", ParentID: "f1", Title: "舊標題", USN: 1, CreatedAt: "0", UpdatedAt: "0"}

	exp.ExportNote("user1", meta, "<p>old</p>")

	meta.Title = "新標題"
	meta.USN = 2
	exp.ExportNote("user1", meta, "<p>new</p>")

	// 舊檔案應被覆寫（新標題產生新路徑，舊的應不存在）
	if fs.Exists("user1/NOTE/工作/舊標題.md") {
		t.Error("舊檔案應已不存在")
	}
	data, _ := fs.ReadFile("user1/NOTE/工作/新標題.md")
	if !strings.Contains(string(data), "usn: 2") {
		t.Error("新檔案應有更新的 USN")
	}
}

func TestExportCard_WritesJSONFile(t *testing.T) {
	exp, fs := newTestExporter()
	err := exp.ExportCard("user1", CardMeta{
		ID: "card1", ParentID: "c1", Name: "鼎泰豐",
		Fields: strPtr(`{"店名":"鼎泰豐"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := fs.ReadFile("user1/CARD/美食清單/鼎泰豐.json")
	if err != nil {
		t.Fatal(err)
	}
	var card CardMeta
	json.Unmarshal(data, &card)
	if card.Name != "鼎泰豐" {
		t.Errorf("card name: got %q, want %q", card.Name, "鼎泰豐")
	}
}

func TestExportChart_WritesJSONFile(t *testing.T) {
	exp, fs := newTestExporter()
	err := exp.ExportChart("user1", CardMeta{
		ID: "chart1", ParentID: "ch1", Name: "Q4報表",
		Fields: strPtr(`[{"月份":"10月","營收":50000}]`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if !fs.Exists("user1/CHART/月營收/Q4報表.json") {
		t.Error("Chart JSON 應該存在")
	}
}

func TestDeleteFolder_RemovesDirectory(t *testing.T) {
	exp, fs := newTestExporter()
	noteType := "NOTE"
	exp.ExportFolder("user1", FolderMeta{ID: "f1", MemberID: "user1", FolderName: "工作", Type: &noteType})
	exp.ExportNote("user1", NoteMeta{ID: "n1", ParentID: "f1", Title: "test", USN: 1, CreatedAt: "0", UpdatedAt: "0"}, "<p>x</p>")

	err := exp.DeleteFolder("user1", "f1")
	if err != nil {
		t.Fatal(err)
	}

	if fs.Exists("user1/NOTE/工作") {
		t.Error("目錄應已刪除")
	}
}

func TestDeleteNote_RemovesMDFile(t *testing.T) {
	exp, fs := newTestExporter()
	exp.ExportNote("user1", NoteMeta{ID: "n1", ParentID: "f1", Title: "test", USN: 1, CreatedAt: "0", UpdatedAt: "0"}, "<p>x</p>")

	err := exp.DeleteNote("user1", "n1", "test", "f1")
	if err != nil {
		t.Fatal(err)
	}

	if fs.Exists("user1/NOTE/工作/test.md") {
		t.Error("MD 檔案應已刪除")
	}
}
