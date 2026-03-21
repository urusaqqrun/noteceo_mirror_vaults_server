package mirror

import (
	"strings"
	"testing"
)

func TestNoteToMarkdown_BasicHTML(t *testing.T) {
	meta := NoteMeta{
		ID:        "note1",
		ParentID:  "f1",
		Title:     "測試筆記",
		USN:       5,
		CreatedAt: "1700000000000",
		UpdatedAt: "1709000000000",
	}
	html := "<h1>標題</h1><p>這是內容</p>"

	md, err := NoteToMarkdown(meta, html)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(md, "id: note1") {
		t.Error("frontmatter 應包含 id")
	}
	if !strings.Contains(md, "parentID: f1") {
		t.Error("frontmatter 應包含 parentID")
	}
	if !strings.Contains(md, "usn: 5") {
		t.Error("frontmatter 應包含 usn")
	}
	if !strings.Contains(md, "htmlHash:") {
		t.Error("frontmatter 應包含 htmlHash")
	}
	if !strings.Contains(md, "這是內容") {
		t.Error("body 應包含轉換後的 Markdown 內容")
	}
}

func TestNoteToMarkdown_WithTags(t *testing.T) {
	meta := NoteMeta{
		ID:        "note2",
		ParentID:  "f1",
		Title:     "有標籤",
		USN:       1,
		Tags:      []string{"工作", "會議"},
		CreatedAt: "1700000000000",
		UpdatedAt: "1709000000000",
	}
	md, err := NoteToMarkdown(meta, "<p>content</p>")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "tags:") {
		t.Error("frontmatter 應包含 tags")
	}
	if !strings.Contains(md, "工作") {
		t.Error("tags 應包含「工作」")
	}
}

func TestNoteToMarkdown_EmptyHTML(t *testing.T) {
	meta := NoteMeta{ID: "note3", ParentID: "f1", Title: "空", USN: 1, CreatedAt: "0", UpdatedAt: "0"}
	md, err := NoteToMarkdown(meta, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "---") {
		t.Error("即使內容為空也應有 frontmatter")
	}
}

func TestNoteToMarkdown_HTMLHashConsistency(t *testing.T) {
	meta := NoteMeta{ID: "note4", ParentID: "f1", Title: "hash test", USN: 1, CreatedAt: "0", UpdatedAt: "0"}
	html := "<p>same content</p>"

	md1, _ := NoteToMarkdown(meta, html)
	md2, _ := NoteToMarkdown(meta, html)

	// 相同 HTML 應產生相同 hash
	hash1 := extractFrontmatterField(md1, "htmlHash")
	hash2 := extractFrontmatterField(md2, "htmlHash")
	if hash1 != hash2 {
		t.Errorf("same HTML should produce same hash: %q vs %q", hash1, hash2)
	}

	// 不同 HTML 應產生不同 hash
	md3, _ := NoteToMarkdown(meta, "<p>different content</p>")
	hash3 := extractFrontmatterField(md3, "htmlHash")
	if hash1 == hash3 {
		t.Error("different HTML should produce different hash")
	}
}

func TestMarkdownToNote_ParseFrontmatter(t *testing.T) {
	md := `---
id: note1
parentID: f1
title: 測試筆記
usn: 5
htmlHash: abc123
createdAt: "1700000000000"
updatedAt: "1709000000000"
tags:
  - 工作
  - 會議
---

# 標題

這是內容
`
	meta, body, err := MarkdownToNote(md)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "note1" {
		t.Errorf("ID: got %q, want %q", meta.ID, "note1")
	}
	if meta.ParentID != "f1" {
		t.Errorf("ParentID: got %q, want %q", meta.ParentID, "f1")
	}
	if meta.USN != 5 {
		t.Errorf("USN: got %d, want %d", meta.USN, 5)
	}
	if meta.HTMLHash != "abc123" {
		t.Errorf("HTMLHash: got %q, want %q", meta.HTMLHash, "abc123")
	}
	if len(meta.Tags) != 2 {
		t.Errorf("Tags: got %d items, want 2", len(meta.Tags))
	}
	if !strings.Contains(body, "標題") {
		t.Error("body 應包含內容")
	}
}

func TestMarkdownToNote_NoFrontmatter(t *testing.T) {
	md := "# 純 Markdown\n\n沒有 frontmatter 的筆記"

	meta, body, err := MarkdownToNote(md)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "" {
		t.Errorf("沒有 frontmatter 時 ID 應為空，got %q", meta.ID)
	}
	if !strings.Contains(body, "純 Markdown") {
		t.Error("body 應包含完整內容")
	}
}

