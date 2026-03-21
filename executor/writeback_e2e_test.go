package executor

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
)

type mockWriter struct {
	mu sync.Mutex

	upsertFolderDocs []Doc
	upsertNoteDocs   []Doc
	upsertCardDocs   []Doc
	upsertChartDocs  []Doc
	upsertItemDocs   []Doc

	deleteItemIDs []string
	deleteDocs    []struct {
		collection string
		docID      string
	}
}

func (m *mockWriter) UpsertFolder(_ context.Context, _ string, doc Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertFolderDocs = append(m.upsertFolderDocs, cloneDoc(doc))
	return nil
}

func (m *mockWriter) UpsertNote(_ context.Context, _ string, doc Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertNoteDocs = append(m.upsertNoteDocs, cloneDoc(doc))
	return nil
}

func (m *mockWriter) UpsertCard(_ context.Context, _ string, doc Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertCardDocs = append(m.upsertCardDocs, cloneDoc(doc))
	return nil
}

func (m *mockWriter) UpsertChart(_ context.Context, _ string, doc Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertChartDocs = append(m.upsertChartDocs, cloneDoc(doc))
	return nil
}

func (m *mockWriter) UpsertItem(_ context.Context, _ string, doc Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upsertItemDocs = append(m.upsertItemDocs, cloneDoc(doc))
	return nil
}

func (m *mockWriter) DeleteItemDoc(_ context.Context, _ string, docID string, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteItemIDs = append(m.deleteItemIDs, docID)
	return nil
}

func (m *mockWriter) DeleteDocument(_ context.Context, _ string, collection, docID string, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteDocs = append(m.deleteDocs, struct {
		collection string
		docID      string
	}{collection: collection, docID: docID})
	return nil
}

type mockUSNReader struct {
	byDocID map[string]int
}

func (m *mockUSNReader) GetDocUSN(_ context.Context, _ string, _ string, docID string) (int, error) {
	if usn, ok := m.byDocID[docID]; ok {
		return usn, nil
	}
	return 0, nil
}

type mockUSNIncrementer struct {
	mu    sync.Mutex
	next  int
	calls int
}

func (m *mockUSNIncrementer) IncrementUSN(_ context.Context, _ string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	m.calls++
	return m.next, nil
}

