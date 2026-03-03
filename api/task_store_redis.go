package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisTaskStore struct {
	rdb *redis.Client
}

func NewRedisTaskStore(rdb *redis.Client) *RedisTaskStore {
	return &RedisTaskStore{rdb: rdb}
}

func (s *RedisTaskStore) taskKey(id string) string       { return "vault:task:" + id }
func (s *RedisTaskStore) activeKey(userID string) string { return "vault:task:active:" + userID }

func (s *RedisTaskStore) NextTaskID(ctx context.Context) (string, error) {
	n, err := s.rdb.Incr(ctx, "vault:task:counter").Result()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("task-%d", n), nil
}

func (s *RedisTaskStore) CreateTask(ctx context.Context, task *Task) error {
	return s.saveTask(ctx, task)
}

func (s *RedisTaskStore) UpdateTask(ctx context.Context, task *Task) error {
	return s.saveTask(ctx, task)
}

func (s *RedisTaskStore) saveTask(ctx context.Context, task *Task) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if terminalTask(task.Status) {
		return s.rdb.Set(ctx, s.taskKey(task.ID), data, defaultTaskTTL()).Err()
	}
	return s.rdb.Set(ctx, s.taskKey(task.ID), data, 0).Err()
}

func (s *RedisTaskStore) GetTask(ctx context.Context, taskID string) (*Task, error) {
	val, err := s.rdb.Get(ctx, s.taskKey(taskID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal([]byte(val), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *RedisTaskStore) TryAcquireUserActiveTask(ctx context.Context, userID, taskID string) (bool, error) {
	ok, err := s.rdb.SetNX(ctx, s.activeKey(userID), taskID, 30*time.Minute).Result()
	return ok, err
}

func (s *RedisTaskStore) ReleaseUserActiveTask(ctx context.Context, userID, taskID string) error {
	lua := redis.NewScript(`
		local v = redis.call("GET", KEYS[1])
		if v == ARGV[1] then
			return redis.call("DEL", KEYS[1])
		end
		return 0
	`)
	return lua.Run(ctx, s.rdb, []string{s.activeKey(userID)}, taskID).Err()
}
