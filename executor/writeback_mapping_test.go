package executor

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

func TestFolderMetaToItemBson_PreservesFolderItemType(t *testing.T) {
	noteType := "NOTE"
	doc := folderMetaToItemBson(&mirror.FolderMeta{
		ID:         "f1",
		MemberID:   "u1",
		FolderName: "資料夾",
		Type:       &noteType,
	}, "NOTE_FOLDER", 10)

	if got, _ := doc["itemType"].(string); got != "NOTE_FOLDER" {
		t.Fatalf("itemType mismatch: got %q, want %q", got, "NOTE_FOLDER")
	}
}

func TestFolderMetaToItemBson_InvalidTypeFallsBack(t *testing.T) {
	doc := folderMetaToItemBson(&mirror.FolderMeta{
		ID:         "f1",
		MemberID:   "u1",
		FolderName: "資料夾",
	}, "UNKNOWN", 0)

	if got, _ := doc["itemType"].(string); got != "FOLDER" {
		t.Fatalf("itemType fallback mismatch: got %q, want %q", got, "FOLDER")
	}
}

func TestChartAndCardLegacyBson_IncludeIsDeleted(t *testing.T) {
	deleted := true
	cardDoc := cardMetaToBson(&mirror.CardMeta{
		ID:        "c1",
		ParentID:  "p1",
		Name:      "card",
		USN:       1,
		IsDeleted: deleted,
	})
	chartDoc := chartMetaToBson(&mirror.CardMeta{
		ID:        "h1",
		ParentID:  "p1",
		Name:      "chart",
		USN:       1,
		IsDeleted: deleted,
	})

	if got, _ := cardDoc["isDeleted"].(bool); !got {
		t.Fatal("card bson should include isDeleted=true")
	}
	if got, _ := chartDoc["isDeleted"].(bool); !got {
		t.Fatal("chart bson should include isDeleted=true")
	}
}

func TestEnsureDocID_CreateWithEmptyID(t *testing.T) {
	doc := bson.M{"_id": "", "itemType": "NOTE"}
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
	doc := bson.M{"_id": "existing-id", "itemType": "NOTE"}
	ensureDocID(doc, mirror.ImportActionCreate)
	if doc["_id"] != "existing-id" {
		t.Fatalf("ensureDocID should not overwrite existing _id, got %q", doc["_id"])
	}
}

func TestEnsureDocID_UpdateWithEmptyID(t *testing.T) {
	doc := bson.M{"_id": "", "itemType": "NOTE"}
	ensureDocID(doc, mirror.ImportActionUpdate)
	if doc["_id"] != "" {
		t.Fatal("ensureDocID should not generate _id for non-create actions")
	}
}