func TestE2E_FullRoundTrip_ExportDiffImportWriteback(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	resolver := mirror.NewPathResolver([]mirror.FolderNode{
		{ID: "f-note", FolderName: "工作", Type: "NOTE"},
		{ID: "f-card", FolderName: "卡片", Type: "CARD"},
		{ID: "f-chart", FolderName: "圖表", Type: "CHART"},
	})
	exporter := mirror.NewExporter(fs, resolver)

	noteType := "NOTE"
	if err := exporter.ExportFolder("user1", mirror.FolderMeta{ID: "f-note", FolderName: "工作", Type: &noteType}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportNote("user1", mirror.NoteMeta{ID: "n1", ParentID: "f-note", Title: "會議A", USN: 1}, "<p>A</p>"); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportNote("user1", mirror.NoteMeta{ID: "n2", ParentID: "f-note", Title: "會議B", USN: 1}, "<p>B</p>"); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportCard("user1", mirror.CardMeta{ID: "c1", ParentID: "f-card", Name: "卡片A", Fields: strPtr(`{"k":"v"}`), USN: 1}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportChart("user1", mirror.CardMeta{ID: "h1", ParentID: "f-chart", Name: "圖表A", Fields: strPtr(`{"series":[1]}`), USN: 1}); err != nil {
		t.Fatal(err)
	}

	beforeSnap, err := TakeSnapshot(fs, "user1")
	if err != nil {
		t.Fatal(err)
	}

	if err := exporter.ExportNote("user1", mirror.NoteMeta{ID: "n1", ParentID: "f-note", Title: "會議A", USN: 2}, "<p>A modified</p>"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("user1/NOTE/工作/ai-new.md", []byte("AI created content without frontmatter")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("user1/NOTE/工作/會議B.md"); err != nil {
		t.Fatal(err)
	}

	afterSnap, err := TakeSnapshot(fs, "user1")
	if err != nil {
		t.Fatal(err)
	}
	diff := ComputeDiff(beforeSnap, afterSnap)

	importer := mirror.NewImporter(fs)
	entries, err := importer.ProcessDiff(
		"user1",
		diff.Created,
		diff.Modified,
		diff.Deleted,
		toMovedEntries(diff.Moved),
		map[string]string{"NOTE/工作/會議B.md": "n2"},
	)
	if err != nil {
		t.Fatal(err)
	}

	writer := &mockWriter{}
	result := WriteBack(context.Background(), writer, nil, nil, "user1", entries, 0)
	if result.Errors != 0 {
		t.Fatalf("errors: got %d, want 0", result.Errors)
	}
	if result.Created != 1 || result.Updated != 1 || result.Deleted != 1 {
		t.Fatalf("unexpected writeback result: %+v", result)
	}
	if !slices.Contains(writer.deleteItemIDs, "n2") {
		t.Fatalf("deleted IDs should contain n2, got %v", writer.deleteItemIDs)
	}

	var hasGeneratedCreate bool
	for _, doc := range writer.upsertItemDocs {
		itemType, _ := doc["itemType"].(string)
		id, _ := doc["_id"].(string)
		if itemType == "NOTE" && len(id) == 24 && id != "n1" && id != "n2" {
			hasGeneratedCreate = true
		}
	}
	if !hasGeneratedCreate {
		t.Fatal("expected a created NOTE item with auto-generated 24-char _id")
	}
}

func TestE2E_AICreateNotes_NoFrontmatterID(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	if err := fs.WriteFile("user1/NOTE/plain.md", []byte("plain markdown without frontmatter")); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("user1/NOTE/no-id.md", []byte("---\ntitle: no-id\nparentID: f1\n---\n\nbody")); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("user1/NOTE/empty-id.md", []byte("---\nid: \"\"\ntitle: empty-id\nparentID: f1\n---\n\nbody")); err != nil {
		t.Fatal(err)
	}

	importer := mirror.NewImporter(fs)
	entries, err := importer.ProcessDiff("user1", []string{
		"NOTE/plain.md",
		"NOTE/no-id.md",
		"NOTE/empty-id.md",
	}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	writer := &mockWriter{}
	result := WriteBack(context.Background(), writer, nil, nil, "user1", entries, 0)
	if result.Errors != 0 {
		t.Fatalf("errors: got %d, want 0", result.Errors)
	}
	if result.Created != 3 {
		t.Fatalf("created: got %d, want 3", result.Created)
	}
	if len(writer.upsertItemDocs) != 3 {
		t.Fatalf("upsert item docs: got %d, want 3", len(writer.upsertItemDocs))
	}
	for _, doc := range writer.upsertItemDocs {
		id, _ := doc["_id"].(string)
		if len(id) != 24 {
			t.Fatalf("expected generated 24-char _id, got %q", id)
		}
	}
}

func TestE2E_CardChartRoundTrip(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	resolver := mirror.NewPathResolver([]mirror.FolderNode{
		{ID: "f-card", FolderName: "卡片", Type: "CARD"},
		{ID: "f-chart", FolderName: "圖表", Type: "CHART"},
	})
	exporter := mirror.NewExporter(fs, resolver)
	if err := exporter.ExportCard("user1", mirror.CardMeta{ID: "c1", ParentID: "f-card", Name: "任務卡", Fields: strPtr(`{"status":"old"}`), Reviews: strPtr("[]"), Coordinates: strPtr("{}"), USN: 1}); err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportChart("user1", mirror.CardMeta{ID: "h1", ParentID: "f-chart", Name: "週報圖", Fields: strPtr(`{"series":[1,2]}`), USN: 1}); err != nil {
		t.Fatal(err)
	}
	beforeSnap, err := TakeSnapshot(fs, "user1")
	if err != nil {
		t.Fatal(err)
	}

	if err := exporter.ExportCard("user1", mirror.CardMeta{ID: "c1", ParentID: "f-card", Name: "任務卡", Fields: strPtr(`{"status":"new"}`), Reviews: strPtr(`["r1"]`), Coordinates: strPtr(`{"x":1}`), USN: 2}); err != nil {
		t.Fatal(err)
	}
	newCardRaw, _ := json.Marshal(map[string]string{
		"parentID": "f-card",
		"name":     "新卡片",
		"fields":   `{"priority":"p1"}`,
	})
	if err := fs.WriteFile("user1/CARD/卡片/新卡片.json", newCardRaw); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("user1/CHART/圖表/週報圖.json"); err != nil {
		t.Fatal(err)
	}

	afterSnap, err := TakeSnapshot(fs, "user1")
	if err != nil {
		t.Fatal(err)
	}
	diff := ComputeDiff(beforeSnap, afterSnap)
	importer := mirror.NewImporter(fs)
	entries, err := importer.ProcessDiff(
		"user1",
		diff.Created,
		diff.Modified,
		diff.Deleted,
		toMovedEntries(diff.Moved),
		map[string]string{"CHART/圖表/週報圖.json": "h1"},
	)
	if err != nil {
		t.Fatal(err)
	}

	writer := &mockWriter{}
	result := WriteBack(context.Background(), writer, nil, nil, "user1", entries, 0)
	if result.Errors != 0 {
		t.Fatalf("errors: got %d, want 0", result.Errors)
	}
	if result.Created != 1 || result.Updated != 1 || result.Deleted != 1 {
		t.Fatalf("unexpected writeback result: %+v", result)
	}
	if !slices.Contains(writer.deleteItemIDs, "h1") {
		t.Fatalf("deleted IDs should contain h1, got %v", writer.deleteItemIDs)
	}

	var hasUpdatedCard bool
	var hasCreatedCard bool
	for _, doc := range writer.upsertItemDocs {
		itemType, _ := doc["itemType"].(string)
		id, _ := doc["_id"].(string)
		fields := docToMap(doc["fields"])
		if itemType == "CARD" && id == "c1" {
			if got, _ := fields["fields"].(string); got == `{"status":"new"}` {
				hasUpdatedCard = true
			}
		}
		if itemType == "CARD" && id != "c1" && len(id) == 24 {
			hasCreatedCard = true
		}
	}
	if !hasUpdatedCard {
		t.Fatal("expected updated card fields to be written back")
	}
	if !hasCreatedCard {
		t.Fatal("expected created card with auto-generated _id")
	}
}

func TestE2E_ConcurrentConflict_MixedUSN(t *testing.T) {
	entries := []mirror.ImportEntry{
		{Action: mirror.ImportActionUpdate, Collection: "item", NoteMeta: &mirror.NoteMeta{ID: "n1", ParentID: "f1", Title: "n1"}, NoteBody: "n1"},
		{Action: mirror.ImportActionUpdate, Collection: "item", NoteMeta: &mirror.NoteMeta{ID: "n2", ParentID: "f1", Title: "n2"}, NoteBody: "n2"},
		{Action: mirror.ImportActionUpdate, Collection: "item", NoteMeta: &mirror.NoteMeta{ID: "n3", ParentID: "f1", Title: "n3"}, NoteBody: "n3"},
		{Action: mirror.ImportActionUpdate, Collection: "item", NoteMeta: &mirror.NoteMeta{ID: "n4", ParentID: "f1", Title: "n4"}, NoteBody: "n4"},
		{Action: mirror.ImportActionUpdate, Collection: "item", NoteMeta: &mirror.NoteMeta{ID: "n5", ParentID: "f1", Title: "n5"}, NoteBody: "n5"},
	}
	reader := &mockUSNReader{
		byDocID: map[string]int{
			"n1": 9,
			"n2": 7,
			"n3": 5,
			"n4": 4,
			"n5": 1,
		},
	}
	writer := &mockWriter{}
	result := WriteBack(context.Background(), writer, reader, nil, "user1", entries, 5)

	if result.Errors != 0 {
		t.Fatalf("errors: got %d, want 0", result.Errors)
	}
	if result.Skipped != 2 || result.Updated != 3 {
		t.Fatalf("unexpected conflict result: %+v", result)
	}
	if len(writer.upsertItemDocs) != 3 {
		t.Fatalf("upsert count: got %d, want 3", len(writer.upsertItemDocs))
	}
}

func TestE2E_MixedBatchOperations(t *testing.T) {
	entries := []mirror.ImportEntry{
		{
			Action:     mirror.ImportActionCreate,
			Collection: "folder",
			FolderMeta: &mirror.FolderMeta{ID: "f-new", FolderName: "新資料夾", USN: 1},
		},
		{
			Action:     mirror.ImportActionCreate,
			Collection: "note",
			NoteMeta:   &mirror.NoteMeta{ID: "n-front", ParentID: "f-new", Title: "WithID", USN: 1},
			NoteBody:   "hello",
		},
		{
			Action:     mirror.ImportActionCreate,
			Collection: "note",
			NoteMeta:   &mirror.NoteMeta{ID: "", ParentID: "f-new", Title: "NoID", USN: 1},
			NoteBody:   "created by AI",
		},
		{
			Action:     mirror.ImportActionUpdate,
			Collection: "note",
			NoteMeta:   &mirror.NoteMeta{ID: "n-up", ParentID: "f-new", Title: "Updated", USN: 1},
			NoteBody:   "updated body",
		},
		{
			Action:     mirror.ImportActionMove,
			Collection: "note",
			NoteMeta:   &mirror.NoteMeta{ID: "n-move", ParentID: "f-new", Title: "Moved", USN: 1},
			NoteBody:   "moved body",
		},
		{
			Action:     mirror.ImportActionDelete,
			Collection: "item",
			DocID:      "n-delete",
		},
		{
			Action:     mirror.ImportActionCreate,
			Collection: "card",
			CardMeta:   &mirror.CardMeta{ID: "c-new", ParentID: "f-card", Name: "CardNew", Fields: strPtr(`{"a":1}`), USN: 1},
		},
		{
			Action:     mirror.ImportActionUpdate,
			Collection: "chart",
			CardMeta:   &mirror.CardMeta{ID: "h-up", ParentID: "f-chart", Name: "ChartUp", Fields: strPtr(`{"series":[9]}`), USN: 1},
		},
	}
	writer := &mockWriter{}
	usnInc := &mockUSNIncrementer{next: 200}
	result := WriteBack(context.Background(), writer, nil, usnInc, "user1", entries, 0)

	if result.Errors != 0 {
		t.Fatalf("errors: got %d, want 0", result.Errors)
	}
	if result.Created != 4 || result.Updated != 2 || result.Moved != 1 || result.Deleted != 1 {
		t.Fatalf("unexpected mixed batch result: %+v", result)
	}
	if usnInc.calls != 8 {
		t.Fatalf("usn increment calls: got %d, want 8", usnInc.calls)
	}
	if len(writer.upsertFolderDocs) != 1 || len(writer.upsertNoteDocs) != 4 || len(writer.upsertCardDocs) != 1 || len(writer.upsertChartDocs) != 1 {
		t.Fatalf("unexpected write distribution: folders=%d notes=%d cards=%d charts=%d",
			len(writer.upsertFolderDocs), len(writer.upsertNoteDocs), len(writer.upsertCardDocs), len(writer.upsertChartDocs))
	}
	if len(writer.deleteItemIDs) != 1 || writer.deleteItemIDs[0] != "n-delete" {
		t.Fatalf("unexpected delete calls: %v", writer.deleteItemIDs)
	}

	var generatedNoID bool
	for _, doc := range writer.upsertNoteDocs {
		title, _ := doc["title"].(string)
		id, _ := doc["_id"].(string)
		if title == "NoID" && len(id) == 24 {
			generatedNoID = true
		}
	}
	if !generatedNoID {
		t.Fatal("expected note NoID to receive generated _id")
	}
}

func toMovedEntries(moved []MovedFile) []mirror.MovedFileEntry {
	out := make([]mirror.MovedFileEntry, 0, len(moved))
	for _, m := range moved {
		out = append(out, mirror.MovedFileEntry{
			OldPath: m.OldPath,
			NewPath: m.NewPath,
		})
	}
	return out
}

func cloneDoc(doc Doc) Doc {
	raw, err := json.Marshal(doc)
	if err != nil {
		return doc
	}
	var cloned Doc
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return doc
	}
	return cloned
}

func strPtr(s string) *string { return &s }

func docToMap(v interface{}) map[string]interface{} {
	switch m := v.(type) {
	case map[string]interface{}:
		return m
	default:
		return map[string]interface{}{}
	}
}
