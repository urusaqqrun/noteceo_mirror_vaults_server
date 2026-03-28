package executor

import (
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

func TestItemDataToItemDoc_BasicMapping(t *testing.T) {
	data := &mirror.ItemMirrorData{
		ID:       "item1",
		Name:     "測試",
		ItemType: "KANBAN",
		Fields:   map[string]interface{}{"color": "red", "size": float64(5)},
	}
	doc := itemDataToItemDoc(data, 10)

	if doc["_id"] != "item1" {
		t.Errorf("_id: got %v", doc["_id"])
	}
	if doc["name"] != "測試" {
		t.Errorf("name: got %v", doc["name"])
	}
	if doc["itemType"] != "KANBAN" {
		t.Errorf("itemType: got %v", doc["itemType"])
	}
	fields, ok := doc["fields"].(Doc)
	if !ok {
		t.Fatal("fields should be Doc")
	}
	if fields["color"] != "red" {
		t.Errorf("fields.color: got %v", fields["color"])
	}
	if _, ok := fields["updatedAt"]; !ok {
		t.Error("fields.updatedAt should be set")
	}
}

func TestItemDataToItemDoc_DoesNotMutateOriginal(t *testing.T) {
	originalFields := map[string]interface{}{"key": "value"}
	data := &mirror.ItemMirrorData{
		ID:       "item3",
		Name:     "test",
		ItemType: "NOTE",
		Fields:   originalFields,
	}
	doc := itemDataToItemDoc(data, 5)
	fields := doc["fields"].(Doc)
	fields["injected"] = "bad"

	if _, ok := originalFields["injected"]; ok {
		t.Fatal("itemDataToItemDoc should not share Fields map with original")
	}
}

func TestEnsureDocID_CreateWithEmptyID(t *testing.T) {
	doc := Doc{"_id": "", "itemType": "NOTE"}
	ensureDocID(doc, mirror.ImportActionCreate)
	id, ok := doc["_id"].(string)
	if !ok || id == "" {
		t.Fatal("ensureDocID should generate a non-empty _id for create action")
	}
	if len(id) != 24 {
		t.Fatalf("generated _id should be 24-char hex, got %q (len=%d)", id, len(id))
	}
}

func TestEnsureDocID_CreateWithExistingID(t *testing.T) {
	doc := Doc{"_id": "existing-id", "itemType": "NOTE"}
	ensureDocID(doc, mirror.ImportActionCreate)
	if doc["_id"] != "existing-id" {
		t.Fatalf("ensureDocID should not overwrite existing _id, got %q", doc["_id"])
	}
}

func TestEnsureDocID_UpdateWithEmptyID(t *testing.T) {
	doc := Doc{"_id": "", "itemType": "NOTE"}
	ensureDocID(doc, mirror.ImportActionUpdate)
	if doc["_id"] != "" {
		t.Fatal("ensureDocID should not generate _id for non-create actions")
	}
}
