package sync

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	StreamName       = "vault:sync-events"
	ConsumerGroup    = "mirror-service-group"
	MaxPendingRetry  = 10
	PendingBackoffMs = 500
)

// SyncEvent Redis Streams 中的同步事件
type SyncEvent struct {
	Collection string // folder / note / card / chart
	UserID     string
	DocID      string
	Action     string // create / update / delete
	Timestamp  int64
	USN        int // USN poller 用，Redis Streams 事件為 0
}

// EventHandler 事件處理介面（方便 mock）
type EventHandler interface {
	HandleEvent(ctx context.Context, event SyncEvent) error
}

// Consumer Redis Streams consumer
type Consumer struct {
	rdb      *redis.Client
	handler  EventHandler
	consumer string
}

func NewConsumer(rdb *redis.Client, handler EventHandler, consumerName string) *Consumer {
	return &Consumer{rdb: rdb, handler: handler, consumer: consumerName}
}

// EnsureGroup 建立 consumer group（若不存在）
func (c *Consumer) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, StreamName, ConsumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Start 開始消費迴圈（阻塞），使用 ctx 控制生命週期
func (c *Consumer) Start(ctx context.Context) error {
	if err := c.EnsureGroup(ctx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// 先嘗試重處理 pending 訊息，避免失敗事件永遠卡在 PEL。
		if handled, err := c.processPending(ctx); err != nil {
			log.Printf("processPending error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		} else if handled > 0 {
			// 有處理過 pending，先讓出時間片避免忙迴圈。
			time.Sleep(200 * time.Millisecond)
			continue
		}

		results, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    ConsumerGroup,
			Consumer: c.consumer,
			Streams:  []string{StreamName, ">"},
			Block:    5 * time.Second,
			Count:    10,
		}).Result()

		if err != nil {
			if err == redis.Nil || ctx.Err() != nil {
				continue
			}
			log.Printf("XReadGroup error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, stream := range results {
			for _, msg := range stream.Messages {
				event := parseEvent(msg.Values)
				// 驗證必要欄位，缺少則 ACK 後跳過
				if event.UserID == "" || event.DocID == "" {
					log.Printf("parseEvent 缺少必要欄位 (id=%s): userID=%q docID=%q，ACK 並跳過", msg.ID, event.UserID, event.DocID)
					c.rdb.XAck(ctx, StreamName, ConsumerGroup, msg.ID)
					continue
				}
				if err := c.handler.HandleEvent(ctx, event); err != nil {
					if errors.Is(err, ErrVaultLocked) {
						continue
					}
					log.Printf("HandleEvent error (id=%s): %v", msg.ID, err)
					continue
				}
				if err := c.rdb.XAck(ctx, StreamName, ConsumerGroup, msg.ID).Err(); err != nil {
					log.Printf("XAck error (id=%s): %v", msg.ID, err)
				}
			}
		}
	}
}

// processPending 讀取目前 consumer 的 pending 訊息並重試處理。
// 超過 MaxPendingRetry 次的訊息會被 ACK 並丟棄（dead-letter）。
func (c *Consumer) processPending(ctx context.Context) (int, error) {
	pending, err := c.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   StreamName,
		Group:    ConsumerGroup,
		Consumer: c.consumer,
		Start:    "-",
		End:      "+",
		Count:    10,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return 0, nil
		}
		return 0, err
	}

	if len(pending) == 0 {
		return 0, nil
	}

	handled := 0
	for _, p := range pending {
		msgs, err := c.rdb.XRangeN(ctx, StreamName, p.ID, p.ID, 1).Result()
		if err != nil {
			log.Printf("XRangeN error (id=%s): %v", p.ID, err)
			continue
		}
		if len(msgs) == 0 {
			// 訊息已被 MAXLEN 裁剪，直接 ACK 清除 PEL
			if err := c.rdb.XAck(ctx, StreamName, ConsumerGroup, p.ID).Err(); err != nil {
				log.Printf("XAck trimmed msg error (id=%s): %v", p.ID, err)
			}
			handled++
			continue
		}

		event := parseEvent(msgs[0].Values)
		if err := c.handler.HandleEvent(ctx, event); err != nil {
			if errors.Is(err, ErrVaultLocked) {
				continue
			}
			// 非鎖定錯誤：若超過重試上限，dead-letter；否則用 XClaim 讓 RetryCount 增加。
			if p.RetryCount >= MaxPendingRetry {
				log.Printf("[Dead-letter] 訊息 %s 重試 %d 次仍失敗，ACK 並丟棄", p.ID, p.RetryCount)
				if ackErr := c.rdb.XAck(ctx, StreamName, ConsumerGroup, p.ID).Err(); ackErr != nil {
					log.Printf("[Dead-letter] XAck error (id=%s): %v", p.ID, ackErr)
				}
				handled++
				continue
			}
			if _, claimErr := c.rdb.XClaim(ctx, &redis.XClaimArgs{
				Stream:   StreamName,
				Group:    ConsumerGroup,
				Consumer: c.consumer,
				MinIdle:  0,
				Messages: []string{p.ID},
			}).Result(); claimErr != nil {
				log.Printf("XClaim error (id=%s): %v", p.ID, claimErr)
			}
			log.Printf("Handle pending event error (id=%s, retry=%d): %v", p.ID, p.RetryCount, err)
			time.Sleep(time.Duration(PendingBackoffMs*(p.RetryCount+1)) * time.Millisecond)
			continue
		}
		if err := c.rdb.XAck(ctx, StreamName, ConsumerGroup, p.ID).Err(); err != nil {
			log.Printf("XAck pending event error (id=%s): %v", p.ID, err)
			continue
		}
		handled++
	}
	return handled, nil
}

// ConsumeOnce 消費一批事件（用於測試）
func (c *Consumer) ConsumeOnce(ctx context.Context) ([]SyncEvent, error) {
	results, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    ConsumerGroup,
		Consumer: c.consumer,
		Streams:  []string{StreamName, ">"},
		Block:    100 * time.Millisecond,
		Count:    10,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}

	var events []SyncEvent
	for _, stream := range results {
		for _, msg := range stream.Messages {
			event := parseEvent(msg.Values)
			events = append(events, event)
			c.rdb.XAck(ctx, StreamName, ConsumerGroup, msg.ID)
		}
	}
	return events, nil
}

func parseEvent(values map[string]interface{}) SyncEvent {
	return SyncEvent{
		Collection: strVal(values["collection"]),
		UserID:     strVal(values["userId"]),
		DocID:      strVal(values["docId"]),
		Action:     strVal(values["action"]),
		Timestamp:  intVal(values["timestamp"]),
	}
}

func strVal(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intVal(v interface{}) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case string:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}
