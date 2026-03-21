package sync

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/urusaqqrun/vault-mirror-service/mirror"
	"github.com/urusaqqrun/vault-mirror-service/model"
)

type mockDataReader struct {
	folders map[string]*model.Folder
	notes   map[string]*model.Note
	cards   map[string]*model.Card
	charts  map[string]*model.Chart
}

func (m *mockDataReader) ListFolders(_ context.Context, _ string) ([]*model.Folder, error) {
	out := make([]*model.Folder, 0, len(m.folders))
	for _, f := range m.folders {
		out = append(out, f)
	}
	return out, nil
}
func (m *mockDataReader) GetFolder(_ context.Context, _ string, id string) (*model.Folder, error) {
	return m.folders[id], nil
}
func (m *mockDataReader) GetNote(_ context.Context, _ string, id string) (*model.Note, error) {
	return m.notes[id], nil
}
func (m *mockDataReader) GetCard(_ context.Context, _ string, id string) (*model.Card, error) {
	return m.cards[id], nil
}
func (m *mockDataReader) GetChart(_ context.Context, _ string, id string) (*model.Chart, error) {
	return m.charts[id], nil
}
func (m *mockDataReader) GetItem(_ context.Context, _ string, _ string) (*model.Item, error) {
	return nil, nil
}
func (m *mockDataReader) ListItemFolders(_ context.Context, _ string) ([]*model.Item, error) {
	items := make([]*model.Item, 0, len(m.folders))
	for _, f := range m.folders {
		ft := f.GetType()
		parentID := ""
		if f.ParentID != nil {
			parentID = *f.ParentID
		}
		items = append(items, &model.Item{
			ID:   f.ID,
			Type: "FOLDER",
			Fields: map[string]interface{}{
				"name":       f.FolderName,
				"folderType": ft,
				"parentID":   parentID,
			},
		})
	}
	return items, nil
}

func ptr(s string) *string { return &s }

func TestEventPipeline_NoteCreate_ExportsMarkdown(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	noteType := "NOTE"
	title := "新筆記"
	content := "<p>Hello</p>"

	reader := &mockDataReader{
		folders: map[string]*model.Folder{
			"f1": {ID: "f1", FolderName: "工作", Type: &noteType},
		},
		notes: map[string]*model.Note{
			"n1": {ID: "n1", Title: &title, Content: &content, FolderID: "f1", Usn: 3, CreateAt: 1, UpdateAt: 2},
		},
		cards:  map[string]*model.Card{},
		charts: map[string]*model.Chart{},
	}

	h := NewSyncEventHandler(fs, reader)
	err := h.HandleEvent(context.Background(), SyncEvent{Collection: "note", UserID: "u1", DocID: "n1", Action: "create"})
	if err != nil {
		t.Fatal(err)
	}

	if !fs.Exists("u1/NOTE/工作/新筆記.md") {
		t.Fatal("expected markdown to be exported")
	}
	data, _ := fs.ReadFile("u1/NOTE/工作/新筆記.md")
	if string(data) == "" {
		t.Fatal("markdown should not be empty")
	}
}

