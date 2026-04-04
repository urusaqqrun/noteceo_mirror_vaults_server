package mirror

import (
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/model"
)

// --- ItemToMirrorData 測試 ---

func TestItemToMirrorData_BasicConversion(t *testing.T) {
	item := &model.Item{
		ID:   "i1",
		Name: "我的看板",
		Type: "KANBAN",
		Fields: map[string]interface{}{
			"color":    "blue",
			"parentID": "f1",
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

func TestItemToMirrorData_EmptyNamePreserved(t *testing.T) {
	item := &model.Item{
		ID:     "abc123",
		Name:   "",
		Type:   "NOTE",
		Fields: map[string]interface{}{},
	}
	data := ItemToMirrorData(item)
	if data.Name != "" {
		t.Fatalf("empty name should be preserved (filename uses ID), got %q", data.Name)
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
