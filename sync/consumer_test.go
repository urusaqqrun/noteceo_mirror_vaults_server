package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// mockHandler 記錄收到的事件
type mockHandler struct {
	events []SyncEvent
}

func (h *mockHandler) HandleEvent(_ context.Context, event SyncEvent) error {
	h.events = append(h.events, event)
	return nil
}

type flakyHandler struct {
	calls int
}

func (h *flakyHandler) HandleEvent(_ context.Context, event SyncEvent) error {
	h.calls++
	if h.calls == 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func setupTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return mr, rdb
}

func TestConsumer_EnsureGroup(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")

	err := consumer.EnsureGroup(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 第二次呼叫不應報錯
	err = consumer.EnsureGroup(context.Background())
	if err != nil {
		t.Fatal("second EnsureGroup should not error:", err)
	}
}

func TestConsumer_ConsumeOnce_FolderCreate(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	// 發佈事件
	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "folder",
			"userId":     "user1",
			"docId":      "f1",
			"action":     "create",
			"timestamp":  "1709000000000",
		},
	})

	events, err := consumer.ConsumeOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Collection != "folder" {
		t.Errorf("collection: got %q, want %q", events[0].Collection, "folder")
	}
	if events[0].UserID != "user1" {
		t.Errorf("userId: got %q, want %q", events[0].UserID, "user1")
	}
	if events[0].Action != "create" {
		t.Errorf("action: got %q, want %q", events[0].Action, "create")
	}
}

func TestConsumer_ConsumeOnce_NoteUpdate(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "note",
			"userId":     "user1",
			"docId":      "n1",
			"action":     "update",
			"timestamp":  "1709000000000",
		},
	})

	events, err := consumer.ConsumeOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Collection != "note" || events[0].Action != "update" {
		t.Errorf("unexpected event: %+v", events[0])
	}
}

func TestConsumer_ConsumeOnce_MultipleEvents(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	for _, action := range []string{"create", "update", "delete"} {
		rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: StreamName,
			Values: map[string]interface{}{
				"collection": "note",
				"userId":     "user1",
				"docId":      "n" + action,
				"action":     action,
				"timestamp":  "1709000000000",
			},
		})
	}

	events, err := consumer.ConsumeOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
}

func TestConsumer_ConsumeOnce_NoEvents(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	events, err := consumer.ConsumeOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want 0", len(events))
	}
}

func TestConsumer_AcksAfterProcessing(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &mockHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "folder",
			"userId":     "user1",
			"docId":      "f1",
			"action":     "create",
			"timestamp":  "1709000000000",
		},
	})

	// 消費一次
	consumer.ConsumeOnce(ctx)

	// 再消費一次，不應再收到已 ACK 的事件
	events, _ := consumer.ConsumeOnce(ctx)
	if len(events) != 0 {
		t.Errorf("已 ACK 的事件不應再被消費，got %d events", len(events))
	}
}

func TestConsumer_ProcessPending_RetriesAndAck(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &flakyHandler{}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "note",
			"userId":     "user1",
			"docId":      "n1",
			"action":     "update",
			"timestamp":  "1709000000000",
		},
	})

	// 先把訊息讀進 pending（模擬先前處理失敗未 ACK）
	_, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: "test-1",
		Streams:  []string{StreamName, ">"},
		Count:    1,
		Block:    100 * time.Millisecond,
	}).Result()
	if err != nil {
		t.Fatal(err)
	}

	// 第一次 pending 處理會失敗（flaky 第 1 次）
	handled, err := consumer.processPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if handled != 0 {
		t.Fatalf("first pending handled: got %d, want 0", handled)
	}

	// 第二次 pending 處理應成功並 ACK
	handled, err = consumer.processPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if handled != 1 {
		t.Fatalf("handled pending: got %d, want 1", handled)
	}
}

// alwaysFailHandler 永遠回傳錯誤，用於測試 dead-letter
type alwaysFailHandler struct {
	calls int
	err   error
}

func (h *alwaysFailHandler) HandleEvent(_ context.Context, _ SyncEvent) error {
	h.calls++
	return h.err
}

func TestProcessPending_XClaim_DeliveryCounterIncrements(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &alwaysFailHandler{err: errors.New("always fail")}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "note",
			"userId":     "user1",
			"docId":      "n1",
			"action":     "update",
			"timestamp":  "1709000000000",
		},
	})

	// 初次讀取讓訊息進入 PEL（delivery count = 1）
	rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: "test-1",
		Streams:  []string{StreamName, ">"},
		Count:    1,
		Block:    100 * time.Millisecond,
	})

	// 多次呼叫 processPending，XClaim 應遞增 delivery counter
	for i := 0; i < 3; i++ {
		consumer.processPending(ctx)
	}

	// 檢查 delivery count 確實有增加（XPendingExt 回傳 RetryCount）
	pending, err := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   StreamName,
		Group:    ConsumerGroup,
		Consumer: "test-1",
		Start:    "-",
		End:      "+",
		Count:    10,
	}).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	// 初始讀取 = 1，再加 3 次 XClaim = 4
	if pending[0].RetryCount < 2 {
		t.Errorf("expected RetryCount >= 2 after XClaim, got %d", pending[0].RetryCount)
	}
	t.Logf("delivery counter after 3 XClaim calls: %d", pending[0].RetryCount)
}

func TestProcessPending_DeadLetterAfterMaxRetry(t *testing.T) {
	_, rdb := setupTestRedis(t)
	defer rdb.Close()

	handler := &alwaysFailHandler{err: errors.New("permanent failure")}
	consumer := NewConsumer(rdb, handler, "test-1")
	ctx := context.Background()
	consumer.EnsureGroup(ctx)

	rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamName,
		Values: map[string]interface{}{
			"collection": "note",
			"userId":     "user1",
			"docId":      "n1",
			"action":     "update",
			"timestamp":  "1709000000000",
		},
	})

	// 初次讀取
	rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: "test-1",
		Streams:  []string{StreamName, ">"},
		Count:    1,
		Block:    100 * time.Millisecond,
	})

	// 持續重試直到超過 MaxPendingRetry
	deadLettered := false
	for i := 0; i < MaxPendingRetry+5; i++ {
		handled, _ := consumer.processPending(ctx)

		// 檢查 PEL 是否已清空（被 dead-letter ACK）
		pend, _ := rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
			Stream:   StreamName,
			Group:    ConsumerGroup,
			Consumer: "test-1",
			Start:    "-",
			End:      "+",
			Count:    10,
		}).Result()
		if len(pend) == 0 && handled > 0 {
			deadLettered = true
			t.Logf("dead-lettered at iteration %d", i)
			break
		}
	}

	if !deadLettered {
		t.Fatal("訊息未被 dead-letter 處理，delivery counter 可能未正確遞增")
	}
}
