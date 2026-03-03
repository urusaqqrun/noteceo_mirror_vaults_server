package sync

import (
	"context"
	"testing"
	"time"
)

// mockUSNQuerier 模擬 MongoDB USN 查詢
type mockUSNQuerier struct {
	latestUSN map[string]int
	changes   map[string][]SyncEvent
}

func newMockQuerier() *mockUSNQuerier {
	return &mockUSNQuerier{
		latestUSN: make(map[string]int),
		changes:   make(map[string][]SyncEvent),
	}
}

func (q *mockUSNQuerier) GetLatestUSN(_ context.Context, userId string) (int, error) {
	return q.latestUSN[userId], nil
}

func (q *mockUSNQuerier) GetChangesAfterUSN(_ context.Context, userId string, afterUSN int) ([]SyncEvent, error) {
	var result []SyncEvent
	for _, e := range q.changes[userId] {
		result = append(result, e)
	}
	return result, nil
}

func TestUSNPoller_NoChanges(t *testing.T) {
	querier := newMockQuerier()
	querier.latestUSN["user1"] = 5

	handler := &mockHandler{}
	poller := NewUSNPoller(querier, handler, time.Second, func() []string { return []string{"user1"} })
	poller.SetLastUSN("user1", 5)

	n := poller.PollUser(context.Background(), "user1")
	if n != 0 {
		t.Errorf("got %d processed, want 0", n)
	}
	if len(handler.events) != 0 {
		t.Errorf("got %d events, want 0", len(handler.events))
	}
}

func TestUSNPoller_WithChanges(t *testing.T) {
	querier := newMockQuerier()
	querier.latestUSN["user1"] = 8
	querier.changes["user1"] = []SyncEvent{
		{Collection: "note", UserID: "user1", DocID: "n1", Action: "create"},
		{Collection: "note", UserID: "user1", DocID: "n2", Action: "update"},
	}

	handler := &mockHandler{}
	poller := NewUSNPoller(querier, handler, time.Second, func() []string { return []string{"user1"} })
	poller.SetLastUSN("user1", 5)

	n := poller.PollUser(context.Background(), "user1")
	if n != 2 {
		t.Errorf("got %d processed, want 2", n)
	}
	if len(handler.events) != 2 {
		t.Errorf("got %d handler events, want 2", len(handler.events))
	}
	if poller.GetLastUSN("user1") != 8 {
		t.Errorf("lastUSN: got %d, want 8", poller.GetLastUSN("user1"))
	}
}

func TestUSNPoller_SetAndGetLastUSN(t *testing.T) {
	querier := newMockQuerier()
	handler := &mockHandler{}
	poller := NewUSNPoller(querier, handler, time.Second, nil)

	poller.SetLastUSN("user1", 42)
	if got := poller.GetLastUSN("user1"); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	if got := poller.GetLastUSN("user2"); got != 0 {
		t.Errorf("got %d, want 0 for unknown user", got)
	}
}