func TestEventPipeline_FolderUpdate_ExportsFolderJSON(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	noteType := "NOTE"
	reader := &mockDataReader{
		folders: map[string]*model.Folder{
			"f1": {ID: "f1", FolderName: "工作", Type: &noteType, Usn: 2},
		},
		notes: map[string]*model.Note{},
		cards: map[string]*model.Card{}, charts: map[string]*model.Chart{},
	}

	h := NewSyncEventHandler(fs, reader)
	err := h.HandleEvent(context.Background(), SyncEvent{Collection: "folder", UserID: "u1", DocID: "f1", Action: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("u1/NOTE/工作/_folder.json") {
		t.Fatal("expected _folder.json to be exported")
	}
}

func TestEventPipeline_NoteDelete_RemovesFileByDocID(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	md := "---\nid: n1\nparentID: f1\ntitle: test\nusn: 1\nhtmlHash: h\ncreatedAt: \"1\"\nupdatedAt: \"1\"\n---\ncontent"
	_ = fs.WriteFile("u1/NOTE/工作/test.md", []byte(md))

	reader := &mockDataReader{folders: map[string]*model.Folder{}, notes: map[string]*model.Note{}, cards: map[string]*model.Card{}, charts: map[string]*model.Chart{}}
	h := NewSyncEventHandler(fs, reader)
	err := h.HandleEvent(context.Background(), SyncEvent{Collection: "note", UserID: "u1", DocID: "n1", Action: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if fs.Exists("u1/NOTE/工作/test.md") {
		t.Fatal("expected deleted note file to be removed")
	}
}

// testVaultLocker 測試用 VaultLocker 實作
type testVaultLocker struct {
	locked map[string]bool
}

func (l *testVaultLocker) IsLocked(userId string) bool {
	return l.locked[userId]
}

func TestHandleEvent_VaultLocked_ReturnsError(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	reader := &mockDataReader{
		folders: map[string]*model.Folder{},
		notes:   map[string]*model.Note{},
		cards:   map[string]*model.Card{},
		charts:  map[string]*model.Chart{},
	}
	h := NewSyncEventHandler(fs, reader)

	locker := &testVaultLocker{locked: map[string]bool{"u1": true}}
	h.SetLocker(locker)

	// 被鎖定的用戶應該收到 ErrVaultLocked
	err := h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u1", DocID: "n1", Action: "create",
	})
	if !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("expected ErrVaultLocked, got %v", err)
	}

	// 未鎖定的用戶應正常通過（即使 reader 找不到資料也不會報 ErrVaultLocked）
	err = h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u2", DocID: "n2", Action: "create",
	})
	if errors.Is(err, ErrVaultLocked) {
		t.Fatal("u2 不該被鎖定")
	}
}

func TestHandleEvent_VaultUnlocked_ProcessesNormally(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	noteType := "NOTE"
	title := "test"
	content := "<p>ok</p>"
	reader := &mockDataReader{
		folders: map[string]*model.Folder{
			"f1": {ID: "f1", FolderName: "Work", Type: &noteType},
		},
		notes: map[string]*model.Note{
			"n1": {ID: "n1", Title: &title, Content: &content, FolderID: "f1", Usn: 1, CreateAt: 1, UpdateAt: 2},
		},
		cards: map[string]*model.Card{}, charts: map[string]*model.Chart{},
	}
	h := NewSyncEventHandler(fs, reader)

	// 先鎖定再解鎖
	locker := &testVaultLocker{locked: map[string]bool{"u1": true}}
	h.SetLocker(locker)

	err := h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u1", DocID: "n1", Action: "create",
	})
	if !errors.Is(err, ErrVaultLocked) {
		t.Fatal("should be locked")
	}

	// 解鎖後應正常處理
	locker.locked["u1"] = false
	err = h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u1", DocID: "n1", Action: "create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("u1/NOTE/Work/test.md") {
		t.Fatal("note should be exported after unlock")
	}
}

// countingDataReader 記錄 ListItemFolders 呼叫次數
type countingDataReader struct {
	mockDataReader
	listFoldersCalls int32
}

func (m *countingDataReader) ListItemFolders(ctx context.Context, userID string) ([]*model.Item, error) {
	atomic.AddInt32(&m.listFoldersCalls, 1)
	return m.mockDataReader.ListItemFolders(ctx, userID)
}

