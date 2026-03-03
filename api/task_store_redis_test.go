package api

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupRedisStore(t *testing.T) (*RedisTaskStore, *redis.Client) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return NewRedisTaskStore(rdb), rdb
}

func TestRedisTaskStore_CreateGetUpdate(t *testing.T) {
	store, rdb := setupRedisStore(t)
	defer rdb.Close()
	ctx := context.Background()

	id, err := store.NextTaskID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: id, UserID: "u1", Status: TaskStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get task failed: %v", err)
	}
	got.Status = TaskStatusCompleted
	if err := store.UpdateTask(ctx, got); err != nil {
		t.Fatal(err)
	}
	finalTask, _ := store.GetTask(ctx, id)
	if finalTask.Status != TaskStatusCompleted {
		t.Fatalf("status: got %s", finalTask.Status)
	}
}

func TestRedisTaskStore_ActiveTaskLock(t *testing.T) {
	store, rdb := setupRedisStore(t)
	defer rdb.Close()
	ctx := context.Background()

	ok, err := store.TryAcquireUserActiveTask(ctx, "u1", "task-1")
	if err != nil || !ok {
		t.Fatalf("first acquire failed: ok=%v err=%v", ok, err)
	}
	ok, err = store.TryAcquireUserActiveTask(ctx, "u1", "task-2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second acquire should fail")
	}

	if err := store.ReleaseUserActiveTask(ctx, "u1", "task-1"); err != nil {
		t.Fatal(err)
	}
	ok, err = store.TryAcquireUserActiveTask(ctx, "u1", "task-2")
	if err != nil || !ok {
		t.Fatalf("re-acquire failed: ok=%v err=%v", ok, err)
	}
}
