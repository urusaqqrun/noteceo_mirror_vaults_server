package mirror

import (
	"slices"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

func TestItemToNoteMeta_TitleAndUpdatedAtFallback(t *testing.T) {
	item := &model.Item{
		ID:   "n1",
		Type: model.ItemTypeNote,
		Fields: map[string]interface{}{
			"name":      "來自舊欄位標題",
			"parentID":  "f1",
			"usn":       12,
			"createdAt": int64(1700000000000),
			"updateAt":  int64(1709000000000),
		},
	}

	meta, _ := ItemToNoteMeta(item)
	if meta.Title != "來自舊欄位標題" {
		t.Fatalf("title fallback failed: got %q", meta.Title)
	}
	if meta.UpdatedAt != "1709000000000" {
		t.Fatalf("updatedAt fallback failed: got %q", meta.UpdatedAt)
	}
}

func TestItemToFolderMeta_DecodeComplexArrays(t *testing.T) {
	item := &model.Item{
		ID:   "f1",
		Type: model.ItemTypeFolder,
		Fields: map[string]interface{}{
			"name": "工作",
			"usn":  5,
			"indexes": []interface{}{
				map[string]interface{}{
					"name":       "會議",
					"notes":      []interface{}{"n1", "n2"},
					"isReserved": true,
				},
			},
			"isSummarizedNoteIds": []interface{}{"n1"},
		},
	}

	meta := ItemToFolderMeta(item)
	if len(meta.Indexes) != 1 || meta.Indexes[0].Name != "會議" {
		t.Fatalf("indexes decode failed: %+v", meta.Indexes)
	}
	if len(meta.IsSummarizedNoteIds) != 1 || meta.IsSummarizedNoteIds[0] == nil || *meta.IsSummarizedNoteIds[0] != "n1" {
		t.Fatalf("isSummarizedNoteIds decode failed: %+v", meta.IsSummarizedNoteIds)
	}
}

func TestItemToFolderMeta_DecodeSummarizedIDsFromString(t *testing.T) {
	item := &model.Item{
		ID:   "f1",
		Type: model.ItemTypeFolder,
		Fields: map[string]interface{}{
			"name":                "工作",
			"usn":                 5,
			"isSummarizedNoteIds": "n-single",
		},
	}

	meta := ItemToFolderMeta(item)
	if len(meta.IsSummarizedNoteIds) != 1 || meta.IsSummarizedNoteIds[0] == nil || *meta.IsSummarizedNoteIds[0] != "n-single" {
		t.Fatalf("string summarized ids decode failed: %+v", meta.IsSummarizedNoteIds)
	}
}

func TestItemToFolderMeta_DecodeSummarizedIDsVariants(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected []string
	}{
		{
			name:     "string slice",
			value:    []string{"id1", "id2"},
			expected: []string{"id1", "id2"},
		},
		{
			name:     "interface slice",
			value:    []interface{}{"id1", "id2"},
			expected: []string{"id1", "id2"},
		},
		{
			name:     "plain string",
			value:    "id1",
			expected: []string{"id1"},
		},
		{
			name:     "json string array",
			value:    "[\"id1\",\"id2\"]",
			expected: []string{"id1", "id2"},
		},
		{
			name:     "empty string",
			value:    "",
			expected: nil,
		},
		{
			name:     "nil value",
			value:    nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[string]interface{}{
				"name": "工作",
				"usn":  1,
			}
			if tt.value != nil {
				fields["isSummarizedNoteIds"] = tt.value
			}

			meta := ItemToFolderMeta(&model.Item{
				ID:     "f1",
				Type:   model.ItemTypeFolder,
				Fields: fields,
			})
			got := make([]string, 0, len(meta.IsSummarizedNoteIds))
			for _, p := range meta.IsSummarizedNoteIds {
				if p != nil {
					got = append(got, *p)
				}
			}
			if !slices.Equal(got, tt.expected) {
				t.Fatalf("summarized ids mismatch: got %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestItemToFolderMeta_FolderTypeInference(t *testing.T) {
	tests := []struct {
		itemType string
		wantType *string
	}{
		{itemType: model.ItemTypeNoteFolder, wantType: strPtr("NOTE")},
		{itemType: model.ItemTypeCardFolder, wantType: strPtr("CARD")},
		{itemType: model.ItemTypeChartFolder, wantType: strPtr("CHART")},
		{itemType: model.ItemTypeTodoFolder, wantType: strPtr("TODO")},
		{itemType: model.ItemTypeFolder, wantType: nil},
	}

	for _, tt := range tests {
		t.Run(tt.itemType, func(t *testing.T) {
			meta := ItemToFolderMeta(&model.Item{
				ID:   "f1",
				Type: tt.itemType,
				Fields: map[string]interface{}{
					"name": "folder",
					"usn":  1,
				},
			})
			if tt.wantType == nil {
				if meta.Type != nil {
					t.Fatalf("type should be nil for %s, got %q", tt.itemType, *meta.Type)
				}
				return
			}
			if meta.Type == nil || *meta.Type != *tt.wantType {
				t.Fatalf("type inference mismatch for %s: got %v, want %q", tt.itemType, meta.Type, *tt.wantType)
			}
		})
	}
}

// --- 新格式 ItemToMirrorData 測試 ---

func TestItemToMirrorData_BasicConversion(t *testing.T) {
	item := &model.Item{
		ID:   "i1",
		Name: "我的看板",
		Type: "KANBAN",
		Fields: map[string]interface{}{
			"color":    "blue",
			"folderID": "f1",
		},
	}
	data := ItemToMirrorData(item)
	if data.ID != "i1" || data.Name != "我的看板" || data.ItemType != "KANBAN" {
		t.Fatalf("basic fields mismatch: %+v", data)
	}
	if data.Fields["color"] != "blue" {
		t.Fatalf("fields.color: got %v", data.Fields["color"])
	}
}

func TestItemToMirrorData_EmptyNameFallback(t *testing.T) {
	item := &model.Item{
		ID:     "abc123",
		Name:   "",
		Type:   "NOTE",
		Fields: map[string]interface{}{},
	}
	data := ItemToMirrorData(item)
	if data.Name != VaultFallbackName("abc123") {
		t.Fatalf("empty name should use fallback, got %q", data.Name)
	}
}

func TestItemToMirrorData_NilFieldsInit(t *testing.T) {
	item := &model.Item{ID: "x", Type: "NOTE"}
	data := ItemToMirrorData(item)
	if data.Fields == nil {
		t.Fatal("Fields should be initialized even when item.Fields is nil")
	}
}

func TestItemToMirrorData_DeepCopyFields(t *testing.T) {
	original := map[string]interface{}{"key": "original"}
	item := &model.Item{
		ID:     "x",
		Name:   "test",
		Type:   "NOTE",
		Fields: original,
	}
	data := ItemToMirrorData(item)
	data.Fields["key"] = "mutated"

	if original["key"] != "original" {
		t.Fatal("deep copy failed: modifying MirrorData.Fields should not affect original Item.Fields")
	}
}

func TestItemToNoteMeta_ParentIDAndUpdatedAtFallback(t *testing.T) {
	item := &model.Item{
		ID:   "n1",
		Type: model.ItemTypeNote,
		Fields: map[string]interface{}{
			"name":      "舊資料",
			"folderID":  "legacy-folder",
			"usn":       1,
			"createdAt": int64(1700000000000),
			"updateAt":  int64(1709000000000),
		},
	}
	meta, _ := ItemToNoteMeta(item)
	if meta.ParentID != "legacy-folder" {
		t.Fatalf("parentID fallback failed: got %q", meta.ParentID)
	}
	if meta.UpdatedAt != "1709000000000" {
		t.Fatalf("updatedAt fallback failed: got %q", meta.UpdatedAt)
	}
}