func TestResolverCache_ReducesListFoldersCalls(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	noteType := "NOTE"
	title1 := "note1"
	title2 := "note2"
	content := "<p>x</p>"

	reader := &countingDataReader{
		mockDataReader: mockDataReader{
			folders: map[string]*model.Folder{
				"f1": {ID: "f1", FolderName: "Work", Type: &noteType},
			},
			notes: map[string]*model.Note{
				"n1": {ID: "n1", Title: &title1, Content: &content, FolderID: "f1", Usn: 1, CreateAt: 1, UpdateAt: 2},
				"n2": {ID: "n2", Title: &title2, Content: &content, FolderID: "f1", Usn: 2, CreateAt: 1, UpdateAt: 2},
			},
			cards: map[string]*model.Card{}, charts: map[string]*model.Chart{},
		},
	}

	h := NewSyncEventHandler(fs, reader)

	// 同一用戶連續 5 個 note 事件，應只查 1 次 ListItemFolders（快取命中）
	for i := 0; i < 5; i++ {
		docID := "n1"
		if i%2 == 1 {
			docID = "n2"
		}
		h.HandleEvent(context.Background(), SyncEvent{
			Collection: "note", UserID: "u1", DocID: docID, Action: "update",
		})
	}

	calls := atomic.LoadInt32(&reader.listFoldersCalls)
	if calls > 1 {
		t.Errorf("expected ListItemFolders called <= 1 time (cached), got %d", calls)
	}
	t.Logf("ListItemFolders calls for 5 events: %d", calls)
}

func TestResolverCache_InvalidatedOnFolderEvent(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	noteType := "NOTE"
	title := "n"
	content := "<p>x</p>"

	reader := &countingDataReader{
		mockDataReader: mockDataReader{
			folders: map[string]*model.Folder{
				"f1": {ID: "f1", FolderName: "Work", Type: &noteType},
			},
			notes: map[string]*model.Note{
				"n1": {ID: "n1", Title: &title, Content: &content, FolderID: "f1", Usn: 1, CreateAt: 1, UpdateAt: 2},
			},
			cards: map[string]*model.Card{}, charts: map[string]*model.Chart{},
		},
	}

	h := NewSyncEventHandler(fs, reader)

	// 第一次 note 事件：cache miss → 查 1 次 ListItemFolders
	h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u1", DocID: "n1", Action: "update",
	})

	// folder 事件 → invalidate cache
	h.HandleEvent(context.Background(), SyncEvent{
		Collection: "folder", UserID: "u1", DocID: "f1", Action: "update",
	})

	// 第二次 note 事件：cache 已 invalidated → 再查 1 次 ListItemFolders
	h.HandleEvent(context.Background(), SyncEvent{
		Collection: "note", UserID: "u1", DocID: "n1", Action: "update",
	})

	calls := atomic.LoadInt32(&reader.listFoldersCalls)
	// 第 1 次 note = 1（cache miss），folder update invalidates + rebuilds = 1，
	// 第 2 次 note 命中 folder 事件重建的快取 → 共 2 次
	if calls != 2 {
		t.Errorf("expected ListItemFolders == 2 (cache invalidated then rebuilt by folder event), got %d", calls)
	}
	t.Logf("ListItemFolders calls after folder invalidation: %d", calls)
}

func TestEventPipeline_CardCreate_ExportsCardJSON(t *testing.T) {
	fs := mirror.NewMemoryVaultFS()
	cardType := "CARD"
	fields := `{"name":"A"}`
	reader := &mockDataReader{
		folders: map[string]*model.Folder{
			"c1": {ID: "c1", FolderName: "卡片夾", Type: &cardType},
		},
		notes: map[string]*model.Note{},
		cards: map[string]*model.Card{
			"card1": {ID: "card1", ParentID: "c1", Name: "卡片一", Fields: &fields, Usn: 1},
		},
		charts: map[string]*model.Chart{},
	}

	h := NewSyncEventHandler(fs, reader)
	err := h.HandleEvent(context.Background(), SyncEvent{Collection: "card", UserID: "u1", DocID: "card1", Action: "create"})
	if err != nil {
		t.Fatal(err)
	}
	if !fs.Exists("u1/CARD/卡片夾/卡片一.json") {
		t.Fatal("expected card json to be exported")
	}
}