func TestFolderToJSON_NoteType(t *testing.T) {
	noteType := "NOTE"
	data, err := FolderToJSON(FolderMeta{
		ID:         "f1",
		FolderName: "工作筆記",
		Type:       &noteType,
		OrderAt:    strPtr("1709000003000"),
		Icon:       strPtr("📁"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"id": "f1"`) {
		t.Error("JSON 應輸出小寫 id 欄位")
	}
	if !strings.Contains(s, "工作筆記") {
		t.Error("JSON 應包含 folderName")
	}
	if !strings.Contains(s, "NOTE") {
		t.Error("JSON 應包含 type")
	}
}

func TestFolderToJSON_CardType(t *testing.T) {
	cardType := "CARD"
	fields := []CardFieldMeta{{Name: "書名", Type: "TEXT"}}
	data, err := FolderToJSON(FolderMeta{
		ID:         "c1",
		FolderName: "讀書卡片",
		Type:       &cardType,
		Fields:     fields,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "書名") {
		t.Error("CARD JSON 應包含 fields")
	}
}

func TestJSONToFolder_Basic(t *testing.T) {
	json := `{"ID":"f1","folderName":"工作","type":"NOTE","orderAt":"123"}`
	folder, err := JSONToFolder([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	if folder.ID != "f1" {
		t.Errorf("ID: got %q, want %q", folder.ID, "f1")
	}
	if folder.FolderName != "工作" {
		t.Errorf("FolderName: got %q, want %q", folder.FolderName, "工作")
	}
}

func TestJSONToFolder_MissingFields(t *testing.T) {
	json := `{"ID":"f1","folderName":"最小"}`
	folder, err := JSONToFolder([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	if folder.ID != "f1" {
		t.Errorf("ID: got %q, want %q", folder.ID, "f1")
	}
}

func TestCardToJSON(t *testing.T) {
	data, err := CardToJSON(CardMeta{
		ID:       "card1",
		ParentID: "f1",
		Name:     "鼎泰豐",
		Fields:   strPtr(`{"店名":"鼎泰豐"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "鼎泰豐") {
		t.Error("Card JSON 應包含 name")
	}
}

func TestJSONToCard(t *testing.T) {
	json := `{"id":"card1","parentID":"f1","name":"鼎泰豐","fields":"{\"店名\":\"鼎泰豐\"}"}`
	card, err := JSONToCard([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "鼎泰豐" {
		t.Errorf("Name: got %q, want %q", card.Name, "鼎泰豐")
	}
}

// --- 新格式 ItemMirrorData 序列化 / 反序列化 ---

func TestVaultFallbackName(t *testing.T) {
	got := VaultFallbackName("69a722717998c644cb2610a0")
	if got != "untitled_69a722717998c644cb2610a0" {
		t.Errorf("got %q, want %q", got, "untitled_69a722717998c644cb2610a0")
	}
}

func TestIsVaultFallbackName(t *testing.T) {
	id := "abc123"
	if !IsVaultFallbackName("untitled_abc123", id) {
		t.Error("should match fallback name")
	}
	if IsVaultFallbackName("my-note", id) {
		t.Error("should not match non-fallback name")
	}
	if IsVaultFallbackName("untitled_xyz", id) {
		t.Error("should not match fallback for different id")
	}
}

func TestItemToMirrorJSON_Roundtrip(t *testing.T) {
	data := ItemMirrorData{
		ID:       "item1",
		Name:     "測試項目",
		ItemType: "KANBAN",
		Fields:   map[string]interface{}{"color": "red", "count": float64(42)},
	}
	jsonBytes, err := ItemToMirrorJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := MirrorJSONToItem(jsonBytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != data.ID || parsed.Name != data.Name || parsed.ItemType != data.ItemType {
		t.Errorf("roundtrip mismatch: got %+v", parsed)
	}
	if parsed.Fields["color"] != "red" {
		t.Errorf("fields.color: got %v", parsed.Fields["color"])
	}
}

func TestMirrorJSONToItem_MissingID(t *testing.T) {
	_, err := MirrorJSONToItem([]byte(`{"name":"test","itemType":"NOTE","fields":{}}`))
	if err == nil {
		t.Error("should error when id is missing")
	}
}

func TestMirrorJSONToItem_MissingItemType(t *testing.T) {
	_, err := MirrorJSONToItem([]byte(`{"id":"x","name":"test","fields":{}}`))
	if err == nil {
		t.Error("should error when itemType is missing")
	}
}

func TestMirrorJSONToItem_InvalidJSON(t *testing.T) {
	_, err := MirrorJSONToItem([]byte(`not valid json`))
	if err == nil {
		t.Error("should error on invalid json")
	}
}

// extractFrontmatterField 從 markdown 中提取 frontmatter 欄位值
func extractFrontmatterField(md, field string) string {
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, field+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, field+":"))
		}
	}
	return ""
}
