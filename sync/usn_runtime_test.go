package sync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type runtimeMockQuerier struct {
	mu        sync.Mutex
	latest    map[string]int
	changes   map[string][]SyncEvent
	failUsers map[string]bool
	calls     map[string]int
}

func (q *runtimeMockQuerier) GetLatestUSN(_ context.Context, userId string) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.latest[userId], nil
}

func (q *runtimeMockQuerier) GetChangesAfterUSN(_ context.Context, userId string, afterUSN int) ([]SyncEvent, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls[userId]++
	if q.failUsers[userId] {
		return nil, errors.New("mock failure")
	}
	var out []SyncEvent
	for _, ev := range q.changes[userId] {
		if int(ev.Timestamp) > afterUSN {
			out = append(out, ev)
		}
	}
	return out, nil
}

type runtimeMockHandler struct {
	mu     sync.Mutex
	events []SyncEvent
}

func (h *runtimeMockHandler) HandleEvent(_ context.Context, event SyncEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
	return nil
}

func TestUSNPollerRuntime_StartPollsActiveUsers(t *testing.T) {
	querier := &runtimeMockQuerier{
		latest: map[string]int{"u1": 3, "u2": 2},
		changes: map[string][]SyncEvent{
			"u1": {{Collection: "note", UserID: "u1", DocID: "n1", Action: "update", Timestamp: 3}},
			"u2": {{Collection: "folder", UserID: "u2", DocID: "f1", Action: "update", Timestamp: 2}},
		},
		failUsers: map[string]bool{},
		calls:     map[string]int{},
	}
	handler := &runtimeMockHandler{}
	poller := NewUSNPoller(querier, handler, 10*time.Millisecond, func() []string { return []string{"u1", "u2"} })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	poller.Start(ctx)

	if querier.calls["u1"] == 0 || querier.calls["u2"] == 0 {
		t.Fatalf("expected both users to be polled, got calls=%v", querier.calls)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.events) == 0 {
		t.Fatal("expected events to be handled")
	}
}

func TestUSNPollerRuntime_FailureIsolation(t *testing.T) {
	querier := &runtimeMockQuerier{
		latest: map[string]int{"u1": 1, "u2": 2},
		changes: map[string][]SyncEvent{
			"u2": {{Collection: "note", UserID: "u2", DocID: "n2", Action: "update", Timestamp: 2}},
		},
		failUsers: map[string]bool{"u1": true},
		calls:     map[string]int{},
	}
	handler := &runtimeMockHandler{}
	poller := NewUSNPoller(querier, handler, 10*time.Millisecond, func() []string { return []string{"u1", "u2"} })

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	poller.Start(ctx)

	if querier.calls["u1"] == 0 || querier.calls["u2"] == 0 {
		t.Fatalf("expected both users attempted, got calls=%v", querier.calls)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.events) == 0 {
		t.Fatal("u1 failure should not block u2 handling")
	}
}
