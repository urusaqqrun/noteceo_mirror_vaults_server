package mirror

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
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
	exp.ExportFolder("user1", FolderMeta{ID: "f1", FolderName: "工作", Type: &noteType})
	// 匯出子 Folder
	err := exp.ExportFolder("user1", FolderMeta{
		ID: "f2", FolderName: "會議紀錄", Type: &noteType, ParentID: strPtr("f1"),
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

func TestExportCard_RemovesOldPathBySameID(t *testing.T) {
	exp, fs := newTestExporter()
	err := exp.ExportCard("user1", CardMeta{
		ID:       "card1",
		ParentID: "c1",
		Name:     "舊名稱",
		Fields:   strPtr(`{"店名":"鼎泰豐"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = exp.ExportCard("user1", CardMeta{
		ID:       "card1",
		ParentID: "c1",
		Name:     "新名稱",
		Fields:   strPtr(`{"店名":"鼎泰豐"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fs.Exists("user1/CARD/美食清單/舊名稱.json") {
		t.Fatal("舊 card 路徑應被清理")
	}
	if !fs.Exists("user1/CARD/美食清單/新名稱.json") {
		t.Fatal("新 card 路徑應存在")
	}
}

func TestExportFolder_RemovesOldPathBySameID(t *testing.T) {
	exp, fs := newTestExporter()
	noteType := "NOTE"
	oldFolderJSON := `{"id":"f1","folderName":"舊工作","type":"NOTE","usn":1,"createdAt":"0","updatedAt":"0"}`
	if err := fs.WriteFile("user1/NOTE/舊工作/_folder.json", []byte(oldFolderJSON)); err != nil {
		t.Fatal(err)
	}
	if err := exp.ExportFolder("user1", FolderMeta{
		ID:         "f1",
		FolderName: "工作",
		Type:       &noteType,
	}); err != nil {
		t.Fatal(err)
	}
	if fs.Exists("user1/NOTE/舊工作") {
		t.Fatal("舊 folder 路徑應被清理")
	}
	if !fs.Exists("user1/NOTE/工作/_folder.json") {
		t.Fatal("新 folder 路徑應存在")
	}
}

func TestDeleteFolder_RemovesDirectory(t *testing.T) {
	exp, fs := newTestExporter()
	noteType := "NOTE"
	exp.ExportFolder("user1", FolderMeta{ID: "f1", FolderName: "工作", Type: &noteType})
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

// --- 新格式 ExportItem / DeleteItem / resolveCollision ---

func newItemTestExporter() (*Exporter, *MemoryVaultFS) {
	fs := NewMemoryVaultFS()
	resolver := NewPathResolver([]FolderNode{
		{ID: "f1", FolderName: "工作", Type: "NOTE", ParentID: nil},
		{ID: "c1", FolderName: "看板", Type: "CARD", ParentID: nil},
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
			"folderID": "f1",
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
	resolver := NewPathResolver([]FolderNode{
		{ID: "f-new", FolderName: "新目錄", Type: "NOTE", ParentID: nil},
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
	if !fs.Exists("user1/NOTE/新目錄") {
		t.Error("directory should be created")
	}
	if !fs.Exists("user1/NOTE/新目錄/新目錄.json") {
		t.Error("folder metadata JSON should exist")
	}
}

func TestExportItem_EmptyName_UsesFallback(t *testing.T) {
	exp, fs := newItemTestExporter()
	item := &model.Item{
		ID:   "abc12345",
		Name: "",
		Type: "NOTE",
		Fields: map[string]interface{}{
			"folderID": "f1",
		},
	}
	_, err := exp.ExportItem("user1", item)
	if err != nil {
		t.Fatal(err)
	}
	expectedName := VaultFallbackName("abc12345")
	if !fs.Exists("user1/NOTE/工作/" + sanitizeName(expectedName) + ".json") {
		t.Errorf("should use fallback name %q", expectedName)
	}
}

func TestExportItem_ResolveCollision_DifferentID(t *testing.T) {
	exp, fs := newItemTestExporter()
	item1 := &model.Item{
		ID: "id_aaaabbbb", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"folderID": "f1"},
	}
	item2 := &model.Item{
		ID: "id_ccccdddd", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"folderID": "f1"},
	}
	if _, err := exp.ExportItem("user1", item1); err != nil {
		t.Fatal(err)
	}
	result, err := exp.ExportItem("user1", item2)
	if err != nil {
		t.Fatal(err)
	}
	// item2 的路徑應帶 id 後綴
	if !strings.Contains(result.Path, "_ccccdddd") {
		t.Errorf("collision should add id suffix, got path: %q", result.Path)
	}
	if !fs.Exists(result.Path) {
		t.Error("collision-resolved file should exist")
	}
}

func TestExportItem_ResolveCollision_SameID_Overwrites(t *testing.T) {
	exp, _ := newItemTestExporter()
	item := &model.Item{
		ID: "same-id", Name: "同名", Type: "NOTE",
		Fields: map[string]interface{}{"folderID": "f1"},
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
		Fields: map[string]interface{}{"folderID": "f1"},
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
		Fields: map[string]interface{}{"folderID": "f1"},
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
